package integration_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/mq"
	"github.com/aws/aws-sdk-go-v2/service/mq/types"
)

func TestMQSDKRabbitMQInstanceConstraints(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := mqClient(s, "test", "us-east-1")
	in := mqInput()
	in.EngineType, in.EngineVersion, in.HostInstanceType = types.EngineTypeRabbitmq, new("3.13"), new("mq.t3.micro")
	in.Logs = &types.Logs{General: new(false)}
	_, err := c.CreateBroker(ctx, in)
	mqStatus(t, err, "BadRequestException", 400)
	listed, err := c.ListBrokers(ctx, &sdk.ListBrokersInput{})
	if err != nil || len(listed.BrokerSummaries) != 0 {
		t.Fatal("rejected SDK creation persisted", err)
	}
	in.HostInstanceType = new("mq.m5.large")
	created, err := c.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal("valid RabbitMQ creation with previously rejected name/token", err)
	}
	before := mqMetadata(mqBroker(t, c, created.BrokerId))
	_, err = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{
		BrokerId: created.BrokerId, HostInstanceType: new("mq.t3.micro"), Logs: &types.Logs{General: new(true)},
	})
	mqStatus(t, err, "BadRequestException", 400)
	if !reflect.DeepEqual(before, mqMetadata(mqBroker(t, c, created.BrokerId))) {
		t.Fatal("rejected SDK downgrade changed exact metadata")
	}
	if _, err = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{
		BrokerId: created.BrokerId, HostInstanceType: new("mq.m5.large"), Logs: &types.Logs{General: new(true)},
	}); err != nil {
		t.Fatal("valid RabbitMQ same-instance update", err)
	}
	after := mqBroker(t, c, created.BrokerId)
	if *after.HostInstanceType != "mq.m5.large" || !*after.Logs.General ||
		!before.Created.Equal(*after.Created) || *before.BrokerArn != *after.BrokerArn {
		t.Fatal("valid RabbitMQ update metadata")
	}
	if _, err = c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: created.BrokerId}); err != nil {
		t.Fatal(err)
	}
}

func rabbitMQCFProperties(name string, tags map[string]string) map[string]any {
	p := mqCFProperties(name, tags)
	p["EngineType"], p["EngineVersion"] = "RABBITMQ", "3.13"
	p["Logs"] = map[string]any{"General": false}
	return p
}

func TestCloudFormationMQRabbitMQInstanceConstraintsAndAtomicTags(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mqClient(s, "test", "us-east-1")
	p := rabbitMQCFProperties("cf-rabbit", map[string]string{"owned": "old"})
	p["HostInstanceType"] = "mq.t3.micro"
	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("mq-rabbit-rejected"), TemplateBody: new(mqTemplate(p, false)),
	}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, "mq-rabbit-rejected", cfntypes.StackStatusRollbackComplete)
	events, err := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new("mq-rabbit-rejected")})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events.StackEvents {
		if event.ResourceStatus == cfntypes.ResourceStatusCreateFailed && event.ResourceStatusReason != nil {
			found = found || strings.Contains(*event.ResourceStatusReason, "unavailable for new RabbitMQ brokers")
		}
	}
	if !found {
		t.Fatal("CF creation did not reach the RabbitMQ service eligibility check")
	}
	listed, err := c.ListBrokers(ctx, &sdk.ListBrokersInput{})
	if err != nil || len(listed.BrokerSummaries) != 0 {
		t.Fatal("rejected CF creation left metadata", err)
	}
	name := "mq-rabbit-valid"
	p["HostInstanceType"] = "mq.m5.large"
	if _, err = cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new(name), TemplateBody: new(mqTemplate(p, false)),
	}); err != nil {
		t.Fatal(err)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	id := efsStackOutputs(st)["Id"]
	before := mqMetadata(mqBroker(t, c, new(id)))
	if *before.HostInstanceType != "mq.m5.large" || before.EngineType != types.EngineTypeRabbitmq ||
		*before.EngineVersion != "3.13" || *before.Logs.General || before.Logs.Audit != nil ||
		efsStackOutputs(st)["Arn"] != *before.BrokerArn {
		t.Fatal("valid CF RabbitMQ configuration/Ref/GetAtt")
	}
	if _, err = c.CreateTags(ctx, &sdk.CreateTagsInput{
		ResourceArn: before.BrokerArn, Tags: map[string]string{"external": "actual"},
	}); err != nil {
		t.Fatal(err)
	}
	actual := mqMetadata(mqBroker(t, c, new(id)))
	next := rabbitMQCFProperties("cf-rabbit", map[string]string{"replacement": "rejected"})
	next["HostInstanceType"] = "mq.t3.micro"
	next["Logs"] = map[string]any{"General": true}
	updateMQStack(t, cf, name, next, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(actual, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("rejected CF downgrade changed config, owned or external tags")
	}
	next["HostInstanceType"] = "mq.m5.large"
	updateMQStack(t, cf, name, next, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(actual, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("RabbitMQ later-resource failure did not restore actual config/tags")
	}
	st = updateMQStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	after := mqBroker(t, c, new(id))
	if efsStackOutputs(st)["Id"] != id || *after.HostInstanceType != "mq.m5.large" || !*after.Logs.General ||
		len(after.Tags) != 2 || after.Tags["external"] != "actual" || after.Tags["replacement"] != "rejected" ||
		!before.Created.Equal(*after.Created) || *before.BrokerArn != *after.BrokerArn {
		t.Fatal("valid CF RabbitMQ update, owned tag replacement and external retention")
	}
	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(name)}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, err = c.DescribeBroker(ctx, &sdk.DescribeBrokerInput{BrokerId: new(id)})
	mqStatus(t, err, "NotFoundException", 404)
}

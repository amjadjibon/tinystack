package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/mq"
	"github.com/aws/aws-sdk-go-v2/service/mq/types"
)

func mqCFProperties(name string, tags map[string]string) map[string]any {
	list := []any{}
	for k, v := range tags {
		list = append(list, map[string]any{"Key": k, "Value": v})
	}
	return map[string]any{
		"BrokerName":                 name,
		"EngineType":                 "ACTIVEMQ",
		"EngineVersion":              "5.18",
		"HostInstanceType":           "mq.m5.large",
		"DeploymentMode":             "SINGLE_INSTANCE",
		"PubliclyAccessible":         true,
		"AutoMinorVersionUpgrade":    true,
		"Logs":                       map[string]any{"General": true, "Audit": false},
		"MaintenanceWindowStartTime": map[string]any{"DayOfWeek": "FRIDAY", "TimeOfDay": "12:34", "TimeZone": "UTC"},
		"Users": []any{
			map[string]any{"Username": "admin", "Password": "sample-valid-password"},
		},
		"Tags": list,
	}
}

func mqTemplate(p map[string]any, later bool) string {
	resources := map[string]any{"Broker": map[string]any{"Type": "AWS::AmazonMQ::Broker", "Properties": p}}
	if later {
		resources["Later"] = map[string]any{
			"Type": "AWS::AmazonMQ::Broker", "DependsOn": "Broker",
			"Properties": map[string]any{"BrokerName": "invalid-later"},
		}
	}
	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": map[string]any{
		"Id":  map[string]any{"Value": map[string]any{"Ref": "Broker"}},
		"Arn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Broker", "Arn"}}},
	}})
	return string(raw)
}

func updateMQStack(
	t *testing.T,
	cf *cfn.Client,
	name string,
	p map[string]any,
	later bool,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	if _, e := cf.UpdateStack(
		context.Background(),
		&cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(mqTemplate(p, later))},
	); e != nil {
		t.Fatal(e)
	}
	return wantStatus(context.Background(), t, cf, name, status)
}

func TestCloudFormationMQLifecycleDefaultsLiveRollbackReplacementAndCollision(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mqClient(s, "test", "us-east-1")
	name := "mq-stack"
	p := mqCFProperties("cf-broker", map[string]string{"owned": "old"})
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(name), TemplateBody: new(mqTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	outputs := efsStackOutputs(st)
	id := outputs["Id"]
	before := mqMetadata(mqBroker(t, c, new(id)))
	if outputs["Arn"] != *before.BrokerArn || *before.BrokerName != "cf-broker" || *before.Logs.Audit {
		t.Fatal("CF Ref/GetAtt/read")
	}
	retrieved, retrieveErr := cf.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: new(name)})
	if retrieveErr != nil || strings.Contains(*retrieved.TemplateBody, "sample-valid-password") {
		t.Fatal("GetTemplate leaked write-only literal credentials", retrieveErr)
	}
	_, records := inspect(t, s, "GET", "/_tinystack/requests")
	for _, record := range records {
		if strings.Contains(record.RequestBody, "sample-valid-password") ||
			strings.Contains(record.ResponseBody, "sample-valid-password") {
			t.Fatal("CloudFormation diagnostics leaked credentials")
		}
	}
	events, e := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	for _, event := range events.StackEvents {
		if event.ResourceProperties != nil && strings.Contains(*event.ResourceProperties, "sample-valid-password") {
			t.Fatal("event credential disclosure")
		}
	}
	// Omitted mutable optional properties clear to the explicitly documented local defaults.
	delete(p, "Logs")
	delete(p, "MaintenanceWindowStartTime")
	delete(p, "AutoMinorVersionUpgrade")
	delete(p, "Tags")
	updateMQStack(t, cf, name, p, false, cfntypes.StackStatusUpdateComplete)
	cleared := mqMetadata(mqBroker(t, c, new(id)))
	if *cleared.Logs.General || *cleared.Logs.Audit ||
		cleared.MaintenanceWindowStartTime.DayOfWeek != types.DayOfWeekMonday ||
		*cleared.MaintenanceWindowStartTime.TimeOfDay != "00:00" ||
		!*cleared.AutoMinorVersionUpgrade ||
		len(cleared.Tags) != 0 ||
		!cleared.Created.Equal(*before.Created) {
		t.Fatal("optional-property defaults")
	}
	// External live configuration differs from the last accepted template. Later
	// failure must restore this actual state, including the prior engine version.
	if _, e = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{
		BrokerId: new(id), HostInstanceType: new("mq.t3.micro"),
		Logs: &types.Logs{General: new(true), Audit: new(true)}, MaintenanceWindowStartTime: &types.WeeklyStartTime{
			DayOfWeek: types.DayOfWeekTuesday, TimeOfDay: new("23:45"), TimeZone: new("UTC"),
		},
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.CreateTags(
		ctx,
		&sdk.CreateTagsInput{ResourceArn: before.BrokerArn, Tags: map[string]string{"external": "actual"}},
	); e != nil {
		t.Fatal(e)
	}
	actual := mqMetadata(mqBroker(t, c, new(id)))
	next := mqCFProperties("cf-broker", map[string]string{"owned": "new"})
	next["EngineVersion"] = "5.19"
	updateMQStack(t, cf, name, next, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(actual, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("later failure restored template rather than actual live metadata")
	}
	// Retry the real mutable update, retaining unrelated tags.
	st = updateMQStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	if efsStackOutputs(st)["Id"] != id {
		t.Fatal("mutable update replaced ID")
	}
	updated := mqMetadata(mqBroker(t, c, new(id)))
	if *updated.EngineVersion != "5.19" || updated.Tags["external"] != "actual" || updated.Tags["owned"] != "new" {
		t.Fatal("configuration/owned tags update")
	}
	// A later failure after named replacement reuses the original incarnation.
	replacement := mqCFProperties("cf-replacement", map[string]string{"owned": "replacement"})
	updateMQStack(t, cf, name, replacement, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(updated, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("replacement failure lost original metadata")
	}
	list, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{})
	if e != nil || len(list.BrokerSummaries) != 1 {
		t.Fatal("failed replacement leaked broker")
	}
	// A collision preserves the original and an unrelated existing broker.
	collision := mqInput()
	collision.BrokerName = new("cf-collision")
	collision.CreatorRequestId = new("cf-collision-token")
	unrelated, e := c.CreateBroker(ctx, collision)
	if e != nil {
		t.Fatal(e)
	}
	unrelatedBefore := mqMetadata(mqBroker(t, c, unrelated.BrokerId))
	replacement["BrokerName"] = "cf-collision"
	updateMQStack(t, cf, name, replacement, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(updated, mqMetadata(mqBroker(t, c, new(id)))) ||
		!reflect.DeepEqual(unrelatedBefore, mqMetadata(mqBroker(t, c, unrelated.BrokerId))) {
		t.Fatal("collision damaged brokers")
	}
	// Replacement under the existing name must fail before removing the original.
	replacement["BrokerName"] = "cf-broker"
	replacement["DeploymentMode"] = "ACTIVE_STANDBY_MULTI_AZ"
	updateMQStack(t, cf, name, replacement, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(updated, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("same-name replacement deleted original")
	}
	replacement["BrokerName"] = "cf-replacement"
	replacement["DeploymentMode"] = "SINGLE_INSTANCE"
	st = updateMQStack(t, cf, name, replacement, false, cfntypes.StackStatusUpdateComplete)
	newID := efsStackOutputs(st)["Id"]
	if newID == id {
		t.Fatal("replacement kept old ID")
	}
	_, e = c.DescribeBroker(ctx, &sdk.DescribeBrokerInput{BrokerId: new(id)})
	mqStatus(t, e, "NotFoundException", 404)
	if _, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(name)}); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, e = c.DescribeBroker(ctx, &sdk.DescribeBrokerInput{BrokerId: new(newID)})
	mqStatus(t, e, "NotFoundException", 404)
	if !reflect.DeepEqual(unrelatedBefore, mqMetadata(mqBroker(t, c, unrelated.BrokerId))) {
		t.Fatal("stack delete removed external broker")
	}
}

func TestCloudFormationMQRequiredRemovalUsersAndTagBoundaries(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mqClient(s, "test", "us-east-1")
	name := "mq-boundaries"
	tags := map[string]string{}
	for i := range 50 {
		tags[fmt.Sprintf("old%02d", i)] = ""
	}
	p := mqCFProperties("cf-boundaries", tags)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(name), TemplateBody: new(mqTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	id := efsStackOutputs(st)["Id"]
	initial := mqMetadata(mqBroker(t, c, new(id)))
	for _, prop := range []string{
		"BrokerName", "EngineType", "EngineVersion", "HostInstanceType", "DeploymentMode", "PubliclyAccessible", "Users",
	} {
		next := mqCFProperties("cf-boundaries", tags)
		delete(next, prop)
		updateMQStack(t, cf, name, next, false, cfntypes.StackStatusUpdateRollbackComplete)
		if !reflect.DeepEqual(initial, mqMetadata(mqBroker(t, c, new(id)))) {
			t.Fatal("required property removal changed broker", prop)
		}
	}
	changed := mqCFProperties("cf-boundaries", tags)
	changed["Users"].([]any)[0].(map[string]any)["Password"] = "different-password"
	updateMQStack(t, cf, name, changed, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(initial, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("write-only mutation persisted")
	}
	duplicate := mqCFProperties("cf-boundaries", map[string]string{})
	duplicate["Tags"] = []any{
		map[string]any{"Key": "dup", "Value": "one"},
		map[string]any{"Key": "dup", "Value": "two"},
	}
	updateMQStack(t, cf, name, duplicate, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(initial, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("duplicate tags changed broker")
	}
	replacementTags := map[string]string{}
	for i := range 50 {
		replacementTags[fmt.Sprintf("new%02d", i)] = ""
	}
	next := mqCFProperties("cf-boundaries", replacementTags)
	next["Logs"] = map[string]any{"General": false, "Audit": false}
	updateMQStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	b := mqMetadata(mqBroker(t, c, new(id)))
	if len(b.Tags) != 50 || *b.Logs.General {
		t.Fatal("disjoint 50-tag replacement failed")
	}
	// Free one owned key and add a live external key; then expanding back to 50
	// template keys overflows. The config and tag changes must both be rejected.
	delete(replacementTags, "new49")
	next = mqCFProperties("cf-boundaries", replacementTags)
	updateMQStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	if _, e := c.CreateTags(
		ctx,
		&sdk.CreateTagsInput{ResourceArn: b.BrokerArn, Tags: map[string]string{"external": "must-stay"}},
	); e != nil {
		t.Fatal(e)
	}
	before := mqMetadata(mqBroker(t, c, new(id)))
	replacementTags["new49"] = ""
	overflow := mqCFProperties("cf-boundaries", replacementTags)
	overflow["Logs"] = map[string]any{"General": false, "Audit": true}
	updateMQStack(t, cf, name, overflow, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(before, mqMetadata(mqBroker(t, c, new(id)))) {
		t.Fatal("overflow partially changed config/tags")
	}
	// Clearing all owned keys retains the external key.
	clearedProps := mqCFProperties("cf-boundaries", nil)
	delete(clearedProps, "Tags")
	updateMQStack(t, cf, name, clearedProps, false, cfntypes.StackStatusUpdateComplete)
	after := mqBroker(t, c, new(id))
	if len(after.Tags) != 1 || after.Tags["external"] != "must-stay" {
		t.Fatal("owned tag removal removed external key")
	}
	encryption := mqCFProperties("cf-boundaries-encrypted", nil)
	encryption["EncryptionOptions"] = map[string]any{"UseAwsOwnedKey": true}
	st = updateMQStack(t, cf, name, encryption, false, cfntypes.StackStatusUpdateComplete)
	encryptedID := efsStackOutputs(st)["Id"]
	encrypted := mqMetadata(mqBroker(t, c, new(encryptedID)))
	delete(encryption, "EncryptionOptions")
	updateMQStack(t, cf, name, encryption, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(encrypted, mqMetadata(mqBroker(t, c, new(encryptedID)))) {
		t.Fatal("removed encryption option changed broker")
	}
}

func TestCloudFormationMQCreateRollbackAndUnavailableAttributes(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mqClient(s, "test", "us-east-1")
	p := mqCFProperties("cf-rollback", nil)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("mq-create-rollback"), TemplateBody: new(mqTemplate(p, true))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "mq-create-rollback", cfntypes.StackStatusRollbackComplete)
	list, e := c.ListBrokers(ctx, &sdk.ListBrokersInput{})
	if e != nil || len(list.BrokerSummaries) != 0 {
		t.Fatal("create rollback retained broker")
	}
	for _, attr := range []string{
		"AmqpEndpoints", "ConsoleURLs", "ConfigurationId", "MqttEndpoints", "IpAddresses", "EngineVersionCurrent",
	} {
		var template map[string]any
		json.Unmarshal([]byte(mqTemplate(p, false)), &template)
		template["Outputs"].(map[string]any)["Arn"] = map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []any{"Broker", attr}},
		}
		raw, _ := json.Marshal(template)
		_, e = cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{StackName: new("mq-unsupported-attribute"), TemplateBody: new(string(raw))},
		)
		code(t, e, "ValidationError")
	}
}

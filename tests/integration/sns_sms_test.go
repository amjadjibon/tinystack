package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
)

func TestSNSSMS(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	ctx := context.Background()
	if _, err := c.Publish(ctx, &sns.PublishInput{
		PhoneNumber: aws.String("+15555550100"), Message: aws.String("code 1234"),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"AWS.SNS.SMS.SMSType": {DataType: aws.String("String"), StringValue: aws.String("Transactional")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	records := deliveries(t, s, 1)
	if records[0].Target != "+15555550100" || records[0].Payload != "code 1234" || records[0].Source != "" {
		t.Fatalf("direct = %+v", records[0])
	}

	_, err := c.Publish(ctx, &sns.PublishInput{PhoneNumber: aws.String("555"), Message: aws.String("x")})
	wantCode(t, err, "InvalidParameter")

	created, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("texts")})
	if err != nil {
		t.Fatal(err)
	}

	subscribed, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: created.TopicArn, Protocol: aws.String("sms"), Endpoint: aws.String("+447700900123"),
	})
	if err != nil || aws.ToString(subscribed.SubscriptionArn) == "pending confirmation" {
		t.Fatalf("subscribed = %+v, %v", subscribed, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: created.TopicArn, MessageStructure: aws.String("json"),
		Message: aws.String(`{"default": "long text", "sms": "short"}`),
	}); err != nil {
		t.Fatal(err)
	}

	records = deliveries(t, s, 2)
	if records[0].Target != "+447700900123" || records[0].Payload != "short" ||
		records[0].Source != aws.ToString(created.TopicArn) {
		t.Fatalf("subscription = %+v", records[0])
	}
}

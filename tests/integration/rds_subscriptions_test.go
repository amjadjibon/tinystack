package integration_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestRDSEventSubscriptions(t *testing.T) {
	s := server(t)
	c, topics, queues := rdsClient(s, "test", "us-east-1"), snsClient(s, "test"), sqsClient(s, "test")
	ctx := t.Context()
	in := &rds.CreateEventSubscriptionInput{
		SubscriptionName: new("my-events"), SnsTopicArn: new("arn:aws:sns:us-east-1:000000000000:missing"),
		SourceType: new("db-instance"), EventCategories: []string{"availability"},
		SourceIds: []string{"selected"}, Tags: []types.Tag{{Key: new("team"), Value: new("databases")}},
	}
	_, err := c.CreateEventSubscription(ctx, in)
	code(t, err, "SNSTopicArnNotFound")

	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("rds-events")})
	if err != nil {
		t.Fatal(err)
	}

	queueURL, queueARN := queueARN(t, queues, "rds-events")
	if _, subscribeErr := topics.Subscribe(ctx, &sns.SubscribeInput{
		Protocol: new("sqs"), Endpoint: new(queueARN), TopicArn: topic.TopicArn,
	}); subscribeErr != nil {
		t.Fatal(subscribeErr)
	}

	var sourceARN *string
	for _, name := range []string{"selected", "other"} {
		db, createErr := c.CreateDBInstance(ctx, rdsInput(name))
		if createErr != nil {
			t.Fatal(createErr)
		}

		if name == "selected" {
			sourceARN = db.DBInstance.DBInstanceArn
		}
	}

	in.SnsTopicArn = topic.TopicArn
	sub, err := c.CreateEventSubscription(ctx, in)
	if err != nil || aws.ToString(sub.EventSubscription.Status) != "active" {
		t.Fatal(sub, err)
	}

	_, err = c.CreateEventSubscription(ctx, in)
	code(t, err, "SubscriptionAlreadyExist")

	added, err := c.AddSourceIdentifierToSubscription(ctx, &rds.AddSourceIdentifierToSubscriptionInput{
		SubscriptionName: in.SubscriptionName, SourceIdentifier: new("other"),
	})
	if err != nil || !slices.Contains(added.EventSubscription.SourceIdsList, "other") {
		t.Fatal(added, err)
	}

	removed, err := c.RemoveSourceIdentifierFromSubscription(ctx, &rds.RemoveSourceIdentifierFromSubscriptionInput{
		SubscriptionName: in.SubscriptionName, SourceIdentifier: new("other"),
	})
	if err != nil || !slices.Equal(removed.EventSubscription.SourceIdsList, []string{"selected"}) {
		t.Fatal(removed, err)
	}

	tags, err := c.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{
		ResourceName: sub.EventSubscription.EventSubscriptionArn,
	})
	if err != nil || len(tags.TagList) != 1 || aws.ToString(tags.TagList[0].Value) != "databases" {
		t.Fatal(tags, err)
	}

	modify := &rds.ModifyEventSubscriptionInput{SubscriptionName: in.SubscriptionName, Enabled: new(false)}
	disabled, err := c.ModifyEventSubscription(ctx, modify)
	if err != nil || aws.ToBool(disabled.EventSubscription.Enabled) {
		t.Fatal(disabled, err)
	}

	if _, rebootErr := c.RebootDBInstance(
		ctx,
		&rds.RebootDBInstanceInput{DBInstanceIdentifier: new("selected")},
	); rebootErr != nil {
		t.Fatal(rebootErr)
	}

	modify.Enabled = new(true)
	if _, err = c.ModifyEventSubscription(ctx, modify); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RebootDBInstance(ctx, &rds.RebootDBInstanceInput{DBInstanceIdentifier: new("other")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: new("selected"), CopyTagsToSnapshot: new(true),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RebootDBInstance(
		ctx,
		&rds.RebootDBInstanceInput{DBInstanceIdentifier: new("selected")},
	); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "RDS SNS notification through SQS", func() bool {
		out, receiveErr := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: new(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		if len(out.Messages) == 0 {
			return false
		}

		if len(out.Messages) != 1 {
			t.Fatalf("received %d notifications, want one", len(out.Messages))
		}

		var envelope struct{ TopicArn, Subject, Message string }
		if envelopeErr := json.Unmarshal([]byte(*out.Messages[0].Body), &envelope); envelopeErr != nil {
			t.Fatal(envelopeErr)
		}

		var body map[string]any
		if bodyErr := json.Unmarshal([]byte(envelope.Message), &body); bodyErr != nil {
			t.Fatal(bodyErr)
		}

		if envelope.TopicArn != *topic.TopicArn || envelope.Subject != "RDS Notification Message" ||
			body["Event Source"] != "db-instance" || body["Source ID"] != "selected" || body["Source ARN"] != *sourceARN ||
			body["Event Message"] != "DB instance restarted" || body["Event ID"] == nil {
			t.Fatal(envelope, body)
		}

		if _, deleteErr := queues.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: new(queueURL), ReceiptHandle: out.Messages[0].ReceiptHandle,
		}); deleteErr != nil {
			t.Fatal(deleteErr)
		}

		return true
	})

	described, err := c.DescribeEventSubscriptions(ctx, &rds.DescribeEventSubscriptionsInput{
		SubscriptionName: in.SubscriptionName,
	})
	if err != nil || len(described.EventSubscriptionsList) != 1 ||
		!aws.ToBool(described.EventSubscriptionsList[0].Enabled) {
		t.Fatal(described, err)
	}

	if _, err = c.DeleteEventSubscription(ctx, &rds.DeleteEventSubscriptionInput{
		SubscriptionName: in.SubscriptionName,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeEventSubscriptions(
		ctx,
		&rds.DescribeEventSubscriptionsInput{SubscriptionName: in.SubscriptionName},
	)
	code(t, err, "SubscriptionNotFound")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, receiveErr := queues.ReceiveMessage(
			ctx,
			&sqs.ReceiveMessageInput{QueueUrl: new(queueURL), WaitTimeSeconds: 1},
		)
		if receiveErr != nil || len(out.Messages) != 0 {
			t.Fatal("unexpected notification after delivery", out, receiveErr)
		}
	}
}

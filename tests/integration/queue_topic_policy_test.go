package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const allowPublish = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
	`"Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage","Resource":"*"}]}`

func TestSQSQueuePolicy(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "orders", map[string]string{"Policy": allowPublish})
	policy := func() (string, bool) {
		t.Helper()
		out, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: &queueURL, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
		})
		if err != nil {
			t.Fatal(err)
		}

		v, ok := out.Attributes["Policy"]
		return v, ok
	}

	if got, _ := policy(); got != allowPublish {
		t.Fatalf("policy %q", got)
	}

	for _, bad := range []string{"{", `{"Version":"2012-10-17"}`, "[]"} {
		_, err := c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl: &queueURL, Attributes: map[string]string{"Policy": bad},
		})
		code(t, err, "InvalidAttributeValue")
	}

	if _, err := c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &queueURL, Attributes: map[string]string{"Policy": ""},
	}); err != nil {
		t.Fatal(err)
	}

	if got, ok := policy(); ok {
		t.Fatalf("policy after removal %q", got)
	}
}

func TestSNSTopicPolicy(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	ctx := context.Background()
	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("alerts")})
	if err != nil {
		t.Fatal(err)
	}

	attributes := func() map[string]string {
		t.Helper()
		out, getErr := c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
		if getErr != nil {
			t.Fatal(getErr)
		}

		return out.Attributes
	}

	if !strings.Contains(attributes()["Policy"], "__default_policy_ID") {
		t.Fatalf("default policy %q", attributes()["Policy"])
	}

	_, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: aws.String("Policy"), AttributeValue: aws.String("{}"),
	})
	code(t, err, "InvalidParameter")

	if _, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: aws.String("Policy"), AttributeValue: aws.String(allowPublish),
	}); err != nil {
		t.Fatal(err)
	}

	if got := attributes()["Policy"]; got != allowPublish {
		t.Fatalf("policy %q", got)
	}

	created, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String("events"), Attributes: map[string]string{"Policy": allowPublish},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: created.TopicArn})
	if err != nil || out.Attributes["Policy"] != allowPublish {
		t.Fatalf("created %+v, %v", out, err)
	}
}

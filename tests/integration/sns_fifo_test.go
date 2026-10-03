package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func TestSNSFIFOTopics(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String("orders.fifo"), Attributes: map[string]string{"FifoTopic": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	attributes, err := c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
	if err != nil || attributes.Attributes["FifoTopic"] != "true" ||
		attributes.Attributes["ContentBasedDeduplication"] != "false" {
		t.Fatalf("attributes: %+v %v", attributes, err)
	}

	var badParam *snstypes.InvalidParameterException
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("lambda"),
		Endpoint: aws.String("arn:aws:lambda:us-east-1:000000000000:function:f"),
	}); !errors.As(err, &badParam) {
		t.Fatalf("a FIFO topic accepted a function: %v", err)
	}

	queueURL := createQueue(t, q, "orders.fifo", map[string]string{"FifoQueue": "true"})
	queueAttributes, err := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: &queueURL, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	queue := queueAttributes.Attributes["QueueArn"]
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &queue,
	}); err != nil {
		t.Fatal(err)
	}

	standard, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("standard")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: standard.TopicArn, Protocol: aws.String("sqs"), Endpoint: &queue,
	}); !errors.As(err, &badParam) {
		t.Fatalf("a standard topic accepted a FIFO queue: %v", err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: aws.String("no group"), MessageDeduplicationId: aws.String("d"),
	}); !errors.As(err, &badParam) {
		t.Fatalf("published without a group: %v", err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: aws.String("no dedup"), MessageGroupId: aws.String("g"),
	}); !errors.As(err, &badParam) {
		t.Fatalf("published without deduplication: %v", err)
	}

	first, err := c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: aws.String("one"), MessageGroupId: aws.String("g"),
		MessageDeduplicationId: aws.String("1"),
	})
	if err != nil || aws.ToString(first.SequenceNumber) == "" {
		t.Fatalf("first: %+v %v", first, err)
	}

	repeat, err := c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: aws.String("one again"), MessageGroupId: aws.String("g"),
		MessageDeduplicationId: aws.String("1"),
	})
	if err != nil || aws.ToString(repeat.MessageId) != aws.ToString(first.MessageId) ||
		aws.ToString(repeat.SequenceNumber) != aws.ToString(first.SequenceNumber) {
		t.Fatalf("repeat: %+v %v", repeat, err)
	}

	if _, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: aws.String("ContentBasedDeduplication"),
		AttributeValue: aws.String("true"),
	}); err != nil {
		t.Fatal(err)
	}

	batch, err := c.PublishBatch(ctx, &sns.PublishBatchInput{
		TopicArn: topic.TopicArn,
		PublishBatchRequestEntries: []snstypes.PublishBatchRequestEntry{
			{Id: aws.String("a"), Message: aws.String("two"), MessageGroupId: aws.String("g")},
			{Id: aws.String("b"), Message: aws.String("two"), MessageGroupId: aws.String("g")},
		},
	})
	if err != nil || len(batch.Successful) != 2 ||
		aws.ToString(batch.Successful[0].MessageId) != aws.ToString(batch.Successful[1].MessageId) ||
		aws.ToString(batch.Successful[0].SequenceNumber) <= aws.ToString(first.SequenceNumber) {
		t.Fatalf("batch: %+v %v", batch, err)
	}

	got, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, MaxNumberOfMessages: 10,
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{"MessageGroupId"},
	})
	if err != nil || len(got.Messages) != 2 || got.Messages[0].Attributes["MessageGroupId"] != "g" {
		t.Fatalf("received: %+v %v", got, err)
	}

	var envelope struct{ Message, SequenceNumber string }
	if err = json.Unmarshal([]byte(aws.ToString(got.Messages[0].Body)), &envelope); err != nil ||
		envelope.Message != "one" || envelope.SequenceNumber != aws.ToString(first.SequenceNumber) {
		t.Fatalf("first message: %+v %v", envelope, err)
	}
}

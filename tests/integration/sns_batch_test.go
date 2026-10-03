package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestSNSPublishBatch(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("batch")})
	if err != nil {
		t.Fatal(err)
	}

	queueURL, arn := queueARN(t, q, "batch-inbox")
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &arn,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := c.PublishBatch(ctx, &sns.PublishBatchInput{
		TopicArn: topic.TopicArn,
		PublishBatchRequestEntries: []snstypes.PublishBatchRequestEntry{
			{Id: aws.String("one"), Message: aws.String("first")},
			{Id: aws.String("two"), Message: aws.String("")},
			{Id: aws.String("three"), Message: aws.String("third")},
		},
	})
	if err != nil || len(out.Successful) != 2 || len(out.Failed) != 1 ||
		aws.ToString(out.Failed[0].Id) != "two" || aws.ToString(out.Failed[0].Code) != "InvalidParameter" ||
		!out.Failed[0].SenderFault || aws.ToString(out.Successful[1].MessageId) == "" {
		t.Fatalf("batch: %+v %v", out, err)
	}

	got, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10})
	if err != nil || len(got.Messages) != 2 {
		t.Fatalf("received: %+v %v", got, err)
	}

	var distinct *snstypes.BatchEntryIdsNotDistinctException
	if _, err = c.PublishBatch(ctx, &sns.PublishBatchInput{
		TopicArn: topic.TopicArn,
		PublishBatchRequestEntries: []snstypes.PublishBatchRequestEntry{
			{Id: aws.String("a"), Message: aws.String("x")}, {Id: aws.String("a"), Message: aws.String("y")},
		},
	}); !errors.As(err, &distinct) {
		t.Fatalf("repeated IDs: %v", err)
	}

	var empty *snstypes.EmptyBatchRequestException
	if _, err = c.PublishBatch(ctx, &sns.PublishBatchInput{
		TopicArn: topic.TopicArn, PublishBatchRequestEntries: []snstypes.PublishBatchRequestEntry{},
	}); !errors.As(err, &empty) {
		t.Fatalf("no entries: %v", err)
	}

	entries := make([]snstypes.PublishBatchRequestEntry, 11)
	for i := range entries {
		entries[i] = snstypes.PublishBatchRequestEntry{Id: aws.String(string(rune('a' + i))), Message: aws.String("m")}
	}

	var tooMany *snstypes.TooManyEntriesInBatchRequestException
	if _, err = c.PublishBatch(ctx, &sns.PublishBatchInput{
		TopicArn: topic.TopicArn, PublishBatchRequestEntries: entries,
	}); !errors.As(err, &tooMany) {
		t.Fatalf("eleven entries: %v", err)
	}
}

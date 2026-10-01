package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func TestSQSHighThroughputFIFO(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()

	_, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("plain"), Attributes: map[string]string{"DeduplicationScope": "messageGroup"},
	})
	wantCode(t, err, "InvalidAttributeName")

	_, err = c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("mixed.fifo"),
		Attributes: map[string]string{"FifoQueue": "true", "FifoThroughputLimit": "perMessageGroupId"},
	})
	wantCode(t, err, "InvalidAttributeValue")

	plainFIFO := createQueue(t, c, "orders.fifo", map[string]string{"FifoQueue": "true"})
	attributes, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: &plainFIFO, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
	})
	if err != nil || attributes.Attributes["DeduplicationScope"] != "queue" ||
		attributes.Attributes["FifoThroughputLimit"] != "perQueue" {
		t.Fatalf("defaults = %+v, %v", attributes, err)
	}

	queueURL := createQueue(t, c, "events.fifo", map[string]string{
		"FifoQueue": "true", "DeduplicationScope": "messageGroup", "FifoThroughputLimit": "perMessageGroupId",
	})

	// With the messageGroup scope, one deduplication ID is unique per group.
	for _, group := range []string{"a", "b", "a"} {
		if _, err = c.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: &queueURL, MessageBody: aws.String("body-" + group), MessageGroupId: aws.String(group),
			MessageDeduplicationId: aws.String("same"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	attributes, err = c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil || attributes.Attributes["ApproximateNumberOfMessages"] != "2" {
		t.Fatalf("messages = %+v, %v", attributes, err)
	}

	// A receive retried with its ReceiveRequestAttemptId gets the same
	// messages and receipt handles.
	first, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, MaxNumberOfMessages: 10, ReceiveRequestAttemptId: aws.String("attempt-1"),
	})
	if err != nil || len(first.Messages) != 2 {
		t.Fatalf("receive = %+v, %v", first, err)
	}

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, MaxNumberOfMessages: 10, ReceiveRequestAttemptId: aws.String("attempt-1"),
	})
	if err != nil || len(again.Messages) != 2 {
		t.Fatalf("retry = %+v, %v", again, err)
	}

	for i := range first.Messages {
		if aws.ToString(first.Messages[i].ReceiptHandle) != aws.ToString(again.Messages[i].ReceiptHandle) {
			t.Fatalf("retry returned other receipt handles")
		}
	}

	other, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, MaxNumberOfMessages: 10, ReceiveRequestAttemptId: aws.String("attempt-2"),
	})
	if err != nil || len(other.Messages) != 0 {
		t.Fatalf("new attempt = %+v, %v", other, err)
	}

	// Once a message is deleted, the attempt no longer replays.
	if _, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: &queueURL, ReceiptHandle: first.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	replay, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, MaxNumberOfMessages: 10, ReceiveRequestAttemptId: aws.String("attempt-1"),
	})
	if err != nil || len(replay.Messages) != 0 {
		t.Fatalf("replay after delete = %+v, %v", replay, err)
	}

	_, err = c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, ReceiveRequestAttemptId: aws.String("has space"),
	})
	wantCode(t, err, "InvalidParameterValue")
}

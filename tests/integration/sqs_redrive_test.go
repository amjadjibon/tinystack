package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// deadLetters sends n messages to the queue at sourceURL, which has no
// visibility timeout, and receives the queue's messages twice, so a redrive
// policy with a maxReceiveCount of 1 moves them all to the dead-letter queue.
func deadLetters(t *testing.T, c *sqs.Client, sourceURL string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		_, err := c.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: aws.String(sourceURL), MessageBody: aws.String(string(rune('a' + i))),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	for range 2 {
		if _, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(sourceURL), MaxNumberOfMessages: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// visible returns the number of visible messages in a queue.
func visible(t *testing.T, c *sqs.Client, queueURL string) string {
	t.Helper()
	out, err := c.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL), AttributeNames: []sqstypes.QueueAttributeName{"ApproximateNumberOfMessages"},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Attributes["ApproximateNumberOfMessages"]
}

// finished waits for the newest move task of a source queue to leave RUNNING.
func finished(t *testing.T, c *sqs.Client, sourceARN string) sqstypes.ListMessageMoveTasksResultEntry {
	t.Helper()
	for range 100 {
		out, err := c.ListMessageMoveTasks(context.Background(), &sqs.ListMessageMoveTasksInput{
			SourceArn: aws.String(sourceARN),
		})
		if err != nil || len(out.Results) != 1 {
			t.Fatalf("got %+v, %v", out, err)
		}

		if task := out.Results[0]; aws.ToString(task.Status) != "RUNNING" {
			return task
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("the task did not finish")
	return sqstypes.ListMessageMoveTasksResultEntry{}
}

func TestSQSRedrive(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	// Received messages stay visible, so they can be received again at once.
	dlqURL := createQueue(t, c, "orders-dlq", map[string]string{"VisibilityTimeout": "0"})
	dlqARN := "arn:aws:sqs:us-east-1:000000000000:orders-dlq"
	ordersURL := createQueue(t, c, "orders", map[string]string{
		"VisibilityTimeout": "0",
		"RedrivePolicy":     `{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":1}`,
	})
	archiveURL, archiveARN := queueARN(t, c, "archive")
	ordersARN := "arn:aws:sqs:us-east-1:000000000000:orders"

	sources, err := c.ListDeadLetterSourceQueues(ctx, &sqs.ListDeadLetterSourceQueuesInput{
		QueueUrl: aws.String(dlqURL),
	})
	if err != nil || len(sources.QueueUrls) != 1 || sources.QueueUrls[0] != ordersURL {
		t.Fatalf("got %+v, %v", sources, err)
	}

	// A dead-lettered message remembers the queue it came from.
	deadLetters(t, c, ordersURL, 3)
	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(dlqURL), MaxNumberOfMessages: 10,
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{"DeadLetterQueueSourceArn"},
	})
	if err != nil || len(received.Messages) != 3 ||
		received.Messages[0].Attributes["DeadLetterQueueSourceArn"] != ordersARN {
		t.Fatalf("got %+v, %v", received, err)
	}

	// Without a destination, messages go back to their source queue.
	started, err := c.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{SourceArn: aws.String(dlqARN)})
	if err != nil || aws.ToString(started.TaskHandle) == "" {
		t.Fatalf("got %+v, %v", started, err)
	}

	task := finished(t, c, dlqARN)
	if aws.ToString(task.Status) != "COMPLETED" || task.ApproximateNumberOfMessagesMoved != 3 ||
		aws.ToInt64(task.ApproximateNumberOfMessagesToMove) != 3 || task.TaskHandle != nil {
		t.Fatalf("got %+v", task)
	}

	if n := visible(t, c, ordersURL); n != "3" {
		t.Fatalf("%s messages back in the source queue", n)
	}

	if n := visible(t, c, dlqURL); n != "0" {
		t.Fatalf("%s messages left in the dead-letter queue", n)
	}

	// A slow task to another queue can be cancelled, and only one runs at a time.
	deadLetters(t, c, ordersURL, 0)
	started, err = c.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{
		SourceArn: aws.String(dlqARN), DestinationArn: aws.String(archiveARN),
		MaxNumberOfMessagesPerSecond: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{SourceArn: aws.String(dlqARN)})
	code(t, err, "AWS.SimpleQueueService.UnsupportedOperation")

	cancelled, err := c.CancelMessageMoveTask(ctx, &sqs.CancelMessageMoveTaskInput{
		TaskHandle: started.TaskHandle,
	})
	if err != nil || cancelled.ApproximateNumberOfMessagesMoved > 1 {
		t.Fatalf("got %+v, %v", cancelled, err)
	}

	_, err = c.CancelMessageMoveTask(ctx, &sqs.CancelMessageMoveTaskInput{TaskHandle: started.TaskHandle})
	code(t, err, "AWS.SimpleQueueService.UnsupportedOperation")

	tasks, err := c.ListMessageMoveTasks(ctx, &sqs.ListMessageMoveTasksInput{
		SourceArn: aws.String(dlqARN), MaxResults: aws.Int32(10),
	})
	if err != nil || len(tasks.Results) != 2 || aws.ToString(tasks.Results[0].Status) != "CANCELLED" ||
		aws.ToString(tasks.Results[0].DestinationArn) != archiveARN ||
		aws.ToInt32(tasks.Results[0].MaxNumberOfMessagesPerSecond) != 1 ||
		aws.ToString(tasks.Results[1].Status) != "COMPLETED" {
		t.Fatalf("got %+v, %v", tasks, err)
	}

	moved := visible(t, c, archiveURL)
	if moved != "0" && moved != "1" {
		t.Fatalf("%s messages moved before the cancel", moved)
	}

	// A queue that is no one's dead-letter queue has nothing to redrive.
	_, err = c.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{SourceArn: aws.String(archiveARN)})
	code(t, err, "InvalidParameterValue")
	for _, source := range []string{
		"arn:aws:sqs:us-east-1:000000000000:missing", "arn:aws:sqs:us-east-1:111111111111:orders-dlq",
	} {
		_, err = c.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{SourceArn: aws.String(source)})
		code(t, err, "ResourceNotFoundException")
	}

	_, err = c.CancelMessageMoveTask(ctx, &sqs.CancelMessageMoveTaskInput{TaskHandle: aws.String("nope")})
	code(t, err, "ResourceNotFoundException")
}

func TestSQSMoveTaskSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := sqsClient(s, "test")
	createQueue(t, c, "dlq", nil)
	dlqARN := "arn:aws:sqs:us-east-1:000000000000:dlq"
	ordersURL := createQueue(t, c, "orders", map[string]string{
		"VisibilityTimeout": "0",
		"RedrivePolicy":     `{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":1}`,
	})
	deadLetters(t, c, ordersURL, 3)

	// At one message per second, the task is still running when the server stops.
	_, err := c.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{
		SourceArn: aws.String(dlqARN), MaxNumberOfMessagesPerSecond: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = sqsClient(s, "test")
	task := finished(t, c, dlqARN)
	if aws.ToString(task.Status) != "COMPLETED" || task.ApproximateNumberOfMessagesMoved != 3 {
		t.Fatalf("got %+v", task)
	}

	if n := visible(t, c, ordersURL); n != "3" {
		t.Fatalf("%s messages back in the source queue", n)
	}
}

func TestSQSRedriveAllowPolicy(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	dlqURL, dlqARN := queueARN(t, c, "dlq")
	redrive := map[string]string{"RedrivePolicy": `{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":3}`}

	set := func(policy string) error {
		_, err := c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl: aws.String(dlqURL), Attributes: map[string]string{"RedriveAllowPolicy": policy},
		})
		return err
	}

	if err := set(`{"redrivePermission":"denyAll"}`); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("jobs"), Attributes: redrive})
	code(t, err, "InvalidAttributeValue")

	policy := `{"redrivePermission":"byQueue","sourceQueueArns":["arn:aws:sqs:us-east-1:000000000000:jobs"]}`
	if err = set(policy); err != nil {
		t.Fatal(err)
	}

	createQueue(t, c, "jobs", redrive)
	_, err = c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("other"), Attributes: redrive})
	code(t, err, "InvalidAttributeValue")

	got, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(dlqURL), AttributeNames: []sqstypes.QueueAttributeName{"RedriveAllowPolicy"},
	})
	if err != nil || got.Attributes["RedriveAllowPolicy"] != policy {
		t.Fatalf("got %+v, %v", got, err)
	}

	for _, bad := range []string{
		`{"redrivePermission":"byQueue"}`, `{"redrivePermission":"some"}`, `not json`,
		`{"redrivePermission":"allowAll","sourceQueueArns":["arn:aws:sqs:us-east-1:000000000000:jobs"]}`,
	} {
		code(t, set(bad), "InvalidAttributeValue")
	}
}

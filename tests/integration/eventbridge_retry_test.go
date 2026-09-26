package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestEventBridgeRetriesAndDeadLetters(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	q := sqsClient(s, "test")
	keys := kmsClient(s, "test", "us-east-1")
	ctx := context.Background()
	key := createKey(t, keys, "orders")
	ordersURL := createQueue(t, q, "orders", map[string]string{"KmsMasterKeyId": aws.ToString(key.KeyId)})
	goneURL := createQueue(t, q, "gone", nil)
	dlqURL := createQueue(t, q, "dlq", nil)
	putRule(t, c, "orders", orderPattern)
	queue := func(name string) string { return "arn:aws:sqs:us-east-1:000000000000:" + name }
	dlq := &ebtypes.DeadLetterConfig{Arn: aws.String(queue("dlq"))}

	for _, target := range []ebtypes.Target{
		{RetryPolicy: &ebtypes.RetryPolicy{MaximumRetryAttempts: aws.Int32(186)}},
		{RetryPolicy: &ebtypes.RetryPolicy{MaximumEventAgeInSeconds: aws.Int32(59)}},
		{DeadLetterConfig: &ebtypes.DeadLetterConfig{Arn: aws.String("arn:aws:sns:us-east-1:000000000000:t")}},
		{DeadLetterConfig: &ebtypes.DeadLetterConfig{Arn: aws.String("arn:aws:sqs:eu-west-1:000000000000:q")}},
	} {
		target.Id, target.Arn = aws.String("t"), aws.String(queue("orders"))
		_, err := c.PutTargets(ctx, &eventbridge.PutTargetsInput{
			Rule: aws.String("orders"), Targets: []ebtypes.Target{target},
		})
		code(t, err, "ValidationException")
	}

	_, err := c.PutTargets(ctx, &eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: []ebtypes.Target{
		{
			Id:               aws.String("retried"),
			Arn:              aws.String(queue("orders")),
			DeadLetterConfig: dlq,
			RetryPolicy: &ebtypes.RetryPolicy{
				MaximumRetryAttempts:     aws.Int32(10),
				MaximumEventAgeInSeconds: aws.Int32(600),
			},
		},
		{Id: aws.String("gone"), Arn: aws.String(queue("gone")), DeadLetterConfig: dlq},
	}})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String("orders")})
	if err != nil || len(listed.Targets) != 2 {
		t.Fatalf("got %+v, %v", listed, err)
	}

	retried := listed.Targets[1]
	if aws.ToString(retried.Id) != "retried" || aws.ToInt32(retried.RetryPolicy.MaximumRetryAttempts) != 10 ||
		aws.ToInt32(retried.RetryPolicy.MaximumEventAgeInSeconds) != 600 ||
		aws.ToString(retried.DeadLetterConfig.Arn) != queue("dlq") || listed.Targets[0].RetryPolicy != nil {
		t.Fatalf("got %+v", listed.Targets)
	}

	// A deleted target is not retried: its event goes to the dead-letter queue
	// at once. A disabled key fails the other delivery, which is retried once
	// the key works again.
	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(goneURL)}); err != nil {
		t.Fatal(err)
	}

	if _, err = keys.DisableKey(ctx, &kms.DisableKeyInput{KeyId: key.KeyId}); err != nil {
		t.Fatal(err)
	}

	putOrder := func(detail string) {
		t.Helper()
		out, putErr := c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
			Source: aws.String("shop"), DetailType: aws.String("order"), Detail: aws.String(detail),
		}}})
		if putErr != nil || out.FailedEntryCount != 0 {
			t.Fatalf("got %+v, %v", out, putErr)
		}
	}

	putOrder(`{"id":1}`)
	dead := receiveAttributes(t, q, dlqURL)
	if dead["TARGET_ARN"] != queue("gone") || dead["ERROR_CODE"] != "QueueDoesNotExist" ||
		dead["RETRY_ATTEMPTS"] != "0" || dead["RULE_ARN"] != "arn:aws:events:us-east-1:000000000000:rule/orders" {
		t.Fatalf("dead letter %v", dead)
	}

	if _, err = keys.EnableKey(ctx, &kms.EnableKeyInput{KeyId: key.KeyId}); err != nil {
		t.Fatal(err)
	}

	retriedOut, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &ordersURL, WaitTimeSeconds: 5})
	if err != nil || len(retriedOut.Messages) != 1 ||
		!strings.Contains(aws.ToString(retriedOut.Messages[0].Body), `"id":1`) {
		t.Fatalf("retried event %+v, %v", retriedOut, err)
	}

	// Without retries, a failed delivery goes to the dead-letter queue.
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: []ebtypes.Target{{
		Id: aws.String("retried"), Arn: aws.String(queue("orders")), DeadLetterConfig: dlq,
		RetryPolicy: &ebtypes.RetryPolicy{MaximumRetryAttempts: aws.Int32(0)},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
		Rule: aws.String("orders"), Ids: []string{"gone"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = keys.DisableKey(ctx, &kms.DisableKeyInput{KeyId: key.KeyId}); err != nil {
		t.Fatal(err)
	}

	putOrder(`{"id":2}`)
	dead = receiveAttributes(t, q, dlqURL)
	if dead["TARGET_ARN"] != queue("orders") || dead["ERROR_CODE"] != "KmsDisabled" ||
		dead["EXHAUSTED_RETRY_CONDITION"] != "MaximumRetryAttemptsExhausted" {
		t.Fatalf("dead letter %v", dead)
	}
}

// receiveAttributes waits for one message and returns its string attributes.
func receiveAttributes(t *testing.T, q *sqs.Client, queueURL string) map[string]string {
	t.Helper()
	out, err := q.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(queueURL), WaitTimeSeconds: 5, MessageAttributeNames: []string{"All"},
	})
	if err != nil || len(out.Messages) != 1 {
		t.Fatalf("got %+v, %v", out, err)
	}

	attributes := map[string]string{}
	for name, v := range out.Messages[0].MessageAttributes {
		attributes[name] = aws.ToString(v.StringValue)
	}

	if _, err = q.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
		QueueUrl: aws.String(queueURL), ReceiptHandle: out.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	return attributes
}

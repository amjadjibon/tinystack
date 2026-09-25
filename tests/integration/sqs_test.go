package integration_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func sqsClient(s *httptest.Server, key string) *sqs.Client {
	return sqs.New(
		sqs.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

func createQueue(t *testing.T, c *sqs.Client, name string, attributes map[string]string) string {
	t.Helper()
	out, err := c.CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: &name, Attributes: attributes})
	if err != nil {
		t.Fatal(err)
	}

	return *out.QueueUrl
}

func TestSQSLifecycle(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "orders", map[string]string{"VisibilityTimeout": "60"})
	if queueURL != s.URL+"/000000000000/orders" {
		t.Fatalf("queue URL %s", queueURL)
	}

	if again := createQueue(t, c, "orders", map[string]string{"VisibilityTimeout": "60"}); again != queueURL {
		t.Fatal("identical CreateQueue was not idempotent")
	}

	_, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("orders")})
	var exists *types.QueueNameExists
	if !errors.As(err, &exists) || exists.ErrorCode() != "QueueAlreadyExists" {
		t.Fatalf("conflicting CreateQueue: %v", err)
	}

	createQueue(t, c, "orders-dlq", nil)
	createQueue(t, c, "emails", nil)

	got, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders")})
	if err != nil || *got.QueueUrl != queueURL {
		t.Fatal(got, err)
	}

	page, err := c.ListQueues(
		ctx,
		&sqs.ListQueuesInput{QueueNamePrefix: aws.String("orders"), MaxResults: aws.Int32(1)},
	)
	if err != nil || len(page.QueueUrls) != 1 || page.QueueUrls[0] != queueURL || page.NextToken == nil {
		t.Fatal(page, err)
	}

	page, err = c.ListQueues(ctx, &sqs.ListQueuesInput{
		QueueNamePrefix: aws.String("orders"),
		MaxResults:      aws.Int32(1),
		NextToken:       page.NextToken,
	})
	if err != nil || len(page.QueueUrls) != 1 || !strings.HasSuffix(page.QueueUrls[0], "/orders-dlq") {
		t.Fatal(page, err)
	}

	sent, err := c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &queueURL,
		MessageBody: aws.String("hello ✓"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"color":  {DataType: aws.String("String"), StringValue: aws.String("blue")},
			"count":  {DataType: aws.String("Number"), StringValue: aws.String("3")},
			"blob":   {DataType: aws.String("Binary"), BinaryValue: []byte{0, 1, 2}},
			"ignore": {DataType: aws.String("String.custom"), StringValue: aws.String("x")},
		},
	})
	if err != nil || sent.MD5OfMessageAttributes == nil {
		t.Fatal(sent, err)
	}

	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    &queueURL,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil || len(received.Messages) != 1 {
		t.Fatal(received, err)
	}

	msg := received.Messages[0]
	if *msg.MessageId != *sent.MessageId || *msg.Body != "hello ✓" || len(msg.MessageAttributes) != 4 ||
		*msg.MD5OfMessageAttributes != *sent.MD5OfMessageAttributes ||
		string(msg.MessageAttributes["blob"].BinaryValue) != "\x00\x01\x02" ||
		msg.Attributes["ApproximateReceiveCount"] != "1" || msg.Attributes["SenderId"] != "test" {
		t.Fatalf("received %#v", msg)
	}

	empty, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(empty.Messages) != 0 {
		t.Fatal("in-flight message was delivered again", err)
	}

	if _, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 0,
	}); err != nil {
		t.Fatal(err)
	}

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              &queueURL,
		MessageAttributeNames: []string{"co.*", "color"},
		//nolint:staticcheck // The deprecated member is still sent by older clients.
		AttributeNames: []types.QueueAttributeName{"ApproximateReceiveCount"},
	})
	if err != nil || len(again.Messages) != 1 || again.Messages[0].Attributes["ApproximateReceiveCount"] != "2" ||
		len(again.Messages[0].MessageAttributes) != 1 {
		t.Fatal(again, err)
	}

	_, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 5,
	})
	var stale *types.ReceiptHandleIsInvalid
	if !errors.As(err, &stale) {
		t.Fatalf("stale receipt changed visibility: %v", err)
	}
	// Deleting with a stale receipt handle succeeds without deleting.
	if _, err = c.DeleteMessage(
		ctx,
		&sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: &queueURL, ReceiptHandle: again.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: again.Messages[0].ReceiptHandle, VisibilityTimeout: 0,
	})
	if !errors.As(err, &stale) {
		t.Fatalf("deleted message changed visibility: %v", err)
	}

	_, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: aws.String("garbage")})
	if !errors.As(err, &stale) {
		t.Fatalf("malformed receipt handle accepted: %v", err)
	}

	if _, err = c.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &queueURL}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders")})
	var gone *types.QueueDoesNotExist
	if !errors.As(err, &gone) || gone.ErrorCode() != "AWS.SimpleQueueService.NonExistentQueue" {
		t.Fatalf("deleted queue still resolves: %v", err)
	}
}

func TestSQSValidation(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "limits", map[string]string{"MaximumMessageSize": "1024"})
	for name, in := range map[string]*sqs.SendMessageInput{
		"too large":         {MessageBody: aws.String(strings.Repeat("x", 1025))},
		"invalid character": {MessageBody: aws.String("bad \x00")},
		"reserved name": {MessageBody: aws.String("ok"), MessageAttributes: map[string]types.MessageAttributeValue{
			"AWS.trace": {DataType: aws.String("String"), StringValue: aws.String("x")},
		}},
		"bad number": {MessageBody: aws.String("ok"), MessageAttributes: map[string]types.MessageAttributeValue{
			"n": {DataType: aws.String("Number"), StringValue: aws.String("NaN")},
		}},
	} {
		in.QueueUrl = &queueURL
		if _, err := c.SendMessage(ctx, in); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}

	if _, err := c.ReceiveMessage(
		ctx,
		&sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 11},
	); err == nil {
		t.Fatal("MaxNumberOfMessages 11 accepted")
	}

	team := sqsClient(s, "team-b")
	isolated := createQueue(t, team, "limits", nil)
	if isolated == queueURL {
		t.Fatal("accounts share queue URLs")
	}

	_, err := c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &isolated, MessageBody: aws.String("x")})
	var gone *types.QueueDoesNotExist
	if !errors.As(err, &gone) {
		t.Fatalf("cross-account queue URL accepted: %v", err)
	}

	listed, err := c.ListQueues(ctx, &sqs.ListQueuesInput{})
	if err != nil || len(listed.QueueUrls) != 1 {
		t.Fatal(listed, err)
	}
}

func TestSQSLongPolling(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "poll", nil)

	start := time.Now()
	done := make(chan *sqs.ReceiveMessageOutput, 1)
	go func() {
		out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 10})
		if err != nil {
			t.Error(err)
		}

		done <- out
	}()

	time.Sleep(200 * time.Millisecond)
	if _, err := c.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("wake")},
	); err != nil {
		t.Fatal(err)
	}

	out := <-done
	if out == nil || len(out.Messages) != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("long poll did not wake on send: %v", out)
	}

	// An expiring visibility timeout wakes a waiting receiver.
	start = time.Now()
	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 1})
	if err != nil || len(again.Messages) != 0 {
		t.Fatal("in-flight message delivered", err)
	}

	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("long poll returned before its wait time")
	}

	if _, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: out.Messages[0].ReceiptHandle, VisibilityTimeout: 1,
	}); err != nil {
		t.Fatal(err)
	}

	start = time.Now()
	again, err = c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 10})
	if err != nil || len(again.Messages) != 1 || time.Since(start) > 5*time.Second {
		t.Fatal("expired visibility did not wake the long poll", err)
	}
}

func TestSQSDelay(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "delayed", map[string]string{"DelaySeconds": "1"})

	attrs, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("too-late"),
		Attributes: map[string]string{"DelaySeconds": "901"},
	})
	var badValue *types.InvalidAttributeValue
	if !errors.As(err, &badValue) {
		t.Fatalf("DelaySeconds 901 accepted: %v %v", attrs, err)
	}

	_, err = c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: &queueURL, MessageBody: aws.String("x"), DelaySeconds: 901,
	})
	if err == nil || !strings.Contains(err.Error(), "InvalidParameterValue") {
		t.Fatalf("message DelaySeconds 901 accepted: %v", err)
	}

	// The queue default delays the message until a long poll picks it up.
	start := time.Now()
	if _, err = c.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("later")},
	); err != nil {
		t.Fatal(err)
	}

	early, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(early.Messages) != 0 {
		t.Fatal("delayed message delivered early", err)
	}

	out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 5})
	if err != nil || len(out.Messages) != 1 || *out.Messages[0].Body != "later" {
		t.Fatal("long poll did not wake when the delay ended", err)
	}

	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("delayed message arrived after %s", elapsed)
	}

	// A per-message delay of zero overrides the queue default. The Go SDK omits
	// a zero DelaySeconds, so send it over the Query wire.
	status, _, body := queryCall(t, s, "/", url.Values{
		"Action": {"SendMessage"}, "QueueUrl": {queueURL}, "MessageBody": {"now"}, "DelaySeconds": {"0"},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("SendMessage with DelaySeconds=0: %d %s", status, body)
	}

	now, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(now.Messages) != 1 || *now.Messages[0].Body != "now" {
		t.Fatal("zero message delay did not override the queue delay", err)
	}
}

func TestSQSBatch(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "batch", nil)

	entry := func(id string) types.SendMessageBatchRequestEntry {
		return types.SendMessageBatchRequestEntry{Id: aws.String(id), MessageBody: aws.String("body " + id)}
	}

	var tooMany []types.SendMessageBatchRequestEntry
	for i := range 11 {
		tooMany = append(tooMany, entry(fmt.Sprint("e", i)))
	}

	var empty *types.EmptyBatchRequest
	var many *types.TooManyEntriesInBatchRequest
	var repeated *types.BatchEntryIdsNotDistinct
	var badID *types.InvalidBatchEntryId
	var tooLong *types.BatchRequestTooLong
	// Six 200 KB bodies fit the message size limit but not the 1 MiB batch limit.
	var huge []types.SendMessageBatchRequestEntry
	for i := range 6 {
		huge = append(huge, types.SendMessageBatchRequestEntry{
			Id: aws.String(fmt.Sprint("h", i)), MessageBody: aws.String(strings.Repeat("x", 200000)),
		})
	}

	for _, tc := range []struct {
		target  any
		entries []types.SendMessageBatchRequestEntry
	}{
		{&empty, []types.SendMessageBatchRequestEntry{}},
		{&many, tooMany},
		{&repeated, []types.SendMessageBatchRequestEntry{entry("a"), entry("a")}},
		{&badID, []types.SendMessageBatchRequestEntry{entry("not valid")}},
		{&tooLong, huge},
	} {
		_, err := c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: &queueURL, Entries: tc.entries})
		if !errors.As(err, tc.target) {
			t.Fatalf("expected %T, got %v", tc.target, err)
		}
	}

	// Entries succeed or fail independently.
	sent, err := c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: &queueURL,
		Entries: []types.SendMessageBatchRequestEntry{
			entry("first"),
			{
				Id:          aws.String("second"),
				MessageBody: aws.String("with attributes"),
				MessageAttributes: map[string]types.MessageAttributeValue{
					"color": {DataType: aws.String("String"), StringValue: aws.String("red")},
				},
			},
			{Id: aws.String("bad"), MessageBody: aws.String("bad \x00")},
		},
	})
	if err != nil || len(sent.Successful) != 2 || len(sent.Failed) != 1 {
		t.Fatal(sent, err)
	}

	if f := sent.Failed[0]; *f.Id != "bad" || *f.Code != "InvalidMessageContents" || !f.SenderFault {
		t.Fatalf("failed entry %+v", f)
	}

	if s := sent.Successful[1]; *s.Id != "second" || s.MD5OfMessageAttributes == nil {
		t.Fatalf("successful entry %+v", s)
	}

	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10})
	if err != nil || len(received.Messages) != 2 {
		t.Fatal(received, err)
	}

	first, second := received.Messages[0].ReceiptHandle, received.Messages[1].ReceiptHandle
	changed, err := c.ChangeMessageVisibilityBatch(ctx, &sqs.ChangeMessageVisibilityBatchInput{
		QueueUrl: &queueURL,
		Entries: []types.ChangeMessageVisibilityBatchRequestEntry{
			{Id: aws.String("ok"), ReceiptHandle: first, VisibilityTimeout: 0},
			{Id: aws.String("garbage"), ReceiptHandle: aws.String("garbage")},
		},
	})
	if err != nil || len(changed.Successful) != 1 || len(changed.Failed) != 1 ||
		*changed.Failed[0].Code != "ReceiptHandleIsInvalid" {
		t.Fatal(changed, err)
	}

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10})
	if err != nil || len(again.Messages) != 1 {
		t.Fatal("visibility batch did not release one message", again, err)
	}

	deleted, err := c.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
		QueueUrl: &queueURL,
		Entries: []types.DeleteMessageBatchRequestEntry{
			{Id: aws.String("current"), ReceiptHandle: again.Messages[0].ReceiptHandle},
			{Id: aws.String("second"), ReceiptHandle: second},
			{Id: aws.String("stale"), ReceiptHandle: first},
			{Id: aws.String("garbage"), ReceiptHandle: aws.String("garbage")},
		},
	})
	if err != nil || len(deleted.Successful) != 3 || len(deleted.Failed) != 1 {
		t.Fatal(deleted, err)
	}

	left, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10})
	if err != nil || len(left.Messages) != 0 {
		t.Fatal("batch delete left messages", left, err)
	}

	// Query uses flattened, numbered entries on the way in and out.
	status, _, body := queryCall(t, s, "/", url.Values{
		"Action":                            {"SendMessageBatch"},
		"QueueUrl":                          {queueURL},
		"SendMessageBatchRequestEntry.1.Id": {"q1"},
		"SendMessageBatchRequestEntry.1.MessageBody":                       {"one"},
		"SendMessageBatchRequestEntry.1.MessageAttribute.1.Name":           {"k"},
		"SendMessageBatchRequestEntry.1.MessageAttribute.1.Value.DataType": {"String"},
		"SendMessageBatchRequestEntry.1.MessageAttribute.1.Value.StringValue": {
			"v",
		},
		"SendMessageBatchRequestEntry.2.Id":          {"q2"},
		"SendMessageBatchRequestEntry.2.MessageBody": {"bad \x00"},
	}, nil)
	var batch struct {
		Successful []struct {
			ID            string `xml:"Id"`
			AttributesMD5 string `xml:"MD5OfMessageAttributes"`
		} `xml:"SendMessageBatchResult>SendMessageBatchResultEntry"`
		Failed []struct {
			ID          string `xml:"Id"`
			Code        string `xml:"Code"`
			SenderFault bool   `xml:"SenderFault"`
		} `xml:"SendMessageBatchResult>BatchResultErrorEntry"`
	}

	if status != http.StatusOK || xml.Unmarshal(body, &batch) != nil || len(batch.Successful) != 1 ||
		batch.Successful[0].ID != "q1" || batch.Successful[0].AttributesMD5 == "" || len(batch.Failed) != 1 ||
		batch.Failed[0].ID != "q2" || batch.Failed[0].Code != "InvalidMessageContents" ||
		!batch.Failed[0].SenderFault {
		t.Fatalf("%d %s", status, body)
	}
}

func TestSQSDeadLetterQueue(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	dlqURL := createQueue(t, c, "orders-dlq", nil)
	const dlqARN = "arn:aws:sqs:us-east-1:000000000000:orders-dlq"

	var badValue *types.InvalidAttributeValue
	for _, policy := range []string{
		`{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:missing","maxReceiveCount":2}`,
		`{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:000000000000:orders-dlq","maxReceiveCount":2}`,
		`{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":0}`,
		`{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":2,"extra":true}`,
		`not json`,
	} {
		_, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName:  aws.String("orders"),
			Attributes: map[string]string{"RedrivePolicy": policy},
		})
		if !errors.As(err, &badValue) {
			t.Fatalf("policy %s accepted: %v", policy, err)
		}
	}

	// maxReceiveCount may be a string; a zero visibility timeout redelivers at once.
	queueURL := createQueue(t, c, "orders", map[string]string{
		"VisibilityTimeout": "0",
		"RedrivePolicy":     `{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":"2"}`,
	})

	attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll},
	})
	if err != nil || attrs.Attributes["QueueArn"] != "arn:aws:sqs:us-east-1:000000000000:orders" ||
		attrs.Attributes["RedrivePolicy"] != `{"deadLetterTargetArn":"`+dlqARN+`","maxReceiveCount":2}` ||
		attrs.Attributes["VisibilityTimeout"] != "0" || attrs.Attributes["CreatedTimestamp"] == "" {
		t.Fatal(attrs, err)
	}

	_, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &queueURL,
		Attributes: map[string]string{
			"RedrivePolicy": `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:orders","maxReceiveCount":2}`,
		},
	})
	if !errors.As(err, &badValue) {
		t.Fatalf("queue accepted itself as its dead-letter queue: %v", err)
	}

	_, err = c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: &queueURL, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNamePolicy},
	})
	var badName *types.InvalidAttributeName
	if !errors.As(err, &badName) {
		t.Fatalf("unsupported attribute requested: %v", err)
	}

	sent, err := c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("poison")})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		out, receiveErr := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
		if receiveErr != nil || len(out.Messages) != 1 {
			t.Fatal("message not redelivered", out, receiveErr)
		}
	}

	// The third receive exceeds maxReceiveCount and moves the message.
	out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(out.Messages) != 0 {
		t.Fatal("message delivered past maxReceiveCount", out, err)
	}

	moved, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &dlqURL})
	if err != nil || len(moved.Messages) != 1 || *moved.Messages[0].MessageId != *sent.MessageId {
		t.Fatal("message not moved to the dead-letter queue", moved, err)
	}

	// Clearing the policy stops redrive; counts cover visible, in-flight, and delayed messages.
	if _, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   &queueURL,
		Attributes: map[string]string{"RedrivePolicy": "", "VisibilityTimeout": "30"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, delay := range []int32{0, 0, 60} {
		if _, err = c.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: &queueURL, MessageBody: aws.String("m"), DelaySeconds: delay,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL}); err != nil {
		t.Fatal(err)
	}

	attrs, err = c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: &queueURL,
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameRedrivePolicy,
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	want := map[string]string{
		"ApproximateNumberOfMessages":           "1",
		"ApproximateNumberOfMessagesNotVisible": "1",
		"ApproximateNumberOfMessagesDelayed":    "1",
	}
	if err != nil || fmt.Sprint(attrs.Attributes) != fmt.Sprint(want) {
		t.Fatal(attrs, err)
	}

	// Query renders attributes as flattened Name/Value entries.
	status, _, body := queryCall(t, s, "/", url.Values{
		"Action": {"GetQueueAttributes"}, "QueueUrl": {queueURL}, "AttributeName.1": {"QueueArn"},
	}, nil)
	var queried struct {
		Attributes []struct{ Name, Value string } `xml:"GetQueueAttributesResult>Attribute"`
	}

	if status != http.StatusOK || xml.Unmarshal(body, &queried) != nil || len(queried.Attributes) != 1 ||
		queried.Attributes[0].Value != "arn:aws:sqs:us-east-1:000000000000:orders" {
		t.Fatalf("%d %s", status, body)
	}
}

func TestSQSFIFO(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()

	for name, attributes := range map[string]map[string]string{
		"orders":      {"FifoQueue": "true"},
		"orders.fifo": nil,
		"plain":       {"ContentBasedDeduplication": "true"},
	} {
		if _, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name, Attributes: attributes}); err == nil {
			t.Fatalf("%s %v accepted", name, attributes)
		}
	}

	queueURL := createQueue(t, c, "orders.fifo", map[string]string{"FifoQueue": "true"})
	standardURL := createQueue(t, c, "standard", nil)
	send := func(url, body, group, dedup string) (*sqs.SendMessageOutput, error) {
		in := &sqs.SendMessageInput{QueueUrl: &url, MessageBody: &body}
		if group != "" {
			in.MessageGroupId = &group
		}

		if dedup != "" {
			in.MessageDeduplicationId = &dedup
		}

		return c.SendMessage(ctx, in)
	}

	for _, tc := range []struct{ url, group, dedup string }{
		{queueURL, "", "d"},          // missing group
		{queueURL, "g", ""},          // no deduplication ID or content-based deduplication
		{standardURL, "g", "d"},      // FIFO parameters on a standard queue
		{queueURL, "bad group", "d"}, // spaces are not allowed
	} {
		if _, err := send(tc.url, "x", tc.group, tc.dedup); err == nil {
			t.Fatalf("send %+v accepted", tc)
		}
	}

	if _, err := c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: &queueURL, MessageBody: aws.String("x"), MessageGroupId: aws.String("g"),
		MessageDeduplicationId: aws.String("d"), DelaySeconds: 5,
	}); err == nil {
		t.Fatal("per-message delay accepted on a FIFO queue")
	}

	a, err := send(queueURL, "a", "g1", "d1")
	if err != nil || a.SequenceNumber == nil {
		t.Fatal(a, err)
	}

	b, err := send(queueURL, "b", "g1", "d2")
	if err != nil || *b.SequenceNumber <= *a.SequenceNumber {
		t.Fatal(b, err)
	}

	if _, err = send(queueURL, "c", "g2", "d3"); err != nil {
		t.Fatal(err)
	}
	// A repeated deduplication ID returns the original message and enqueues nothing.
	dup, err := send(queueURL, "a again", "g1", "d1")
	if err != nil || *dup.MessageId != *a.MessageId || *dup.SequenceNumber != *a.SequenceNumber {
		t.Fatal(dup, err)
	}

	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    &queueURL,
		MaxNumberOfMessages:         10,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil || len(received.Messages) != 3 {
		t.Fatal(received, err)
	}

	var bodies []string
	for _, msg := range received.Messages {
		bodies = append(bodies, *msg.Body)
	}

	first := received.Messages[0]
	if strings.Join(bodies, ",") != "a,b,c" || first.Attributes["MessageGroupId"] != "g1" ||
		first.Attributes["MessageDeduplicationId"] != "d1" || first.Attributes["SequenceNumber"] != *a.SequenceNumber {
		t.Fatal(bodies, first.Attributes)
	}

	// b is visible again, but a is still in flight, so group g1 stays blocked.
	if _, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: received.Messages[1].ReceiptHandle, VisibilityTimeout: 0,
	}); err != nil {
		t.Fatal(err)
	}

	blocked, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(blocked.Messages) != 0 {
		t.Fatal("message delivered while its group was in flight", blocked, err)
	}

	// Deleting a unblocks the group and wakes a waiting receiver.
	done := make(chan *sqs.ReceiveMessageOutput, 1)
	go func() {
		out, receiveErr := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 10})
		if receiveErr != nil {
			t.Error(receiveErr)
		}

		done <- out
	}()

	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if _, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: &queueURL, ReceiptHandle: first.ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	next := <-done
	if next == nil || len(next.Messages) != 1 || *next.Messages[0].Body != "b" || time.Since(start) > 5*time.Second {
		t.Fatal("deleting the in-flight message did not release the group", next)
	}

	// FifoQueue is fixed at creation; content-based deduplication hashes the body.
	var badName *types.InvalidAttributeName
	if _, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &queueURL, Attributes: map[string]string{"FifoQueue": "false"},
	}); !errors.As(err, &badName) {
		t.Fatalf("FifoQueue changed after creation: %v", err)
	}

	if _, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &queueURL, Attributes: map[string]string{"ContentBasedDeduplication": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	one, err := send(queueURL, "same", "g3", "")
	if err != nil {
		t.Fatal(err)
	}

	two, err := send(queueURL, "same", "g3", "")
	if err != nil || *two.MessageId != *one.MessageId {
		t.Fatal("content-based deduplication did not match", two, err)
	}

	attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll},
	})
	if err != nil || attrs.Attributes["FifoQueue"] != "true" ||
		attrs.Attributes["ContentBasedDeduplication"] != "true" {
		t.Fatal(attrs, err)
	}

	batch, err := c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: &queueURL,
		Entries: []types.SendMessageBatchRequestEntry{
			{Id: aws.String("x"), MessageBody: aws.String("x"), MessageGroupId: aws.String("g4")},
			{Id: aws.String("y"), MessageBody: aws.String("y")},
		},
	})
	if err != nil || len(batch.Successful) != 1 || batch.Successful[0].SequenceNumber == nil ||
		len(batch.Failed) != 1 || *batch.Failed[0].Code != "MissingParameter" {
		t.Fatal(batch, err)
	}

	// A dead-letter queue must match the source queue's type.
	var badValue *types.InvalidAttributeValue
	if _, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &standardURL,
		Attributes: map[string]string{
			"RedrivePolicy": `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:orders.fifo","maxReceiveCount":1}`,
		},
	}); !errors.As(err, &badValue) {
		t.Fatalf("standard queue accepted a FIFO dead-letter queue: %v", err)
	}
}

func TestSQSConcurrentReceivers(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "work", nil)
	const total = 60
	for i := range total {
		if _, err := c.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: &queueURL, MessageBody: aws.String(fmt.Sprint(i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				out, err := c.ReceiveMessage(
					ctx,
					&sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10},
				)
				if err != nil {
					t.Error(err)
					return
				}

				if len(out.Messages) == 0 {
					return
				}

				mu.Lock()
				for _, m := range out.Messages {
					seen[*m.Body]++
				}

				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	if len(seen) != total {
		t.Fatalf("received %d distinct messages", len(seen))
	}

	for body, n := range seen {
		if n != 1 {
			t.Fatalf("message %s claimed %d times", body, n)
		}
	}
}

type queryErrorXML struct {
	Error struct {
		Type, Code, Message string
	}

	RequestID string `xml:"RequestId"`
}

type receiveXML struct {
	Result struct {
		Messages []struct {
			MessageID              string `xml:"MessageId"`
			ReceiptHandle          string
			MD5OfBody              string
			Body                   string
			MD5OfMessageAttributes string
			Attributes             []struct{ Name, Value string } `xml:"Attribute"`
			MessageAttributes      []struct {
				Name  string
				Value struct{ StringValue, DataType string }
			} `xml:"MessageAttribute"`
		} `xml:"Message"`
	} `xml:"ReceiveMessageResult"`
}

func queryCall(
	t *testing.T,
	s *httptest.Server,
	path string,
	params url.Values,
	headers map[string]string,
) (status int, header http.Header, body []byte) {
	t.Helper()
	if params.Get("Version") == "" {
		params.Set("Version", "2012-11-05")
	}

	req, err := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(params.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	if body, err = io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, body
}

func TestSQSQueryProtocol(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()

	status, headers, body := queryCall(t, s, "/", url.Values{
		"Action":            {"CreateQueue"},
		"QueueName":         {"legacy"},
		"Attribute.1.Name":  {"VisibilityTimeout"},
		"Attribute.1.Value": {"45"},
	}, nil)
	var created struct {
		QueueURL  string `xml:"CreateQueueResult>QueueUrl"`
		RequestID string `xml:"ResponseMetadata>RequestId"`
	}

	if status != 200 || headers.Get("Content-Type") != "text/xml" || xml.Unmarshal(body, &created) != nil ||
		created.QueueURL != s.URL+"/000000000000/legacy" || created.RequestID != headers.Get("x-amzn-RequestId") ||
		!strings.Contains(string(body), `xmlns="http://queue.amazonaws.com/doc/2012-11-05/"`) {
		t.Fatalf("%d %s", status, body)
	}
	// JSON and Query share state and attributes.
	if again := createQueue(t, c, "legacy", map[string]string{"VisibilityTimeout": "45"}); again != created.QueueURL {
		t.Fatal(again)
	}

	// Legacy SDKs post to the queue URL; flattened message attributes use Name/Value entries.
	status, _, body = queryCall(t, s, "/000000000000/legacy", url.Values{
		"Action":                               {"SendMessage"},
		"QueueUrl":                             {created.QueueURL},
		"MessageBody":                          {"from query"},
		"MessageAttribute.1.Name":              {"color"},
		"MessageAttribute.1.Value.DataType":    {"String"},
		"MessageAttribute.1.Value.StringValue": {"blue"},
		"MessageAttribute.2.Name":              {"raw"},
		"MessageAttribute.2.Value.DataType":    {"Binary"},
		"MessageAttribute.2.Value.BinaryValue": {"AAEC"},
	}, nil)
	var sent struct {
		MessageID     string `xml:"SendMessageResult>MessageId"`
		BodyMD5       string `xml:"SendMessageResult>MD5OfMessageBody"`
		AttributesMD5 string `xml:"SendMessageResult>MD5OfMessageAttributes"`
	}

	if status != 200 || xml.Unmarshal(body, &sent) != nil || sent.AttributesMD5 == "" {
		t.Fatalf("%d %s", status, body)
	}

	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &created.QueueURL, MessageAttributeNames: []string{"All"},
	})
	if err != nil || len(received.Messages) != 1 || *received.Messages[0].MessageId != sent.MessageID ||
		*received.Messages[0].MD5OfBody != sent.BodyMD5 ||
		*received.Messages[0].MD5OfMessageAttributes != sent.AttributesMD5 ||
		string(received.Messages[0].MessageAttributes["raw"].BinaryValue) != "\x00\x01\x02" {
		t.Fatal(received, err)
	}

	if _, err = c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &created.QueueURL,
		MessageBody: aws.String("to query <&>"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"size": {DataType: aws.String("Number"), StringValue: aws.String("7")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	status, _, body = queryCall(t, s, "/", url.Values{
		"Action":                 {"ReceiveMessage"},
		"QueueUrl":               {created.QueueURL},
		"AttributeName.1":        {"All"},
		"MessageAttributeName.1": {"All"},
		"MaxNumberOfMessages":    {"10"},
	}, nil)
	var polled receiveXML
	if status != 200 || xml.Unmarshal(body, &polled) != nil || len(polled.Result.Messages) != 1 {
		t.Fatalf("%d %s", status, body)
	}

	got := polled.Result.Messages[0]
	if got.Body != "to query <&>" || len(got.Attributes) != 4 || len(got.MessageAttributes) != 1 ||
		got.MessageAttributes[0].Name != "size" || got.MessageAttributes[0].Value.StringValue != "7" {
		t.Fatalf("%s", body)
	}

	status, _, body = queryCall(t, s, "/", url.Values{
		"Action": {"DeleteMessage"}, "QueueUrl": {created.QueueURL}, "ReceiptHandle": {got.ReceiptHandle},
	}, nil)
	if status != 200 || !strings.Contains(string(body), "<DeleteMessageResponse") ||
		strings.Contains(string(body), "DeleteMessageResult") {
		t.Fatalf("%d %s", status, body)
	}

	for _, tc := range []struct {
		params  url.Values
		headers map[string]string
		name    string
		code    string
		status  int
	}{
		{
			name:   "missing queue",
			params: url.Values{"Action": {"GetQueueUrl"}, "QueueName": {"nope"}},
			code:   "AWS.SimpleQueueService.NonExistentQueue", status: 400,
		},
		{
			name:   "invalid receipt",
			params: url.Values{"Action": {"DeleteMessage"}, "QueueUrl": {created.QueueURL}, "ReceiptHandle": {"x"}},
			code:   "ReceiptHandleIsInvalid", status: 404,
		},
		{name: "unknown action", params: url.Values{"Action": {"PurgeQueue"}}, code: "InvalidAction", status: 400},
		{name: "missing action", params: url.Values{"QueueName": {"x"}}, code: "MissingAction", status: 400},
		{
			name:   "unknown parameter",
			params: url.Values{"Action": {"ListQueues"}, "Bogus": {"1"}},
			code:   "InvalidParameterValue", status: 400,
		},
		{
			name:   "list gap",
			params: url.Values{"Action": {"ReceiveMessage"}, "QueueUrl": {created.QueueURL}, "AttributeName.2": {"All"}},
			code:   "InvalidParameterValue", status: 400,
		},
		{
			name:   "wrong version",
			params: url.Values{"Action": {"ListQueues"}, "Version": {"2008-01-01"}},
			code:   "NoSuchVersion", status: 400,
		},
		{
			name:   "signed for another service",
			params: url.Values{"Action": {"ListQueues"}},
			headers: map[string]string{
				"Authorization": "AWS4-HMAC-SHA256 Credential=test/20260923/us-east-1/dynamodb/aws4_request",
			},
			code: "InvalidAction", status: 400,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, headers, body := queryCall(t, s, "/", tc.params, tc.headers)
			var e queryErrorXML
			if err := xml.Unmarshal(body, &e); err != nil || status != tc.status || e.Error.Code != tc.code ||
				e.Error.Type != "Sender" || headers.Get("Content-Type") != "text/xml" || e.RequestID == "" {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
}

func TestSQSJSONQueryCompatibleErrors(t *testing.T) {
	s := server(t)
	req, err := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(`{"QueueName":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.GetQueueUrl")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	var e struct {
		Type string `json:"__type"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 400 || e.Type != "QueueDoesNotExist" ||
		resp.Header.Get("X-Amzn-Query-Error") != "AWS.SimpleQueueService.NonExistentQueue;Sender" {
		t.Fatalf("%d %s %v", resp.StatusCode, e.Type, resp.Header)
	}

	// A JSON target with a form body is contradictory and fails before dispatch.
	req, err = http.NewRequest(http.MethodPost, s.URL, strings.NewReader("Action=ListQueues"))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Amz-Target", "AmazonSQS.ListQueues")
	resp, err = s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
		t.Fatalf("contradictory request: %d", resp.StatusCode)
	}
}

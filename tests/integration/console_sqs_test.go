package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func consoleSQSRequest(
	t *testing.T,
	s *httptest.Server,
	operation, account, region string,
	input map[string]any,
) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": "sqs", "operation": operation, "account": account, "region": region, "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		s.URL+consoleActionsPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.URL)
	return r
}

func TestConsoleSQSLifecycle(t *testing.T) {
	s := server(t)
	const account, region = "000000000000", "eu-west-1"
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		response := consoleResponse(t, s, consoleSQSRequest(t, s, operation, account, region, input), http.StatusOK)
		return response["output"].(map[string]any)
	}
	var capabilities map[string]any
	if internal(t, s, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["sqs"] != true {
		t.Fatalf("SQS capability = %+v", capabilities)
	}

	dlq := write("CreateQueue", map[string]any{"QueueName": "console-dlq"})["QueueUrl"].(string)
	dlqAttributes := write("GetQueueAttributes", map[string]any{"QueueUrl": dlq, "AttributeNames": []any{"All"}})
	arn := dlqAttributes["Attributes"].(map[string]any)["QueueArn"].(string)
	url := write("CreateQueue", map[string]any{"QueueName": "console-queue", "Attributes": map[string]any{
		"VisibilityTimeout": "30", "RedrivePolicy": `{"deadLetterTargetArn":"` + arn + `","maxReceiveCount":1}`,
	}, "Tags": map[string]any{"team": "console"}})["QueueUrl"].(string)
	write("SetQueueAttributes", map[string]any{"QueueUrl": url, "Attributes": map[string]any{
		"Policy": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sqs:*","Resource":"*"}]}`,
		"RedriveAllowPolicy": `{"redrivePermission":"allowAll"}`,
	}})
	write("TagQueue", map[string]any{"QueueUrl": url, "Tags": map[string]any{"env": "test"}})
	write("UntagQueue", map[string]any{"QueueUrl": url, "TagKeys": []any{"team"}})
	tags := write("ListQueueTags", map[string]any{"QueueUrl": url})["Tags"].(map[string]any)
	if len(tags) != 1 || tags["env"] != "test" {
		t.Fatalf("tags = %+v", tags)
	}

	binary := base64.StdEncoding.EncodeToString([]byte{0, 255, 128})
	sent := write(
		"SendMessage",
		map[string]any{"QueueUrl": url, "MessageBody": "hello", "MessageAttributes": map[string]any{
			"binary": map[string]any{"DataType": "Binary", "BinaryValue": binary},
		}},
	)
	receive := func(queue string, visibility int) []any {
		t.Helper()
		output := write("ReceiveMessage", map[string]any{
			"QueueUrl": queue, "MaxNumberOfMessages": 10,
			"WaitTimeSeconds": 0, "VisibilityTimeout": visibility, "MessageSystemAttributeNames": []any{"All"},
			"MessageAttributeNames": []any{"All"},
		})
		messages, _ := output["Messages"].([]any)
		return messages
	}
	messages := receive(url, 30)
	if len(messages) != 1 {
		t.Fatalf("messages = %+v", messages)
	}

	message := messages[0].(map[string]any)
	attr := message["MessageAttributes"].(map[string]any)["binary"].(map[string]any)
	if message["MessageId"] != sent["MessageId"] || message["Body"] != "hello" || attr["BinaryValue"] != binary {
		t.Fatalf("message = %+v", message)
	}

	write(
		"ChangeMessageVisibility",
		map[string]any{"QueueUrl": url, "ReceiptHandle": message["ReceiptHandle"], "VisibilityTimeout": 0},
	)
	if result := receive(url, 0); len(result) != 0 {
		t.Fatalf("message should have moved to DLQ: %+v", result)
	}

	sources := write("ListDeadLetterSourceQueues", map[string]any{"QueueUrl": dlq})["QueueUrls"].([]any)
	if len(sources) != 1 || sources[0] != url {
		t.Fatalf("sources = %+v", sources)
	}

	dlqMessages := receive(dlq, 30)
	if len(dlqMessages) != 1 {
		t.Fatalf("DLQ messages = %+v", dlqMessages)
	}

	write(
		"DeleteMessage",
		map[string]any{"QueueUrl": dlq, "ReceiptHandle": dlqMessages[0].(map[string]any)["ReceiptHandle"]},
	)
	task := write("StartMessageMoveTask", map[string]any{"SourceArn": arn, "MaxNumberOfMessagesPerSecond": 1})
	tasks := write("ListMessageMoveTasks", map[string]any{"SourceArn": arn, "MaxResults": 10})["Results"].([]any)
	if len(tasks) != 1 || task["TaskHandle"] == "" {
		t.Fatalf("tasks = %+v", tasks)
	}

	// Create a FIFO queue through the same bridge and check SDK-visible state.
	fifoURL := write("CreateQueue", map[string]any{"QueueName": "console.fifo", "Attributes": map[string]any{
		"FifoQueue": "true", "ContentBasedDeduplication": "true",
	}})["QueueUrl"].(string)
	one := write("SendMessage", map[string]any{"QueueUrl": fifoURL, "MessageBody": "fifo", "MessageGroupId": "group"})
	two := write("SendMessage", map[string]any{"QueueUrl": fifoURL, "MessageBody": "fifo", "MessageGroupId": "group"})
	if one["MessageId"] != two["MessageId"] || one["SequenceNumber"] == "" {
		t.Fatalf("FIFO deduplication: %+v, %+v", one, two)
	}

	options := sqsClient(s, "test").Options()
	options.Region = region
	client := sqs.New(options)
	output, err := client.GetQueueAttributes(
		context.Background(),
		&sqs.GetQueueAttributesInput{
			QueueUrl:       new(fifoURL),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll},
		},
	)
	if err != nil || output.Attributes["ApproximateNumberOfMessages"] != "1" {
		t.Fatalf("SDK queue attributes = %+v, %v", output, err)
	}

	// Scope is enforced even when a supplied queue URL names another account.
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(t, s, consoleSQSRequest(t, s, "GetQueueAttributes", scope.account, scope.region,
			map[string]any{"QueueUrl": url, "AttributeNames": []any{"All"}}), http.StatusBadRequest)
	}

	write("DeleteQueue", map[string]any{"QueueUrl": fifoURL})
	write("DeleteQueue", map[string]any{"QueueUrl": url})
	write("DeleteQueue", map[string]any{"QueueUrl": dlq})
}

func TestConsoleSQSValidationAndSignatures(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		input             map[string]any
		operation, region string
	}{
		{map[string]any{"QueueName": "invalid"}, "CreateQueue", ""},
		{map[string]any{"QueueName": "invalid"}, "CreateQueue", "../us-east-1"},
		{map[string]any{}, "AddPermission", "us-east-1"},
		{map[string]any{"QueueName": "invalid", "Unknown": true}, "CreateQueue", "us-east-1"},
	} {
		consoleResponse(
			t,
			s,
			consoleSQSRequest(t, s, tc.operation, "000000000000", tc.region, tc.input),
			http.StatusBadRequest,
		)
	}

	r := consoleSQSRequest(t, s, "CreateQueue", "000000000000", "us-east-1", map[string]any{"QueueName": "cross-site"})
	r.Header.Set("Origin", "https://evil.example")
	consoleResponse(t, s, r, http.StatusForbidden)
	verified := verifyingServer(t)
	for _, tc := range []struct {
		name, service, region, account string
		signed                         bool
		status                         int
	}{
		{"unsigned", "sqs", "us-east-1", "111111111111", false, http.StatusForbidden},
		{"wrong-service", "s3", "us-east-1", "111111111111", true, http.StatusForbidden},
		{"wrong-region", "sqs", "eu-west-1", "111111111111", true, http.StatusForbidden},
		{"wrong-account", "sqs", "us-east-1", "000000000000", true, http.StatusForbidden},
		{"valid", "sqs", "us-east-1", "111111111111", true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := consoleSQSRequest(
				t,
				verified,
				"CreateQueue",
				tc.account,
				"us-east-1",
				map[string]any{"QueueName": "signed-" + tc.name},
			)
			if tc.signed {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				sum := sha256.Sum256(body)
				credentials := aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"}
				err = v4.NewSigner().SignHTTP(context.Background(), credentials, req, hex.EncodeToString(sum[:]),
					tc.service, tc.region, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			consoleResponse(t, verified, req, tc.status)
		})
	}
}

func TestConsoleSQSPurge(t *testing.T) {
	s := server(t)
	client := sqsClient(s, "test")
	ctx := context.Background()
	url := createQueue(t, client, "purge", map[string]string{"VisibilityTimeout": "30"})
	for _, delay := range []int32{0, 0, 900} {
		if _, err := client.SendMessage(
			ctx,
			&sqs.SendMessageInput{QueueUrl: new(url), MessageBody: new("purge-me"), DelaySeconds: delay},
		); err != nil {
			t.Fatal(err)
		}
	}

	inFlight, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: new(url), MaxNumberOfMessages: 1})
	if err != nil || len(inFlight.Messages) != 1 {
		t.Fatalf("receive = %+v, %v", inFlight, err)
	}

	// Exercise the SDK operation first; the bridge must share its cooldown.
	if _, err = client.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: new(url)}); err != nil {
		t.Fatal(err)
	}

	output, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: new(url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible", "ApproximateNumberOfMessagesDelayed",
	} {
		if output.Attributes[key] != "0" {
			t.Fatalf("%s = %s", key, output.Attributes[key])
		}
	}

	if output.Attributes["VisibilityTimeout"] != "30" {
		t.Fatal("purge changed queue configuration")
	}

	result := consoleResponse(t, s, consoleSQSRequest(t, s, "PurgeQueue", "000000000000", "us-east-1",
		map[string]any{"QueueUrl": url}), http.StatusForbidden)
	if result["code"] != "AWS.SimpleQueueService.PurgeQueueInProgress" {
		t.Fatalf("repeat purge = %+v", result)
	}

	other := createQueue(t, client, "other-purge", nil)
	consoleResponse(t, s, consoleSQSRequest(t, s, "PurgeQueue", "000000000000", "us-east-1",
		map[string]any{"QueueUrl": other}), http.StatusOK)
}

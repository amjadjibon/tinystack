package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

// internal calls one of TinyStack's own endpoints and decodes a 200 JSON
// response into out.
func internal(t *testing.T, s *httptest.Server, method, path string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK && out != nil {
		if guardErr := json.NewDecoder(resp.Body).Decode(out); guardErr != nil {
			t.Fatal(guardErr)
		}
	}

	return resp.StatusCode
}

func inspect(t *testing.T, s *httptest.Server, method, path string) (int, []gateway.Record) {
	t.Helper()
	var out struct {
		Requests []gateway.Record `json:"requests"`
	}

	status := internal(t, s, method, path, &out)
	return status, out.Requests
}

func TestRequestInspector(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sqsClient(s, "team-b")
	createQueue(t, c, "inspected", nil)
	_, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("missing")})
	code(t, err, "AWS.SimpleQueueService.NonExistentQueue")

	status, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	if status != http.StatusOK || len(records) != 2 {
		t.Fatalf("status %d, records %+v", status, records)
	}

	failed, created := records[0], records[1]
	if failed.Operation != "GetQueueUrl" || failed.Status != http.StatusBadRequest ||
		failed.ErrorCode != "QueueDoesNotExist" || failed.ErrorMessage == "" ||
		!strings.Contains(failed.RequestBody, `"QueueName":"missing"`) ||
		!strings.Contains(failed.ResponseBody, "QueueDoesNotExist") {
		t.Fatalf("failed request: %+v", failed)
	}

	if created.Operation != "CreateQueue" || created.Service != "sqs" || created.Account != "111111111111" ||
		created.Region != "us-east-1" || created.Status != http.StatusOK || created.ErrorCode != "" ||
		created.RequestID == "" || created.Method != http.MethodPost {
		t.Fatalf("created request: %+v", created)
	}

	if !strings.Contains(created.ResponseBody, "inspected") || created.RequestSize != int64(len(created.RequestBody)) {
		t.Fatalf("created request: %+v", created)
	}

	if status, _ = inspect(t, s, http.MethodPost, "/_tinystack/requests"); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", status)
	}

	if status, _ = inspect(t, s, http.MethodGet, "/_tinystack/requests?service=sqs"); status != http.StatusBadRequest {
		t.Fatalf("query status %d", status)
	}

	if status, _ = inspect(t, s, http.MethodDelete, "/_tinystack/requests"); status != http.StatusNoContent {
		t.Fatalf("DELETE status %d", status)
	}

	if _, records = inspect(t, s, http.MethodGet, "/_tinystack/requests"); len(records) != 0 {
		t.Fatalf("records after clear: %+v", records)
	}
}

// maxKept is the number of records the inspector keeps.
const maxKept = 256

func TestRequestInspectorLimits(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	url := createQueue(t, c, "big", nil)
	body := strings.Repeat("x", 10000)
	for range maxKept + 4 {
		_, err := c.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: &url, MessageBody: &body})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	if len(records) != maxKept || records[len(records)-1].Operation != "SendMessage" {
		t.Fatalf("kept %d records", len(records))
	}

	last := records[0]
	if len(last.RequestBody) != 4096 || last.RequestSize <= 10000 {
		t.Fatalf("body %d bytes of %d", len(last.RequestBody), last.RequestSize)
	}
}

func TestMetrics(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sqsClient(s, "test")
	createQueue(t, c, "counted", nil)
	createQueue(t, c, "counted-too", nil)
	_, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("missing")})
	code(t, err, "AWS.SimpleQueueService.NonExistentQueue")
	status, _ := raw(t, s, "Bogus", "{}", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown operation status %d", status)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/_tinystack/metrics", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"# TYPE tinystack_requests_total counter",
		`tinystack_requests_total{service="sqs",operation="CreateQueue",status="200"} 2`,
		`tinystack_requests_total{service="sqs",operation="GetQueueUrl",status="400"} 1`,
		`tinystack_requests_total{service="dynamodb",operation="",status="400"} 1`,
		`tinystack_request_duration_seconds_count{service="sqs",operation="CreateQueue",status="200"} 2`,
	} {
		if !strings.Contains(string(text), want+"\n") {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
}

// TestRequestInspectorRedacts keeps no body of an operation whose model marks
// a value sensitive, such as the secret key AssumeRole returns.
func TestRequestInspectorRedacts(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	deployer := createRole(t, s, "test", "deployer", "000000000000", "")
	assumed, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &deployer, RoleSessionName: aws.String("ci"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	last := records[0]
	if last.Operation != "AssumeRole" || last.ResponseSize == 0 ||
		strings.Contains(last.ResponseBody, *assumed.Credentials.SecretAccessKey) ||
		!strings.Contains(last.RequestBody, "redacted") || !strings.Contains(last.ResponseBody, "redacted") {
		t.Fatalf("recorded %+v", last)
	}

	// Other operations keep their bodies.
	if created := records[1]; created.Operation != "CreateRole" || !strings.Contains(created.RequestBody, "deployer") {
		t.Fatalf("recorded %+v", created)
	}
}

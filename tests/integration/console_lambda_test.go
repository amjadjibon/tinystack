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
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func consoleLambdaRequest(
	t *testing.T, s *httptest.Server, service, operation, account, region string, input map[string]any,
) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": service, "operation": operation, "account": account, "region": region, "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		s.URL+consoleActionsPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.URL)
	return r
}

func TestConsoleLambdaLifecycleAndLogs(t *testing.T) {
	s := server(t)
	const account, region, name = "000000000000", "us-east-1", "console-function"
	write := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		out := consoleResponse(
			t,
			s,
			consoleLambdaRequest(t, s, service, operation, account, region, input),
			http.StatusOK,
		)
		return out["output"].(map[string]any)
	}
	var capabilities map[string]any
	if internal(t, s, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["lambda"] != true || capabilities["logs"] != true {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	code := zipOf(t, "app.py", "def echo(e, c): return e\n")
	created := write("lambda", "CreateFunction", map[string]any{
		"FunctionName": name, "Runtime": "python3.12", "Handler": "app.echo",
		"Role": "arn:aws:iam::000000000000:role/lambda", "Code": map[string]any{
			"ZipFile": base64.StdEncoding.EncodeToString(code),
		}, "Environment": map[string]any{"Variables": map[string]any{"STAGE": "dev"}},
		"Architectures": []any{"arm64"}, "Tags": map[string]any{"team": "console"},
	})
	arn := created["FunctionArn"].(string)
	if created["CodeSha256"] != sha(code) || created["CodeSize"] != float64(len(code)) {
		t.Fatalf("created = %+v", created)
	}

	updated := write("lambda", "UpdateFunctionConfiguration", map[string]any{
		"FunctionName": name, "RevisionId": created["RevisionId"], "Description": "From the console",
		"MemorySize": 256, "Timeout": 15, "Environment": map[string]any{"Variables": map[string]any{"STAGE": "test"}},
	})
	consoleResponse(t, s, consoleLambdaRequest(t, s, "lambda", "UpdateFunctionConfiguration", account, region,
		map[string]any{"FunctionName": name, "RevisionId": created["RevisionId"], "Timeout": 20}),
		http.StatusPreconditionFailed)
	write("lambda", "TagResource", map[string]any{"Resource": arn, "Tags": map[string]any{"env": "test"}})
	write("lambda", "UntagResource", map[string]any{"Resource": arn, "TagKeys": []any{"team"}})
	got := write("lambda", "GetFunction", map[string]any{"FunctionName": name})
	if got["Tags"].(map[string]any)["env"] != "test" ||
		got["Configuration"].(map[string]any)["MemorySize"] != float64(256) {
		t.Fatalf("function = %+v", got)
	}
	if len(write("lambda", "ListFunctions", map[string]any{})["Functions"].([]any)) != 1 {
		t.Fatal("function is missing from list")
	}

	invoked := write("lambda", "Invoke", map[string]any{
		"FunctionName": name, "InvocationType": "RequestResponse", "LogType": "Tail",
		"Payload": base64.StdEncoding.EncodeToString([]byte(`{"message":"hello 世界"}`)),
	})
	data, err := base64.StdEncoding.DecodeString(invoked["Payload"].(string))
	if err != nil || !bytes.Contains(data, []byte("hello 世界")) || !bytes.Contains(data, []byte(`"STAGE":"test"`)) {
		t.Fatalf("payload = %s, %v", data, err)
	}
	tail, err := base64.StdEncoding.DecodeString(invoked["LogResult"].(string))
	if err != nil || !bytes.Contains(tail, []byte("echo received")) || invoked["ExecutedVersion"] != "$LATEST" {
		t.Fatalf("invocation = %+v, %v", invoked, err)
	}

	logs := write("logs", "FilterLogEvents", map[string]any{"LogGroupName": "/aws/lambda/" + name, "Limit": 2})
	if len(logs["Events"].([]any)) != 2 || logs["NextToken"] == nil {
		t.Fatalf("logs = %+v", logs)
	}
	next := write("logs", "FilterLogEvents", map[string]any{
		"LogGroupName": "/aws/lambda/" + name, "NextToken": logs["NextToken"], "Limit": 100,
	})
	if len(next["Events"].([]any)) < 2 {
		t.Fatalf("remaining logs = %+v", next)
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "eu-west-1"}} {
		consoleResponse(t, s, consoleLambdaRequest(t, s, "lambda", "GetFunction", scope.account, scope.region,
			map[string]any{"FunctionName": name}), http.StatusNotFound)
		consoleResponse(t, s, consoleLambdaRequest(t, s, "logs", "FilterLogEvents", scope.account, scope.region,
			map[string]any{"LogGroupName": "/aws/lambda/" + name}), http.StatusBadRequest)
	}

	write("lambda", "UpdateFunctionConfiguration", map[string]any{
		"FunctionName": name, "Handler": "app.fail", "RevisionId": updated["RevisionId"],
		"Environment": map[string]any{"Variables": map[string]any{}},
	})
	failed := write("lambda", "Invoke", map[string]any{"FunctionName": name, "Payload": "e30="})
	if failed["FunctionError"] != "Unhandled" {
		t.Fatalf("failed invocation = %+v", failed)
	}
	dry := write("lambda", "Invoke", map[string]any{"FunctionName": name, "InvocationType": "DryRun"})
	if dry["StatusCode"] != float64(http.StatusNoContent) {
		t.Fatalf("dry run = %+v", dry)
	}

	// Code replacement preserves configuration and carries binary ZIP data intact.
	second := zipOf(t, "app.py", "def fail(e, c): raise ValueError('boom')\n")
	replaced := write("lambda", "UpdateFunctionCode", map[string]any{
		"FunctionName": name, "ZipFile": base64.StdEncoding.EncodeToString(second),
	})
	if replaced["CodeSha256"] != sha(second) {
		t.Fatalf("replacement = %+v", replaced)
	}
	sdk, err := lambdaClient(
		s,
		"test",
	).GetFunction(context.Background(), &sdklambda.GetFunctionInput{FunctionName: new(name)})
	if err != nil || aws.ToString(sdk.Configuration.Handler) != "app.fail" ||
		(sdk.Configuration.Environment != nil && len(sdk.Configuration.Environment.Variables) != 0) {
		t.Fatalf("SDK function = %+v, %v", sdk, err)
	}
	write("lambda", "DeleteFunction", map[string]any{"FunctionName": name})
}

func TestConsoleLambdaS3CodeAndTriggers(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	ctx := context.Background()
	write := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, s, consoleLambdaRequest(t, s, service, operation,
			"000000000000", "us-east-1", input), http.StatusOK)["output"].(map[string]any)
	}
	objects := s3Client(s, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("console-artifacts")})
	if err != nil {
		t.Fatal(err)
	}

	code := zipOf(t, "app.py", "def echo(e, c): return e\n")
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("console-artifacts"), Key: new("app.zip"), Body: bytes.NewReader(code),
	})
	if err != nil {
		t.Fatal(err)
	}

	created := write("lambda", "CreateFunction", map[string]any{
		"FunctionName": "connected", "Runtime": "python3.12", "Handler": "app.echo",
		"Role": "arn:aws:iam::000000000000:role/lambda",
		"Code": map[string]any{"S3Bucket": "console-artifacts", "S3Key": "app.zip"},
	})
	arn := created["FunctionArn"].(string)
	if created["CodeSha256"] != sha(code) {
		t.Fatal(created)
	}
	write("lambda", "UpdateFunctionCode", map[string]any{
		"FunctionName": "connected", "S3Bucket": "console-artifacts", "S3Key": "app.zip",
	})
	queues := sqsClient(s, "test")
	url, queue := queueARN(t, queues, "lambda-console-trigger")
	mapping := write("lambda", "CreateEventSourceMapping", map[string]any{
		"FunctionName": "connected", "EventSourceArn": queue, "Enabled": false, "BatchSize": 2,
		"FilterCriteria": map[string]any{"Filters": []any{map[string]any{"Pattern": `{"body":{"kind":["created"]}}`}}},
	})
	id := mapping["UUID"].(string)
	if mapping["State"] != "Disabled" {
		t.Fatal(mapping)
	}
	listed := write("lambda", "ListEventSourceMappings", map[string]any{"FunctionName": "connected"})
	if len(listed["EventSourceMappings"].([]any)) != 1 {
		t.Fatal(listed)
	}
	write("lambda", "UpdateEventSourceMapping", map[string]any{
		"UUID": id, "Enabled": true, "BatchSize": 1, "MaximumBatchingWindowInSeconds": 0,
		"FunctionResponseTypes": []any{"ReportBatchItemFailures"},
	})
	_, err = queues.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: new(url), MessageBody: new(`{"kind":"created"}`)})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		if call.fn.ARN != arn || !bytes.Contains(call.payload, []byte("created")) {
			t.Fatalf("queue invocation = %+v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SQS mapping did not invoke function")
	}
	write("lambda", "UpdateEventSourceMapping", map[string]any{"UUID": id, "Enabled": false})
	write("lambda", "DeleteEventSourceMapping", map[string]any{"UUID": id})
	topic := write("sns", "CreateTopic", map[string]any{"Name": "lambda-console-topic"})["TopicArn"].(string)
	sub := write("sns", "Subscribe", map[string]any{
		"TopicArn": topic, "Protocol": "lambda", "Endpoint": arn, "ReturnSubscriptionArn": true,
	})["SubscriptionArn"].(string)
	write("sns", "Publish", map[string]any{"TopicArn": topic, "Message": "console SNS"})
	select {
	case call := <-runtime.calls:
		if call.fn.ARN != arn || !bytes.Contains(call.payload, []byte("console SNS")) {
			t.Fatalf("SNS invocation = %+v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SNS did not invoke function")
	}
	write("sns", "Unsubscribe", map[string]any{"SubscriptionArn": sub})
	write("lambda", "DeleteFunction", map[string]any{"FunctionName": "connected"})
	image := write("lambda", "CreateFunction", map[string]any{
		"FunctionName": "container", "PackageType": "Image", "Role": "arn:aws:iam::000000000000:role/lambda",
		"Code": map[string]any{"ImageUri": "example.local/lambda:v1"},
	})
	if image["PackageType"] != "Image" {
		t.Fatal(image)
	}
	write(
		"lambda",
		"UpdateFunctionCode",
		map[string]any{"FunctionName": "container", "ImageUri": "example.local/lambda:v2"},
	)
	got := write("lambda", "GetFunction", map[string]any{"FunctionName": "container"})
	if got["Code"].(map[string]any)["ImageUri"] != "example.local/lambda:v2" {
		t.Fatal(got)
	}
}

func TestConsoleLambdaValidationAndSignatures(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		input                      map[string]any
		service, operation, region string
	}{
		{map[string]any{}, "lambda", "ListFunctions", ""},
		{map[string]any{}, "lambda", "ListFunctions", "../us-east-1"},
		{map[string]any{"Unknown": true}, "lambda", "ListFunctions", "us-east-1"},
		{map[string]any{}, "lambda", "PublishVersion", "us-east-1"},
		{map[string]any{"Code": map[string]any{"ZipFile": "not base64"}}, "lambda", "CreateFunction", "us-east-1"},
		{map[string]any{"DestinationName": "console-export"}, "logs", "CreateExportTask", "us-east-1"},
	} {
		consoleResponse(t, s, consoleLambdaRequest(t, s, tc.service, tc.operation,
			"000000000000", tc.region, tc.input), http.StatusBadRequest)
	}
	req := consoleLambdaRequest(t, s, "lambda", "ListFunctions", "000000000000", "us-east-1", map[string]any{})
	req.Header.Set("Origin", "https://evil.example")
	consoleResponse(t, s, req, http.StatusForbidden)
	verified := verifyingServer(t)
	for _, tc := range []struct {
		service, region, account string
		signed                   bool
		status                   int
	}{
		{"lambda", "us-east-1", "111111111111", false, http.StatusForbidden},
		{"sns", "us-east-1", "111111111111", true, http.StatusForbidden},
		{"lambda", "eu-west-1", "111111111111", true, http.StatusForbidden},
		{"lambda", "us-east-1", "000000000000", true, http.StatusForbidden},
		{"lambda", "us-east-1", "111111111111", true, http.StatusOK},
	} {
		r := consoleLambdaRequest(t, verified, "lambda", "ListFunctions", tc.account, "us-east-1", map[string]any{})
		if tc.signed {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			sum := sha256.Sum256(body)
			err = v4.NewSigner().
				SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"},
					r, hex.EncodeToString(sum[:]), tc.service, tc.region, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
		consoleResponse(t, verified, r, tc.status)
	}
	// CloudWatch reads have their own signed service boundary.
	r := consoleLambdaRequest(t, verified, "logs", "FilterLogEvents", "111111111111", "us-east-1",
		map[string]any{"LogGroupName": "/aws/lambda/missing"})
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	err = v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"},
		r, hex.EncodeToString(sum[:]), "logs", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	response := consoleResponse(t, verified, r, http.StatusBadRequest)
	if !strings.Contains(response["message"].(string), "does not exist") {
		t.Fatal(response)
	}
}

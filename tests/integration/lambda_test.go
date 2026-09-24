package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// fakeRuntime runs handlers in-process: "echo" returns the event and the
// function's environment, "fail" raises, and anything else returns null.
// Every invocation is sent to calls when it is set.
type fakeRuntime struct {
	calls    chan invocation
	released []string
	mu       sync.Mutex
}

type invocation struct {
	fn      *lambda.Function
	payload []byte
}

func (r *fakeRuntime) Invoke(_ context.Context, fn *lambda.Function, payload []byte) (lambda.Result, error) {
	if r.calls != nil {
		r.calls <- invocation{fn: fn, payload: payload}
	}

	switch fn.Handler {
	case "app.echo":
		body, err := json.Marshal(map[string]any{"event": json.RawMessage(payload), "env": fn.Environment})
		return lambda.Result{Payload: body}, err
	case "app.fail":
		return lambda.Result{Payload: []byte(`{"errorType":"ValueError","errorMessage":"boom"}`), Error: true}, nil
	default:
		return lambda.Result{Payload: []byte("null")}, nil
	}
}

func (r *fakeRuntime) Release(arn string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, arn)
}

func (*fakeRuntime) Close() {}

func lambdaClient(s *httptest.Server, key string) *sdklambda.Client {
	return sdklambda.New(
		sdklambda.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

// lambdaCode returns the error code and HTTP status of a Lambda error.
func lambdaCode(err error) (code string, status int) {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		code = apiErr.ErrorCode()
	}

	if httpErr, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		status = httpErr.HTTPStatusCode()
	}

	return code, status
}

// zipOf builds a deployment package holding one file.
func zipOf(t *testing.T, name, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	return b.Bytes()
}

func createFunction(t *testing.T, c *sdklambda.Client, name, handler string) *sdklambda.CreateFunctionOutput {
	t.Helper()
	out, err := c.CreateFunction(context.Background(), &sdklambda.CreateFunctionInput{
		FunctionName: aws.String(name),
		Runtime:      lambdatypes.RuntimePython312,
		Role:         aws.String("arn:aws:iam::000000000000:role/lambda"),
		Handler:      aws.String(handler),
		Code:         &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e, c): return e\n")},
		Environment:  &lambdatypes.Environment{Variables: map[string]string{"GREETING": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestLambdaFunctions(t *testing.T) {
	runtime := &fakeRuntime{}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	ctx := context.Background()
	created := createFunction(t, c, "hello", "app.echo")
	if aws.ToString(created.FunctionArn) != "arn:aws:lambda:us-east-1:000000000000:function:hello" ||
		created.State != lambdatypes.StateActive || aws.ToInt32(created.Timeout) != 3 ||
		aws.ToString(created.Version) != "$LATEST" || created.CodeSize == 0 ||
		created.Environment.Variables["GREETING"] != "hi" {
		t.Fatalf("%+v", created)
	}

	for _, name := range []string{"hello", "hello:$LATEST", "000000000000:function:hello", *created.FunctionArn} {
		got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String(name)})
		if err != nil || aws.ToString(got.Configuration.CodeSha256) != aws.ToString(created.CodeSha256) ||
			aws.ToString(got.Code.RepositoryType) != "S3" {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}

	_, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("hello"),
		Runtime:      lambdatypes.RuntimePython312,
		Role:         aws.String("arn:aws:iam::000000000000:role/lambda"),
		Handler:      aws.String("app.echo"),
		Code:         &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "")},
	})
	if _, ok := errors.AsType[*lambdatypes.ResourceConflictException](err); !ok {
		t.Fatalf("duplicate function: %v", err)
	}

	for name, in := range map[string]*sdklambda.CreateFunctionInput{
		"retired runtime": {Runtime: lambdatypes.RuntimePython27},
		"reserved env": {Environment: &lambdatypes.Environment{
			Variables: map[string]string{"AWS_REGION": "eu-west-1"},
		}},
		"not a zip": {Code: &lambdatypes.FunctionCode{ZipFile: []byte("plain")}},
	} {
		in.FunctionName, in.Role, in.Handler = aws.String("bad"), aws.String("arn:aws:iam::000000000000:role/r"),
			aws.String("app.echo")
		if in.Runtime == "" {
			in.Runtime = lambdatypes.RuntimePython312
		}

		if in.Code == nil {
			in.Code = &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "")}
		}

		if _, err = c.CreateFunction(ctx, in); !isType[*lambdatypes.InvalidParameterValueException](err) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// Modeled inputs TinyStack does not implement fail explicitly.
	_, err = c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("from-s3"),
		Role:         aws.String("arn:aws:iam::000000000000:role/r"),
		Runtime:      lambdatypes.RuntimePython312,
		Handler:      aws.String("app.echo"),
		Code:         &lambdatypes.FunctionCode{S3Bucket: aws.String("b"), S3Key: aws.String("k")},
	})
	if _, status := lambdaCode(err); status != http.StatusNotImplemented {
		t.Fatalf("S3 code: %v", err)
	}

	_, err = c.GetFunction(
		ctx,
		&sdklambda.GetFunctionInput{FunctionName: aws.String("hello"), Qualifier: aws.String("1")},
	)
	if _, status := lambdaCode(err); status != http.StatusNotImplemented {
		t.Fatalf("qualifier: %v", err)
	}

	missing, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String("nope")})
	notFound, ok := errors.AsType[*lambdatypes.ResourceNotFoundException](err)
	if !ok || !strings.Contains(aws.ToString(notFound.Message), "function:nope") {
		t.Fatalf("%+v %v", missing, err)
	}

	// Another account's function is not reachable by ARN.
	_, err = lambdaClient(s, "team-b").GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: created.FunctionArn})
	if code, status := lambdaCode(err); code != "AccessDeniedException" || status != http.StatusForbidden {
		t.Fatalf("cross-account: %v", err)
	}

	createFunction(t, c, "another", "app.echo")
	page, err := c.ListFunctions(ctx, &sdklambda.ListFunctionsInput{MaxItems: aws.Int32(1)})
	if err != nil || len(page.Functions) != 1 || aws.ToString(page.Functions[0].FunctionName) != "another" {
		t.Fatalf("%+v %v", page, err)
	}

	page, err = c.ListFunctions(ctx, &sdklambda.ListFunctionsInput{Marker: page.NextMarker})
	if err != nil || len(page.Functions) != 1 || aws.ToString(page.Functions[0].FunctionName) != "hello" ||
		page.NextMarker != nil {
		t.Fatalf("%+v %v", page, err)
	}

	if _, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: aws.String("hello")}); err != nil {
		t.Fatal(err)
	}

	runtime.mu.Lock()
	released := strings.Join(runtime.released, ",")
	runtime.mu.Unlock()
	if released != *created.FunctionArn {
		t.Fatalf("released %s", released)
	}

	_, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: aws.String("hello")})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("second delete: %v", err)
	}
}

func isType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

func TestLambdaInvoke(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "echo", "app.echo")
	createFunction(t, c, "fail", "app.fail")
	out, err := c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("echo"), Payload: []byte(`{"n":1}`)})
	if err != nil || out.StatusCode != 200 || out.FunctionError != nil {
		t.Fatalf("%+v %v", out, err)
	}

	var echoed struct {
		Event map[string]int
		Env   map[string]string
	}
	if err = json.Unmarshal(out.Payload, &echoed); err != nil || echoed.Event["n"] != 1 ||
		echoed.Env["GREETING"] != "hi" || echoed.Env["AWS_LAMBDA_FUNCTION_NAME"] != "echo" ||
		echoed.Env["AWS_ACCESS_KEY_ID"] != "test" {
		t.Fatalf("%s %v", out.Payload, err)
	}

	call := <-runtime.calls
	if call.fn.Timeout != 3*time.Second || call.fn.Runtime != "python3.12" || call.fn.Region != "us-east-1" {
		t.Fatalf("%+v", call.fn)
	}

	// An empty payload reaches the function as an empty object.
	if out, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("echo")}); err != nil ||
		!strings.Contains(string(out.Payload), `"event":{}`) {
		t.Fatalf("%s %v", out.Payload, err)
	}

	<-runtime.calls
	out, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("fail")})
	if err != nil || aws.ToString(out.FunctionError) != "Unhandled" || !strings.Contains(string(out.Payload), "boom") {
		t.Fatalf("%+v %v", out, err)
	}

	<-runtime.calls
	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("echo"), Payload: []byte("{bad")})
	if !isType[*lambdatypes.InvalidRequestContentException](err) {
		t.Fatalf("bad payload: %v", err)
	}

	out, err = c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName:   aws.String("echo"),
		InvocationType: lambdatypes.InvocationTypeDryRun,
	})
	if err != nil || out.StatusCode != http.StatusNoContent {
		t.Fatalf("%+v %v", out, err)
	}

	out, err = c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName:   aws.String("echo"),
		InvocationType: lambdatypes.InvocationTypeEvent,
		Payload:        []byte(`{"async":true}`),
	})
	if err != nil || out.StatusCode != http.StatusAccepted {
		t.Fatalf("%+v %v", out, err)
	}

	select {
	case call = <-runtime.calls:
		if string(call.payload) != `{"async":true}` {
			t.Fatalf("%s", call.payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("async invocation did not run")
	}

	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("missing")})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("missing function: %v", err)
	}

	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: aws.String("echo"),
		LogType:      lambdatypes.LogTypeTail,
	})
	if _, status := lambdaCode(err); status != http.StatusNotImplemented {
		t.Fatalf("log tail: %v", err)
	}
}

// TestLambdaRESTWire checks the REST JSON envelope over raw HTTP.
func TestLambdaRESTWire(t *testing.T) {
	s := server(t)
	req, err := http.NewRequest(http.MethodGet, s.URL+"/2015-03-31/functions/missing", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/lambda/aws4_request, "+
		"SignedHeaders=host, Signature=x")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Amzn-ErrorType") != "ResourceNotFoundException" ||
		!strings.Contains(string(body), `"Message":"Function arn:aws:lambda:us-east-1:000000000000:function:missing`) ||
		!strings.Contains(string(body), `"Type":"User"`) {
		t.Fatal(resp.StatusCode, resp.Header, string(body))
	}
}

func TestLambdaSQSEventSource(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "worker", "app.echo")
	queueURL, arn := queueARN(t, q, "jobs")
	mapping, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: aws.String(arn),
		FunctionName:   aws.String("worker"),
		BatchSize:      aws.Int32(5),
	})
	if err != nil || aws.ToString(mapping.State) != "Enabled" || aws.ToInt32(mapping.BatchSize) != 5 ||
		aws.ToString(mapping.FunctionArn) != "arn:aws:lambda:us-east-1:000000000000:function:worker" {
		t.Fatalf("%+v %v", mapping, err)
	}

	_, err = q.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &queueURL,
		MessageBody: aws.String("job-1"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"kind": {DataType: aws.String("String"), StringValue: aws.String("email")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var call invocation
	select {
	case call = <-runtime.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("event source mapping did not invoke the function")
	}

	var event struct {
		Records []struct {
			Attributes        map[string]string         `json:"attributes"`
			MessageAttributes map[string]map[string]any `json:"messageAttributes"`
			MessageID         string                    `json:"messageId"`
			Body              string                    `json:"body"`
			EventSource       string                    `json:"eventSource"`
			EventSourceARN    string                    `json:"eventSourceARN"`
			AWSRegion         string                    `json:"awsRegion"`
		}
	}
	if err = json.Unmarshal(call.payload, &event); err != nil || len(event.Records) != 1 {
		t.Fatalf("%s %v", call.payload, err)
	}

	record := event.Records[0]
	if record.Body != "job-1" || record.EventSource != "aws:sqs" || record.EventSourceARN != arn ||
		record.AWSRegion != "us-east-1" || record.Attributes["ApproximateReceiveCount"] != "1" ||
		record.MessageAttributes["kind"]["stringValue"] != "email" {
		t.Fatalf("%s", call.payload)
	}

	// Processed messages are deleted.
	deadline := time.Now().Add(5 * time.Second)
	for {
		attrs, attrErr := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       &queueURL,
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
		})
		if attrErr != nil {
			t.Fatal(attrErr)
		}

		if attrs.Attributes["ApproximateNumberOfMessages"] == "0" &&
			attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("message not deleted: %v", attrs.Attributes)
		}

		time.Sleep(20 * time.Millisecond)
	}

	got, err := c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: mapping.UUID})
	if err != nil || aws.ToString(got.LastProcessingResult) != "OK" {
		t.Fatalf("%+v %v", got, err)
	}

	_, err = c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: aws.String(arn),
		FunctionName:   aws.String("worker"),
	})
	if !isType[*lambdatypes.ResourceConflictException](err) {
		t.Fatalf("duplicate mapping: %v", err)
	}

	_, err = c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: aws.String("arn:aws:sqs:us-east-1:000000000000:missing"),
		FunctionName:   aws.String("worker"),
	})
	if !isType[*lambdatypes.InvalidParameterValueException](err) {
		t.Fatalf("missing queue: %v", err)
	}

	list, err := c.ListEventSourceMappings(ctx, &sdklambda.ListEventSourceMappingsInput{
		FunctionName: aws.String("worker"),
	})
	if err != nil || len(list.EventSourceMappings) != 1 {
		t.Fatalf("%+v %v", list, err)
	}

	deleted, err := c.DeleteEventSourceMapping(ctx, &sdklambda.DeleteEventSourceMappingInput{UUID: mapping.UUID})
	if err != nil || aws.ToString(deleted.State) != "Deleting" {
		t.Fatalf("%+v %v", deleted, err)
	}

	_, err = c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: mapping.UUID})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("deleted mapping: %v", err)
	}
}

// TestLambdaSQSFailuresRedrive checks that failed batches stay on the queue so
// its redrive policy moves them to the dead-letter queue.
func TestLambdaSQSFailuresRedrive(t *testing.T) {
	runtime := &fakeRuntime{}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "broken", "app.fail")
	dlqURL, dlqARN := queueARN(t, q, "jobs-dlq")
	queueURL := createQueue(t, q, "jobs", map[string]string{
		"VisibilityTimeout": "0",
		"RedrivePolicy":     `{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":"1"}`,
	})
	arn := "arn:aws:sqs:us-east-1:000000000000:jobs"
	if _, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: aws.String(arn),
		FunctionName:   aws.String("broken"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("x")},
	); err != nil {
		t.Fatal(err)
	}

	out, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &dlqURL, WaitTimeSeconds: 5})
	if err != nil || len(out.Messages) != 1 || aws.ToString(out.Messages[0].Body) != "x" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestSNSDeliversToLambda(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	n := snsClient(s, "test")
	ctx := context.Background()
	fn := createFunction(t, c, "notify", "app.echo")
	topic, err := n.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("events")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = n.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn:   topic.TopicArn,
		Protocol:   aws.String("lambda"),
		Endpoint:   fn.FunctionArn,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	if err == nil {
		t.Fatal("raw delivery to Lambda accepted")
	}

	sub, err := n.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn,
		Protocol: aws.String("lambda"),
		Endpoint: fn.FunctionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = n.Publish(ctx, &sns.PublishInput{
		TopicArn:         topic.TopicArn,
		MessageStructure: aws.String("json"),
		Message:          aws.String(`{"default":"plain","lambda":"for functions"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	var call invocation
	select {
	case call = <-runtime.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("SNS did not invoke the function")
	}

	var event struct {
		Records []struct {
			Sns struct {
				MessageAttributes       map[string]any
				Subject                 *string
				Type, TopicArn, Message string
			}
			EventSource          string
			EventSubscriptionArn string
		}
	}
	if err = json.Unmarshal(call.payload, &event); err != nil || len(event.Records) != 1 {
		t.Fatalf("%s %v", call.payload, err)
	}

	record := event.Records[0]
	if record.EventSource != "aws:sns" || record.EventSubscriptionArn != aws.ToString(sub.SubscriptionArn) ||
		record.Sns.Message != "for functions" || record.Sns.TopicArn != aws.ToString(topic.TopicArn) ||
		record.Sns.Subject != nil || record.Sns.MessageAttributes == nil {
		t.Fatalf("%s", call.payload)
	}
}

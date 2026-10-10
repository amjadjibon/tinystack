package integration_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// persistent starts a server on the SQLite store in dir. stop shuts the server
// down and closes the store, as app.Run does on exit.
func persistent(t *testing.T, dir string, runtime *fakeRuntime) (s *httptest.Server, stop func()) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	s, stopServer := serverStore(t, cfg, runtime, store)
	return s, func() {
		stopServer()
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
}

func TestPersistentRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: "1"}}
	s, stop := persistent(t, dir, &fakeRuntime{})
	tables := client(s, "test", "us-east-1")
	_, err := create(ctx, tables, "orders")
	if err != nil {
		t.Fatal(err)
	}

	item := map[string]types.AttributeValue{"PK": key["PK"], "Total": &types.AttributeValueMemberN{Value: "42"}}
	if _, err = tables.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: item}); err != nil {
		t.Fatal(err)
	}

	queues := sqsClient(s, "test")
	jobs := createQueue(t, queues, "jobs", nil)
	_, err = queues.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &jobs, MessageBody: aws.String("kept")})
	if err != nil {
		t.Fatal(err)
	}

	objects := s3Client(s, "test", "us-east-1")
	if _, err = objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("files")}); err != nil {
		t.Fatal(err)
	}

	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("files"), Key: aws.String("a.txt"), Body: strings.NewReader("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, workArn := queueARN(t, queues, "work")
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "echo", "app.echo")
	_, err = functions.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: aws.String("echo"), EventSourceArn: &workArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	runtime := &fakeRuntime{calls: make(chan invocation, 1)}
	s, stop = persistent(t, dir, runtime)
	t.Cleanup(stop)
	tables = client(s, "test", "us-east-1")
	got, err := tables.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("orders"), Key: key})
	if err != nil {
		t.Fatal(err)
	}

	if total, ok := got.Item["Total"].(*types.AttributeValueMemberN); !ok || total.Value != "42" {
		t.Fatalf("item after restart: %+v", got.Item)
	}

	queues = sqsClient(s, "test")
	url, err := queues.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("jobs")})
	if err != nil {
		t.Fatal(err)
	}

	received, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: url.QueueUrl})
	if err != nil || len(received.Messages) != 1 || *received.Messages[0].Body != "kept" {
		t.Fatalf("messages after restart: %+v, %v", received, err)
	}

	object, err := s3Client(s, "test", "us-east-1").GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("files"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil || string(body) != "hello" {
		t.Fatalf("object after restart: %q, %v", body, err)
	}

	// The reloaded mapping polls again without being recreated.
	work, err := queues.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("work")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = queues.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: work.QueueUrl, MessageBody: aws.String("resumed")})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		if call.fn.Name != "echo" || !strings.Contains(string(call.payload), "resumed") {
			t.Fatalf("invocation %s: %s", call.fn.Name, call.payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the mapping did not resume")
	}
}

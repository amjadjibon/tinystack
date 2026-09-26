package integration_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// partialRuntime records each batch it receives by record label (an SQS body
// or a DynamoDB key) and, the first time it sees a record whose label contains
// "fail", reports it with ReportBatchItemFailures.
type partialRuntime struct {
	seen    map[string]bool
	batches [][]string
	mu      sync.Mutex
}

func (r *partialRuntime) Invoke(_ context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event struct {
		Records []struct {
			Dynamodb *struct {
				Keys           map[string]map[string]string
				SequenceNumber string
			} `json:"dynamodb"`
			MessageID string `json:"messageId"`
			Body      string `json:"body"`
		}
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var labels []string
	failures := []map[string]string{}
	for _, record := range event.Records {
		label, id := record.Body, record.MessageID
		if record.Dynamodb != nil {
			label, id = record.Dynamodb.Keys["PK"]["S"], record.Dynamodb.SequenceNumber
		}

		labels = append(labels, label)
		if strings.Contains(label, "fail") && !r.seen[label] {
			failures = append(failures, map[string]string{"itemIdentifier": id})
		}

		r.seen[label] = true
	}

	r.batches = append(r.batches, labels)
	body, err := json.Marshal(map[string]any{"batchItemFailures": failures})
	return lambda.Result{Payload: body}, err
}

func (*partialRuntime) Release(string) {}

func (*partialRuntime) Close() {}

func (r *partialRuntime) received() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.batches)
}

func TestLambdaSQSMappingOptions(t *testing.T) {
	runtime := &partialRuntime{seen: map[string]bool{}}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "worker", "app.batch")
	queueURL := createQueue(t, q, "jobs", map[string]string{"VisibilityTimeout": "2"})
	arn := "arn:aws:sqs:us-east-1:000000000000:jobs"
	filters := &lambdatypes.FilterCriteria{Filters: []lambdatypes.Filter{
		{Pattern: aws.String(`{"body":{"kind":["job"]}}`)},
		{Pattern: aws.String(`{"body":["plain"]}`)},
	}}

	for name, in := range map[string]*sdklambda.CreateEventSourceMappingInput{
		"window":          {MaximumBatchingWindowInSeconds: aws.Int32(301)},
		"response type":   {FunctionResponseTypes: []lambdatypes.FunctionResponseType{"Other"}},
		"large batch":     {BatchSize: aws.Int32(20)},
		"too large batch": {BatchSize: aws.Int32(10001), MaximumBatchingWindowInSeconds: aws.Int32(1)},
		"pattern": {FilterCriteria: &lambdatypes.FilterCriteria{
			Filters: []lambdatypes.Filter{{Pattern: aws.String("{")}},
		}},
		"filters": {FilterCriteria: &lambdatypes.FilterCriteria{
			Filters: slices.Repeat([]lambdatypes.Filter{{Pattern: aws.String(`{"body":["x"]}`)}}, 6),
		}},
	} {
		in.EventSourceArn, in.FunctionName = aws.String(arn), aws.String("worker")
		if _, err := c.CreateEventSourceMapping(ctx, in); !isType[*lambdatypes.InvalidParameterValueException](err) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	mapping, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn:                 aws.String(arn),
		FunctionName:                   aws.String("worker"),
		BatchSize:                      aws.Int32(20),
		MaximumBatchingWindowInSeconds: aws.Int32(1),
		FunctionResponseTypes: []lambdatypes.FunctionResponseType{
			lambdatypes.FunctionResponseTypeReportBatchItemFailures,
		},
		FilterCriteria: filters,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: mapping.UUID})
	if err != nil || aws.ToInt32(got.BatchSize) != 20 || aws.ToInt32(got.MaximumBatchingWindowInSeconds) != 1 ||
		!slices.Equal(
			got.FunctionResponseTypes,
			mapping.FunctionResponseTypes,
		) || len(got.FunctionResponseTypes) != 1 ||
		got.FilterCriteria == nil || len(got.FilterCriteria.Filters) != 2 ||
		aws.ToString(got.FilterCriteria.Filters[1].Pattern) != `{"body":["plain"]}` {
		t.Fatalf("got %+v, %v", got, err)
	}

	// Messages sent one by one within the window arrive in one batch; the one
	// no filter matches is deleted without an invocation.
	for _, body := range []string{`{"kind":"job","id":"a"}`, `{"kind":"other"}`, `{"kind":"job","id":"fail"}`, "plain"} {
		if _, err = q.SendMessage(
			ctx,
			&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String(body)},
		); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "the retried message", func() bool { return len(runtime.received()) >= 2 })
	batches := runtime.received()
	first, second := slices.Clone(batches[0]), batches[1]
	slices.Sort(first)
	if !slices.Equal(first, []string{"plain", `{"kind":"job","id":"a"}`, `{"kind":"job","id":"fail"}`}) ||
		!slices.Equal(second, []string{`{"kind":"job","id":"fail"}`}) {
		t.Fatalf("batches %q", batches)
	}

	// Every message is done: processed, or filtered out and deleted.
	waitFor(t, "an empty queue", func() bool {
		attrs, attrErr := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: &queueURL,
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		return attrErr == nil && attrs.Attributes["ApproximateNumberOfMessages"] == "0" &&
			attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
	})

	if n := len(runtime.received()); n != 2 {
		t.Fatalf("%d batches", n)
	}
}

func TestLambdaStreamMappingOptions(t *testing.T) {
	runtime := &partialRuntime{seen: map[string]bool{}}
	s := serverWith(t, runtime)
	c := client(s, "test", "us-east-1")
	lc := lambdaClient(s, "test")
	ctx := context.Background()
	arn := streamTable(t, c, "events", types.StreamViewTypeNewImage)
	createFunction(t, lc, "indexer", "app.batch")
	for _, id := range []string{"a", "fail", "c", "a"} {
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("events"), Item: key(id)}); err != nil {
			t.Fatal(err)
		}
	}

	// Only inserts pass the filter, so the second write of a is skipped. The
	// failed record and those after it are retried.
	_, err := lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn:                 &arn,
		FunctionName:                   aws.String("indexer"),
		StartingPosition:               lambdatypes.EventSourcePositionTrimHorizon,
		MaximumBatchingWindowInSeconds: aws.Int32(1),
		FunctionResponseTypes: []lambdatypes.FunctionResponseType{
			lambdatypes.FunctionResponseTypeReportBatchItemFailures,
		},
		FilterCriteria: &lambdatypes.FilterCriteria{Filters: []lambdatypes.Filter{
			{Pattern: aws.String(`{"eventName":["INSERT"]}`)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the retried records", func() bool { return len(runtime.received()) >= 2 })
	if batches := runtime.received(); !slices.Equal(batches[0], []string{"a", "fail", "c"}) ||
		!slices.Equal(batches[1], []string{"fail", "c"}) {
		t.Fatalf("batches %q", batches)
	}
}

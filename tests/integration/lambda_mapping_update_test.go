package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestLambdaUpdateQueueMapping(t *testing.T) {
	runtime := &partialRuntime{seen: map[string]bool{}}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "worker", "app.batch")
	createFunction(t, c, "other", "app.batch")
	queueURL := createQueue(t, q, "jobs", nil)
	arn := "arn:aws:sqs:us-east-1:000000000000:jobs"
	mapping, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: aws.String(arn), FunctionName: aws.String("worker"), Enabled: aws.Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, in := range map[string]*sdklambda.UpdateEventSourceMappingInput{
		"large batch":   {BatchSize: aws.Int32(20)},
		"retries":       {MaximumRetryAttempts: aws.Int32(3)},
		"window":        {MaximumBatchingWindowInSeconds: aws.Int32(301)},
		"response type": {FunctionResponseTypes: []lambdatypes.FunctionResponseType{"Other"}},
	} {
		in.UUID = mapping.UUID
		if _, err = c.UpdateEventSourceMapping(ctx, in); !isType[*lambdatypes.InvalidParameterValueException](err) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	_, err = c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: aws.String("00000000-0000-4000-8000-000000000000"), Enabled: aws.Bool(true),
	})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("missing mapping: %v", err)
	}

	_, err = c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: mapping.UUID, FunctionName: aws.String("missing"),
	})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("missing function: %v", err)
	}

	// A message sent while the mapping is disabled waits for it.
	if _, err = q.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: &queueURL, MessageBody: aws.String(`{"kind":"skip"}`),
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID:                           mapping.UUID,
		FunctionName:                   aws.String("other"),
		Enabled:                        aws.Bool(true),
		BatchSize:                      aws.Int32(20),
		MaximumBatchingWindowInSeconds: aws.Int32(1),
		FilterCriteria: &lambdatypes.FilterCriteria{Filters: []lambdatypes.Filter{
			{Pattern: aws.String(`{"body":{"kind":["job"]}}`)},
		}},
	})
	if err != nil || aws.ToString(updated.State) != "Enabled" || aws.ToInt32(updated.BatchSize) != 20 ||
		!strings.HasSuffix(aws.ToString(updated.FunctionArn), ":function:other") ||
		len(updated.FilterCriteria.Filters) != 1 {
		t.Fatalf("updated %+v, %v", updated, err)
	}

	if _, err = q.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: &queueURL, MessageBody: aws.String(`{"kind":"job"}`),
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the filtered batch", func() bool { return len(runtime.received()) == 1 })
	if batches := runtime.received(); !slices.Equal(batches[0], []string{`{"kind":"job"}`}) {
		t.Fatalf("batches %q", batches)
	}

	// Clearing the filters keeps the other settings.
	cleared, err := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: mapping.UUID, FilterCriteria: &lambdatypes.FilterCriteria{},
	})
	if err != nil || cleared.FilterCriteria != nil || aws.ToInt32(cleared.BatchSize) != 20 {
		t.Fatalf("cleared %+v, %v", cleared, err)
	}

	// Another mapping of the queue cannot be pointed at the same function.
	second, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: aws.String(arn), FunctionName: aws.String("worker"), Enabled: aws.Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: second.UUID, FunctionName: aws.String("other"),
	})
	if !isType[*lambdatypes.ResourceConflictException](err) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestLambdaUpdateStreamMapping(t *testing.T) {
	runtime := &partialRuntime{seen: map[string]bool{}}
	s := serverWith(t, runtime)
	c := client(s, "test", "us-east-1")
	lc := lambdaClient(s, "test")
	ctx := context.Background()
	arn := streamTable(t, c, "events", types.StreamViewTypeNewImage)
	createFunction(t, lc, "indexer", "app.batch")
	put := func(id string) {
		t.Helper()
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("events"), Item: key(id)}); err != nil {
			t.Fatal(err)
		}
	}

	put("a")
	mapping, err := lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &arn, FunctionName: aws.String("indexer"),
		StartingPosition: lambdatypes.EventSourcePositionTrimHorizon,
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the first batch", func() bool { return len(runtime.received()) == 1 })
	updated, err := lc.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: mapping.UUID, BatchSize: aws.Int32(5), MaximumRetryAttempts: aws.Int32(2),
	})
	if err != nil || aws.ToInt32(updated.BatchSize) != 5 || aws.ToInt32(updated.MaximumRetryAttempts) != 2 ||
		updated.StartingPosition != lambdatypes.EventSourcePositionTrimHorizon {
		t.Fatalf("updated %+v, %v", updated, err)
	}

	_, err = lc.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: mapping.UUID, BatchSize: aws.Int32(10001),
	})
	if !isType[*lambdatypes.InvalidParameterValueException](err) {
		t.Fatalf("large batch: %v", err)
	}

	// The updated mapping resumes after the record it already processed.
	put("b")
	waitFor(t, "the second batch", func() bool { return len(runtime.received()) == 2 })
	if batches := runtime.received(); !slices.Equal(batches[1], []string{"b"}) {
		t.Fatalf("batches %q", batches)
	}
}

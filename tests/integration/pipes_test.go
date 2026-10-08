package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/pipes"
	pipetypes "github.com/aws/aws-sdk-go-v2/service/pipes/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const pipeRole = "arn:aws:iam::000000000000:role/pipes"

func pipesClient(s *httptest.Server) *pipes.Client {
	return pipes.New(pipes.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// sqsPipe is a CreatePipe request from one queue to another.
func sqsPipe(name, source, target string) *pipes.CreatePipeInput {
	return &pipes.CreatePipeInput{
		Name: aws.String(name), RoleArn: aws.String(pipeRole), Source: aws.String(source), Target: aws.String(target),
	}
}

// receivePipeBody waits for a message on the queue and returns its body.
func receivePipeBody(t *testing.T, c *sqs.Client, queueURL string) string {
	t.Helper()
	var body string
	waitFor(t, "a message on "+queueURL, func() bool {
		out, err := c.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, MaxNumberOfMessages: 10,
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(out.Messages) == 0 {
			return false
		}

		body = aws.ToString(out.Messages[0].Body)

		return true
	})

	return body
}

func TestPipesLifecycle(t *testing.T) {
	s := server(t)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	_, source := queueARN(t, q, "source")
	_, target := queueARN(t, q, "target")

	in := sqsPipe("orders", source, target)
	in.Description = aws.String("order pipe")
	in.Tags = map[string]string{"team": "core"}
	created, err := c.CreatePipe(ctx, in)
	if err != nil || aws.ToString(created.Arn) != "arn:aws:pipes:us-east-1:000000000000:pipe/orders" ||
		created.CurrentState != pipetypes.PipeStateCreating || created.DesiredState != pipetypes.RequestedPipeStateRunning {
		t.Fatalf("%+v %v", created, err)
	}

	_, err = c.CreatePipe(ctx, in)
	code(t, err, "ConflictException")

	described, err := c.DescribePipe(ctx, &pipes.DescribePipeInput{Name: aws.String("orders")})
	if err != nil || described.CurrentState != pipetypes.PipeStateRunning ||
		aws.ToString(described.Source) != source || aws.ToString(described.Target) != target ||
		aws.ToString(described.Description) != "order pipe" || described.Tags["team"] != "core" ||
		aws.ToInt32(described.SourceParameters.SqsQueueParameters.BatchSize) != 10 {
		t.Fatalf("%+v %v", described, err)
	}

	_, err = c.DescribePipe(ctx, &pipes.DescribePipeInput{Name: aws.String("missing")})
	code(t, err, "NotFoundException")

	stopped := sqsPipe("archive", source, target)
	stopped.DesiredState = pipetypes.RequestedPipeStateStopped
	if _, err = c.CreatePipe(ctx, stopped); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListPipes(ctx, &pipes.ListPipesInput{})
	if err != nil || len(listed.Pipes) != 2 || aws.ToString(listed.Pipes[0].Name) != "archive" ||
		listed.Pipes[0].CurrentState != pipetypes.PipeStateStopped {
		t.Fatalf("%+v %v", listed, err)
	}

	listed, err = c.ListPipes(ctx, &pipes.ListPipesInput{DesiredState: pipetypes.RequestedPipeStateRunning})
	if err != nil || len(listed.Pipes) != 1 || aws.ToString(listed.Pipes[0].Name) != "orders" {
		t.Fatalf("%+v %v", listed, err)
	}

	page, err := c.ListPipes(ctx, &pipes.ListPipesInput{Limit: aws.Int32(1), NamePrefix: aws.String("")})
	if err != nil || len(page.Pipes) != 1 || page.NextToken == nil {
		t.Fatalf("%+v %v", page, err)
	}

	page, err = c.ListPipes(ctx, &pipes.ListPipesInput{Limit: aws.Int32(1), NextToken: page.NextToken})
	if err != nil || len(page.Pipes) != 1 || aws.ToString(page.Pipes[0].Name) != "orders" || page.NextToken != nil {
		t.Fatalf("%+v %v", page, err)
	}

	updated, err := c.UpdatePipe(ctx, &pipes.UpdatePipeInput{
		Name: aws.String("orders"), Description: aws.String("renamed"), RoleArn: aws.String(pipeRole),
		SourceParameters: &pipetypes.UpdatePipeSourceParameters{
			SqsQueueParameters: &pipetypes.UpdatePipeSourceSqsQueueParameters{BatchSize: aws.Int32(5)},
		},
	})
	if err != nil || updated.CurrentState != pipetypes.PipeStateUpdating {
		t.Fatalf("%+v %v", updated, err)
	}

	described, err = c.DescribePipe(ctx, &pipes.DescribePipeInput{Name: aws.String("orders")})
	if err != nil || aws.ToString(described.Description) != "renamed" ||
		aws.ToInt32(described.SourceParameters.SqsQueueParameters.BatchSize) != 5 || described.Tags["team"] != "core" {
		t.Fatalf("%+v %v", described, err)
	}

	stop, err := c.StopPipe(ctx, &pipes.StopPipeInput{Name: aws.String("orders")})
	if err != nil || stop.CurrentState != pipetypes.PipeStateStopping ||
		stop.DesiredState != pipetypes.RequestedPipeStateStopped {
		t.Fatalf("%+v %v", stop, err)
	}

	described, _ = c.DescribePipe(ctx, &pipes.DescribePipeInput{Name: aws.String("orders")})
	if described.CurrentState != pipetypes.PipeStateStopped {
		t.Fatalf("%+v", described)
	}

	start, err := c.StartPipe(ctx, &pipes.StartPipeInput{Name: aws.String("orders")})
	if err != nil || start.CurrentState != pipetypes.PipeStateStarting {
		t.Fatalf("%+v %v", start, err)
	}

	described, _ = c.DescribePipe(ctx, &pipes.DescribePipeInput{Name: aws.String("orders")})
	if described.CurrentState != pipetypes.PipeStateRunning {
		t.Fatalf("%+v", described)
	}

	arn := created.Arn
	if _, err = c.TagResource(ctx, &pipes.TagResourceInput{
		ResourceArn: arn, Tags: map[string]string{"env": "dev", "team": "platform"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &pipes.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 2 || tags.Tags["team"] != "platform" {
		t.Fatalf("%+v %v", tags, err)
	}

	if _, err = c.UntagResource(
		ctx,
		&pipes.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"env"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, _ = c.ListTagsForResource(ctx, &pipes.ListTagsForResourceInput{ResourceArn: arn})
	if len(tags.Tags) != 1 {
		t.Fatalf("%+v", tags)
	}

	_, err = c.TagResource(ctx, &pipes.TagResourceInput{
		ResourceArn: aws.String("arn:aws:pipes:us-east-1:000000000000:pipe/none"), Tags: map[string]string{"a": "b"},
	})
	code(t, err, "NotFoundException")

	deleted, err := c.DeletePipe(ctx, &pipes.DeletePipeInput{Name: aws.String("orders")})
	if err != nil || deleted.CurrentState != pipetypes.PipeStateDeleting ||
		deleted.DesiredState != pipetypes.RequestedPipeStateDescribeResponseDeleted {
		t.Fatalf("%+v %v", deleted, err)
	}

	_, err = c.DeletePipe(ctx, &pipes.DeletePipeInput{Name: aws.String("orders")})
	code(t, err, "NotFoundException")
	_, err = c.StartPipe(ctx, &pipes.StartPipeInput{Name: aws.String("orders")})
	code(t, err, "NotFoundException")
}

func TestPipesQueueToQueueFilterAndTemplate(t *testing.T) {
	s := server(t)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	sourceURL, source := queueARN(t, q, "in")
	targetURL, target := queueARN(t, q, "out")

	in := sqsPipe("filtered", source, target)
	in.SourceParameters = &pipetypes.PipeSourceParameters{
		FilterCriteria: &pipetypes.FilterCriteria{Filters: []pipetypes.Filter{{
			Pattern: aws.String(`{"body":{"kind":["keep"]}}`),
		}}},
	}
	in.TargetParameters = &pipetypes.PipeTargetParameters{
		InputTemplate: aws.String(`{"id":"<$.body.id>","from":"<aws.pipes.pipe-name>"}`),
	}
	if _, err := c.CreatePipe(ctx, in); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{"kind":"drop","id":"1"}`, `{"kind":"keep","id":"2"}`} {
		if _, err := q.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &sourceURL, MessageBody: &body}); err != nil {
			t.Fatal(err)
		}
	}

	body := receivePipeBody(t, q, targetURL)
	if body != `{"id":"2","from":"filtered"}` {
		t.Fatalf("body = %q", body)
	}

	// The dropped message is deleted too, and no other arrives.
	waitFor(t, "an empty source", func() bool {
		attrs, err := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: &sourceURL,
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		return attrs.Attributes["ApproximateNumberOfMessages"] == "0" &&
			attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
	})

	// A stopped pipe moves nothing.
	if _, err := c.StopPipe(ctx, &pipes.StopPipeInput{Name: aws.String("filtered")}); err != nil {
		t.Fatal(err)
	}

	held := `{"kind":"keep","id":"3"}`
	if _, err := q.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &sourceURL, MessageBody: &held}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)
	out, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &targetURL})
	if err != nil || len(out.Messages) != 0 {
		t.Fatalf("%+v %v", out, err)
	}

	if _, err = c.StartPipe(ctx, &pipes.StartPipeInput{Name: aws.String("filtered")}); err != nil {
		t.Fatal(err)
	}

	if body = receivePipeBody(t, q, targetURL); body != `{"id":"3","from":"filtered"}` {
		t.Fatalf("body = %q", body)
	}
}

func TestPipesQueueToLambdaAndEnrichment(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 20)}
	s := serverWith(t, runtime)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	target := createFunction(t, functions, "consumer", "app.echo")
	sourceURL, source := queueARN(t, q, "jobs")

	in := sqsPipe("to-lambda", source, aws.ToString(target.FunctionArn))
	if _, err := c.CreatePipe(ctx, in); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{"one", "two"} {
		if _, err := q.SendMessage(
			ctx,
			&sqs.SendMessageInput{QueueUrl: &sourceURL, MessageBody: aws.String(body)},
		); err != nil {
			t.Fatal(err)
		}
	}

	var records []struct {
		MessageID      string `json:"messageId"`
		Body           string `json:"body"`
		EventSource    string `json:"eventSource"`
		EventSourceARN string `json:"eventSourceARN"`
	}
	for len(records) < 2 {
		select {
		case call := <-runtime.calls:
			var batch []struct {
				MessageID      string `json:"messageId"`
				Body           string `json:"body"`
				EventSource    string `json:"eventSource"`
				EventSourceARN string `json:"eventSourceARN"`
			}
			if err := json.Unmarshal(call.payload, &batch); err != nil {
				t.Fatalf("%s %v", call.payload, err)
			}

			records = append(records, batch...)
		case <-time.After(10 * time.Second):
			t.Fatalf("the function got %d records", len(records))
		}
	}

	for _, r := range records {
		if r.EventSource != "aws:sqs" || r.EventSourceARN != source || r.MessageID == "" ||
			(r.Body != "one" && r.Body != "two") {
			t.Fatalf("%+v", r)
		}
	}

	// The function succeeded, so the messages are gone.
	waitFor(t, "deleted messages", func() bool {
		out, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &sourceURL})
		return err == nil && len(out.Messages) == 0
	})

	// An enrichment function's answer replaces the event.
	enricher := createFunction(t, functions, "enricher", "app.echo")
	outURL, outARN := queueARN(t, q, "enriched")
	withEnrichment := sqsPipe("enriched", source, outARN)
	withEnrichment.Enrichment = enricher.FunctionArn
	withEnrichment.EnrichmentParameters = &pipetypes.PipeEnrichmentParameters{
		InputTemplate: aws.String(`{"text":"<$.body>"}`),
	}
	if _, err := c.CreatePipe(ctx, withEnrichment); err != nil {
		t.Fatal(err)
	}

	if _, err := c.StopPipe(ctx, &pipes.StopPipeInput{Name: aws.String("to-lambda")}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &sourceURL, MessageBody: aws.String("three")},
	); err != nil {
		t.Fatal(err)
	}

	body := receivePipeBody(t, q, outURL)
	var answer struct {
		Env   map[string]string       `json:"env"`
		Event []struct{ Text string } `json:"event"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || len(answer.Event) != 1 ||
		answer.Event[0].Text != "three" || answer.Env["GREETING"] != "hi" {
		t.Fatalf("%s %v", body, err)
	}
}

func TestPipesFailedDeliveryKeepsMessages(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 20)}
	s := serverWith(t, runtime)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	target := createFunction(t, functions, "broken", "app.fail")
	sourceURL, source := queueARN(t, q, "retry")
	if _, err := c.CreatePipe(ctx, sqsPipe("failing", source, aws.ToString(target.FunctionArn))); err != nil {
		t.Fatal(err)
	}

	if _, err := q.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &sourceURL, MessageBody: aws.String("x")},
	); err != nil {
		t.Fatal(err)
	}

	select {
	case <-runtime.calls:
	case <-time.After(10 * time.Second):
		t.Fatal("the function was not invoked")
	}

	// The message was received but not deleted: it is in flight.
	time.Sleep(200 * time.Millisecond)
	attrs, err := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       &sourceURL,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible},
	})
	if err != nil || attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] != "1" {
		t.Fatalf("%+v %v", attrs, err)
	}
}

func TestPipesKinesisAndDynamoDBStreams(t *testing.T) {
	s := server(t)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	k := kinesisClient(s)
	db := client(s, "test", "us-east-1")
	outURL, outARN := queueARN(t, q, "sink")

	if _, err := k.CreateStream(
		ctx,
		&kinesis.CreateStreamInput{StreamName: aws.String("feed"), ShardCount: aws.Int32(1)},
	); err != nil {
		t.Fatal(err)
	}

	summary, err := k.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: aws.String("feed")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = k.PutRecord(ctx, &kinesis.PutRecordInput{
		StreamName: aws.String("feed"), Data: []byte(`{"n":1}`), PartitionKey: aws.String("p"),
	}); err != nil {
		t.Fatal(err)
	}

	fromKinesis := sqsPipe("from-kinesis", aws.ToString(summary.StreamDescriptionSummary.StreamARN), outARN)
	fromKinesis.SourceParameters = &pipetypes.PipeSourceParameters{
		KinesisStreamParameters: &pipetypes.PipeSourceKinesisStreamParameters{
			StartingPosition: pipetypes.KinesisStreamStartPositionTrimHorizon,
		},
		FilterCriteria: &pipetypes.FilterCriteria{
			Filters: []pipetypes.Filter{{Pattern: aws.String(`{"data":{"n":[1]}}`)}},
		},
	}
	if _, err = c.CreatePipe(ctx, fromKinesis); err != nil {
		t.Fatal(err)
	}

	body := receivePipeBody(t, q, outURL)
	var record struct {
		EventSource  string `json:"eventSource"`
		PartitionKey string `json:"partitionKey"`
		Data         []byte `json:"data"`
	}
	if err = json.Unmarshal([]byte(body), &record); err != nil || record.EventSource != "aws:kinesis" ||
		record.PartitionKey != "p" || string(record.Data) != `{"n":1}` {
		t.Fatalf("%s %v", body, err)
	}

	// A record the filter drops is skipped, and a later one is delivered once.
	for _, data := range []string{`{"n":2}`, `{"n":1,"again":true}`} {
		if _, err = k.PutRecord(ctx, &kinesis.PutRecordInput{
			StreamName: aws.String("feed"), Data: []byte(data), PartitionKey: aws.String("p"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	body = receivePipeBody(t, q, outURL)
	if err = json.Unmarshal([]byte(body), &record); err != nil || string(record.Data) != `{"n":1,"again":true}` {
		t.Fatalf("%s %v", body, err)
	}

	time.Sleep(1500 * time.Millisecond)
	if out, recvErr := q.ReceiveMessage(
		ctx,
		&sqs.ReceiveMessageInput{QueueUrl: &outURL},
	); recvErr != nil ||
		len(out.Messages) != 0 {
		t.Fatalf("%+v %v", out, recvErr)
	}

	// A DynamoDB stream pipe.
	streamARN := streamTable(t, db, "orders", types.StreamViewTypeNewImage)
	fromTable := sqsPipe("from-table", streamARN, outARN)
	fromTable.SourceParameters = &pipetypes.PipeSourceParameters{
		DynamoDBStreamParameters: &pipetypes.PipeSourceDynamoDBStreamParameters{
			StartingPosition: pipetypes.DynamoDBStreamStartPositionLatest,
		},
	}
	if _, err = c.CreatePipe(ctx, fromTable); err != nil {
		t.Fatal(err)
	}

	if _, err = db.PutItem(ctx, &sdk.PutItemInput{
		TableName: aws.String(
			"orders",
		),
		Item: map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: "o1"}},
	}); err != nil {
		t.Fatal(err)
	}

	body = receivePipeBody(t, q, outURL)
	var change struct {
		DynamoDB struct {
			NewImage map[string]map[string]string
		} `json:"dynamodb"`
		EventName      string `json:"eventName"`
		EventSourceARN string `json:"eventSourceARN"`
	}
	if err = json.Unmarshal([]byte(body), &change); err != nil || change.EventName != "INSERT" ||
		change.EventSourceARN != streamARN || change.DynamoDB.NewImage["PK"]["S"] != "o1" {
		t.Fatalf("%s %v", body, err)
	}
}

func TestPipesTopicTarget(t *testing.T) {
	s := server(t)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	topics := snsClient(s, "test")
	ctx := context.Background()
	sourceURL, source := queueARN(t, q, "origin")
	subscriberURL, subscriber := queueARN(t, q, "subscriber")

	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("alerts")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(subscriber),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreatePipe(ctx, sqsPipe("to-topic", source, aws.ToString(topic.TopicArn))); err != nil {
		t.Fatal(err)
	}

	if _, err = q.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &sourceURL, MessageBody: aws.String("ping")},
	); err != nil {
		t.Fatal(err)
	}

	var envelope struct{ Message string }
	body := receivePipeBody(t, q, subscriberURL)
	if err = json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}

	var record struct{ Body string }
	if err = json.Unmarshal([]byte(envelope.Message), &record); err != nil || record.Body != "ping" {
		t.Fatalf("%s %v", body, err)
	}
}

func TestPipesUnsupportedAndInvalid(t *testing.T) {
	s := server(t)
	c := pipesClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	_, source := queueARN(t, q, "origin")
	_, target := queueARN(t, q, "sink")

	fails := func(what string, in *pipes.CreatePipeInput) {
		t.Helper()
		if _, err := c.CreatePipe(ctx, in); err == nil {
			t.Fatalf("%s: created", what)
		} else {
			code(t, err, "ValidationException")
		}
	}

	fails("an MQ source", sqsPipe("a", "arn:aws:mq:us-east-1:000000000000:broker:b:b-1", target))
	fails("a Firehose target", sqsPipe("b", source, "arn:aws:firehose:us-east-1:000000000000:deliverystream/d"))
	fails("an ECS target", sqsPipe("c", source, "arn:aws:ecs:us-east-1:000000000000:cluster/c"))
	fails("a missing queue", sqsPipe("d", "arn:aws:sqs:us-east-1:000000000000:nowhere", target))
	fails("another region", sqsPipe("e", "arn:aws:sqs:eu-west-1:000000000000:origin", target))
	fails("a Kinesis target without a key", sqsPipe("f", source, "arn:aws:kinesis:us-east-1:000000000000:stream/feed"))
	fails("an invalid name", sqsPipe("bad name", source, target))
	fails("a non-Lambda enrichment", func() *pipes.CreatePipeInput {
		in := sqsPipe("g", source, target)
		in.Enrichment = aws.String(target)
		return in
	}())
	fails("a batching window", func() *pipes.CreatePipeInput {
		in := sqsPipe("h", source, target)
		in.SourceParameters = &pipetypes.PipeSourceParameters{
			SqsQueueParameters: &pipetypes.PipeSourceSqsQueueParameters{
				MaximumBatchingWindowInSeconds: aws.Int32(5),
			},
		}
		return in
	}())
	fails("an invalid filter", func() *pipes.CreatePipeInput {
		in := sqsPipe("i", source, target)
		in.SourceParameters = &pipetypes.PipeSourceParameters{FilterCriteria: &pipetypes.FilterCriteria{
			Filters: []pipetypes.Filter{{Pattern: aws.String(`{"body":"x"}`)}},
		}}
		return in
	}())
	fails("a template with an unsupported reference", func() *pipes.CreatePipeInput {
		in := sqsPipe("j", source, target)
		in.TargetParameters = &pipetypes.PipeTargetParameters{InputTemplate: aws.String("<$.a[*].b>")}
		return in
	}())
	fails("parameters for another target", func() *pipes.CreatePipeInput {
		in := sqsPipe("k", source, target)
		in.TargetParameters = &pipetypes.PipeTargetParameters{
			KinesisStreamParameters: &pipetypes.PipeTargetKinesisStreamParameters{PartitionKey: aws.String("p")},
		}
		return in
	}())
	fails(
		"a stream without a starting position",
		sqsPipe("l", "arn:aws:kinesis:us-east-1:000000000000:stream/feed", target),
	)
	// A member the profile leaves out is refused by the wire layer.
	unsupported := sqsPipe("m", source, target)
	unsupported.LogConfiguration = &pipetypes.PipeLogConfigurationParameters{Level: pipetypes.LogLevelInfo}
	_, err := c.CreatePipe(ctx, unsupported)
	code(t, err, "NotImplemented")

	listed, err := c.ListPipes(ctx, &pipes.ListPipesInput{})
	if err != nil || len(listed.Pipes) != 0 {
		t.Fatalf("%+v %v", listed, err)
	}
}

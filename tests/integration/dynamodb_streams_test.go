package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	streams "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	streamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

func streamsClient(s *httptest.Server, key string) *streams.Client {
	return streams.New(
		streams.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

// streamTable creates a table with a stream of viewType and returns its ARN.
func streamTable(t *testing.T, c *sdk.Client, name string, viewType types.StreamViewType) string {
	t.Helper()
	out, err := c.CreateTable(context.Background(), &sdk.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash}},
		StreamSpecification: &types.StreamSpecification{
			StreamEnabled:  aws.Bool(true),
			StreamViewType: viewType,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.TableDescription.LatestStreamArn)
}

// readStream returns every record of the stream's shard from the iterator of
// kind (at seq, for the sequence number kinds) and the next iterator.
func readStream(
	t *testing.T,
	c *streams.Client,
	arn string,
	kind streamtypes.ShardIteratorType,
	seq string,
) (records []streamtypes.Record, next *string) {
	t.Helper()
	ctx := context.Background()
	described, err := c.DescribeStream(ctx, &streams.DescribeStreamInput{StreamArn: &arn})
	if err != nil || len(described.StreamDescription.Shards) != 1 {
		t.Fatalf("%+v %v", described, err)
	}

	in := &streams.GetShardIteratorInput{
		StreamArn:         &arn,
		ShardId:           described.StreamDescription.Shards[0].ShardId,
		ShardIteratorType: kind,
	}
	if seq != "" {
		in.SequenceNumber = &seq
	}

	it, err := c.GetShardIterator(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.GetRecords(ctx, &streams.GetRecordsInput{ShardIterator: it.ShardIterator})
	if err != nil {
		t.Fatal(err)
	}

	return out.Records, out.NextShardIterator
}

func stringAttr(a streamtypes.AttributeValue) string {
	if s, ok := a.(*streamtypes.AttributeValueMemberS); ok {
		return s.Value
	}

	return ""
}

func TestDynamoDBStreams(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	sc := streamsClient(s, "test")
	arn := streamTable(t, c, "orders", types.StreamViewTypeNewAndOldImages)
	described, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("orders")})
	if err != nil || !aws.ToBool(described.Table.StreamSpecification.StreamEnabled) ||
		described.Table.StreamSpecification.StreamViewType != types.StreamViewTypeNewAndOldImages ||
		aws.ToString(described.Table.LatestStreamArn) != arn ||
		arn != "arn:aws:dynamodb:us-east-1:000000000000:table/orders/stream/"+
			aws.ToString(described.Table.LatestStreamLabel) {
		t.Fatalf("%+v %v", described, err)
	}

	item := func(id, status string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"PK":     &types.AttributeValueMemberS{Value: id},
			"status": &types.AttributeValueMemberS{Value: status},
		}
	}

	for _, write := range []func() error{
		func() error {
			_, e := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: item("o1", "new")})
			return e
		},
		func() error {
			_, e := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: item("o1", "paid")})
			return e
		},
		// A write that changes nothing leaves no record.
		func() error {
			_, e := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: item("o1", "paid")})
			return e
		},
		func() error {
			_, e := c.DeleteItem(ctx, &sdk.DeleteItemInput{TableName: aws.String("orders"), Key: key("o1")})
			return e
		},
		// Deleting a missing item leaves no record either.
		func() error {
			_, e := c.DeleteItem(ctx, &sdk.DeleteItemInput{TableName: aws.String("orders"), Key: key("o1")})
			return e
		},
		func() error {
			_, e := c.TransactWriteItems(ctx, &sdk.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
				{Put: &types.Put{TableName: aws.String("orders"), Item: item("o2", "new")}},
				{Put: &types.Put{TableName: aws.String("orders"), Item: item("o3", "new")}},
			}})
			return e
		},
	} {
		if err = write(); err != nil {
			t.Fatal(err)
		}
	}

	records, next := readStream(t, sc, arn, streamtypes.ShardIteratorTypeTrimHorizon, "")
	if len(records) != 5 || next == nil {
		t.Fatalf("%d records %v", len(records), next)
	}

	want := []struct{ event, id, oldStatus, newStatus string }{
		{"INSERT", "o1", "", "new"},
		{"MODIFY", "o1", "new", "paid"},
		{"REMOVE", "o1", "paid", ""},
		{"INSERT", "o2", "", "new"},
		{"INSERT", "o3", "", "new"},
	}
	for i, w := range want {
		r := records[i]
		d := r.Dynamodb
		if string(r.EventName) != w.event || aws.ToString(r.EventSource) != "aws:dynamodb" ||
			aws.ToString(r.EventVersion) != "1.1" || aws.ToString(r.AwsRegion) != "us-east-1" ||
			stringAttr(d.Keys["PK"]) != w.id || stringAttr(d.OldImage["status"]) != w.oldStatus ||
			stringAttr(d.NewImage["status"]) != w.newStatus || aws.ToInt64(d.SizeBytes) <= 0 ||
			d.StreamViewType != streamtypes.StreamViewTypeNewAndOldImages || r.UserIdentity != nil ||
			aws.ToString(d.SequenceNumber) != fmtSequence(i+1) {
			t.Fatalf("record %d: %+v %+v", i, r, d)
		}
	}

	// AT and AFTER a sequence number, and LATEST, which sees only new records.
	if at, _ := readStream(t, sc, arn, streamtypes.ShardIteratorTypeAtSequenceNumber, fmtSequence(4)); len(at) != 2 {
		t.Fatalf("at: %d", len(at))
	}

	if after, _ := readStream(
		t,
		sc,
		arn,
		streamtypes.ShardIteratorTypeAfterSequenceNumber,
		fmtSequence(4),
	); len(
		after,
	) != 1 ||
		stringAttr(after[0].Dynamodb.Keys["PK"]) != "o3" {
		t.Fatalf("after: %+v", after)
	}

	latest, latestNext := readStream(t, sc, arn, streamtypes.ShardIteratorTypeLatest, "")
	if len(latest) != 0 || latestNext == nil {
		t.Fatalf("latest: %+v", latest)
	}

	if _, err = c.PutItem(
		ctx,
		&sdk.PutItemInput{TableName: aws.String("orders"), Item: item("o4", "new")},
	); err != nil {
		t.Fatal(err)
	}

	more, err := sc.GetRecords(ctx, &streams.GetRecordsInput{ShardIterator: latestNext, Limit: aws.Int32(10)})
	if err != nil || len(more.Records) != 1 || stringAttr(more.Records[0].Dynamodb.Keys["PK"]) != "o4" {
		t.Fatalf("%+v %v", more, err)
	}

	list, err := sc.ListStreams(ctx, &streams.ListStreamsInput{TableName: aws.String("orders")})
	if err != nil || len(list.Streams) != 1 || aws.ToString(list.Streams[0].StreamArn) != arn {
		t.Fatalf("%+v %v", list, err)
	}

	// Disabling closes the shard; its records stay readable, and a reader
	// that reaches the end gets no next iterator.
	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("orders"),
		StreamSpecification: &types.StreamSpecification{
			StreamEnabled:  aws.Bool(true),
			StreamViewType: types.StreamViewTypeKeysOnly,
		},
	})
	code(t, err, "ValidationException")
	updated, err := c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName:           aws.String("orders"),
		StreamSpecification: &types.StreamSpecification{StreamEnabled: aws.Bool(false)},
	})
	if err != nil || updated.TableDescription.StreamSpecification != nil ||
		aws.ToString(updated.TableDescription.LatestStreamArn) != arn {
		t.Fatalf("%+v %v", updated, err)
	}

	closed, err := sc.DescribeStream(ctx, &streams.DescribeStreamInput{StreamArn: &arn})
	if err != nil || closed.StreamDescription.StreamStatus != streamtypes.StreamStatusDisabled ||
		aws.ToString(closed.StreamDescription.Shards[0].SequenceNumberRange.EndingSequenceNumber) != fmtSequence(6) {
		t.Fatalf("%+v %v", closed, err)
	}

	if all, end := readStream(t, sc, arn, streamtypes.ShardIteratorTypeTrimHorizon, ""); len(all) != 6 || end != nil {
		t.Fatalf("closed stream: %d %v", len(all), end)
	}

	if _, err = c.PutItem(
		ctx,
		&sdk.PutItemInput{TableName: aws.String("orders"), Item: item("o5", "new")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName:           aws.String("orders"),
		StreamSpecification: &types.StreamSpecification{StreamEnabled: aws.Bool(false)},
	})
	code(t, err, "ValidationException")

	// Enabling again opens a new stream that starts empty.
	reopened, err := c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("orders"),
		StreamSpecification: &types.StreamSpecification{
			StreamEnabled:  aws.Bool(true),
			StreamViewType: types.StreamViewTypeKeysOnly,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	second := aws.ToString(reopened.TableDescription.LatestStreamArn)
	if second == arn {
		t.Fatal("re-enabling kept the stream ARN")
	}

	if _, err = c.PutItem(
		ctx,
		&sdk.PutItemInput{TableName: aws.String("orders"), Item: item("o6", "new")},
	); err != nil {
		t.Fatal(err)
	}

	keysOnly, _ := readStream(t, sc, second, streamtypes.ShardIteratorTypeTrimHorizon, "")
	if len(keysOnly) != 1 || keysOnly[0].Dynamodb.NewImage != nil ||
		stringAttr(keysOnly[0].Dynamodb.Keys["PK"]) != "o6" {
		t.Fatalf("%+v", keysOnly)
	}

	list, err = sc.ListStreams(ctx, &streams.ListStreamsInput{Limit: aws.Int32(1)})
	if err != nil || len(list.Streams) != 1 || aws.ToString(list.LastEvaluatedStreamArn) != arn {
		t.Fatalf("%+v %v", list, err)
	}

	list, err = sc.ListStreams(ctx, &streams.ListStreamsInput{ExclusiveStartStreamArn: list.LastEvaluatedStreamArn})
	if err != nil || len(list.Streams) != 1 || aws.ToString(list.Streams[0].StreamArn) != second {
		t.Fatalf("%+v %v", list, err)
	}

	// Deleting the table closes its stream but keeps it readable.
	if _, err = c.DeleteTable(ctx, &sdk.DeleteTableInput{TableName: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	gone, err := sc.DescribeStream(ctx, &streams.DescribeStreamInput{StreamArn: &second})
	if err != nil || gone.StreamDescription.StreamStatus != streamtypes.StreamStatusDisabled {
		t.Fatalf("%+v %v", gone, err)
	}

	// Errors.
	_, err = sc.GetRecords(ctx, &streams.GetRecordsInput{ShardIterator: aws.String("bogus")})
	code(t, err, "ValidationException")
	_, err = streamsClient(s, "team-b").DescribeStream(ctx, &streams.DescribeStreamInput{StreamArn: &arn})
	code(t, err, "ResourceNotFoundException")
	_, err = sc.GetShardIterator(ctx, &streams.GetShardIteratorInput{
		StreamArn:         &arn,
		ShardId:           aws.String("shardId-missing"),
		ShardIteratorType: streamtypes.ShardIteratorTypeTrimHorizon,
	})
	code(t, err, "ResourceNotFoundException")
	_, err = c.CreateTable(ctx, &sdk.CreateTableInput{
		TableName:   aws.String("bad"),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema:           []types.KeySchemaElement{{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash}},
		StreamSpecification: &types.StreamSpecification{StreamEnabled: aws.Bool(true)},
	})
	code(t, err, "ValidationException")
}

func fmtSequence(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 21 {
		s = "0" + s
	}

	return s
}

func TestDynamoDBTimeToLive(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	arn := streamTable(t, c, "sessions", types.StreamViewTypeOldImage)
	enable := &sdk.UpdateTimeToLiveInput{
		TableName: aws.String("sessions"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			Enabled:       aws.Bool(true),
			AttributeName: aws.String("expires"),
		},
	}
	updated, err := c.UpdateTimeToLive(ctx, enable)
	if err != nil || !aws.ToBool(updated.TimeToLiveSpecification.Enabled) {
		t.Fatalf("%+v %v", updated, err)
	}

	_, err = c.UpdateTimeToLive(ctx, enable)
	code(t, err, "ValidationException")
	described, err := c.DescribeTimeToLive(ctx, &sdk.DescribeTimeToLiveInput{TableName: aws.String("sessions")})
	if err != nil || described.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusEnabled ||
		aws.ToString(described.TimeToLiveDescription.AttributeName) != "expires" {
		t.Fatalf("%+v %v", described, err)
	}

	now := time.Now().Unix()
	for id, expires := range map[string]types.AttributeValue{
		"past":    &types.AttributeValueMemberN{Value: strconv.FormatInt(now-60, 10)},
		"future":  &types.AttributeValueMemberN{Value: strconv.FormatInt(now+3600, 10)},
		"ancient": &types.AttributeValueMemberN{Value: strconv.FormatInt(now-6*365*24*3600, 10)},
		"text":    &types.AttributeValueMemberS{Value: strconv.FormatInt(now-60, 10)},
	} {
		_, err = c.PutItem(ctx, &sdk.PutItemInput{
			TableName: aws.String("sessions"),
			Item: map[string]types.AttributeValue{
				"PK":      &types.AttributeValueMemberS{Value: id},
				"expires": expires,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, getErr := c.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("sessions"), Key: key("past")})
		if getErr != nil {
			t.Fatal(getErr)
		}

		if got.Item == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("expired item was not deleted")
		}

		time.Sleep(50 * time.Millisecond)
	}

	scan, err := c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("sessions")})
	if err != nil || scan.Count != 3 {
		t.Fatalf("%+v %v", scan, err)
	}

	// The deletion reaches the stream as a TTL removal.
	records, _ := readStream(t, streamsClient(s, "test"), arn, streamtypes.ShardIteratorTypeTrimHorizon, "")
	var removal *streamtypes.Record
	for i := range records {
		if records[i].EventName == streamtypes.OperationTypeRemove {
			removal = &records[i]
		}
	}

	if removal == nil || removal.UserIdentity == nil || aws.ToString(removal.UserIdentity.Type) != "Service" ||
		aws.ToString(removal.UserIdentity.PrincipalId) != "dynamodb.amazonaws.com" ||
		stringAttr(removal.Dynamodb.Keys["PK"]) != "past" || removal.Dynamodb.OldImage["expires"] == nil {
		t.Fatalf("%+v", records)
	}

	disable := &sdk.UpdateTimeToLiveInput{
		TableName: aws.String("sessions"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			Enabled:       aws.Bool(false),
			AttributeName: aws.String("expires"),
		},
	}
	if _, err = c.UpdateTimeToLive(ctx, disable); err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateTimeToLive(ctx, disable)
	code(t, err, "ValidationException")
	described, err = c.DescribeTimeToLive(ctx, &sdk.DescribeTimeToLiveInput{TableName: aws.String("sessions")})
	if err != nil || described.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
		t.Fatalf("%+v %v", described, err)
	}
}

func TestLambdaDynamoDBStreamEventSource(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := client(s, "test", "us-east-1")
	lc := lambdaClient(s, "test")
	ctx := context.Background()
	arn := streamTable(t, c, "events", types.StreamViewTypeNewImage)
	createFunction(t, lc, "indexer", "app.echo")
	put := func(id string) {
		t.Helper()
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("events"), Item: key(id)}); err != nil {
			t.Fatal(err)
		}
	}

	// LATEST skips records written before the mapping.
	put("before")
	mapping, err := lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn:   &arn,
		FunctionName:     aws.String("indexer"),
		StartingPosition: lambdatypes.EventSourcePositionLatest,
		BatchSize:        aws.Int32(10),
	})
	if err != nil || mapping.StartingPosition != lambdatypes.EventSourcePositionLatest ||
		aws.ToInt32(mapping.MaximumRetryAttempts) != -1 || aws.ToInt32(mapping.BatchSize) != 10 {
		t.Fatalf("%+v %v", mapping, err)
	}

	put("after")
	var call invocation
	select {
	case call = <-runtime.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("stream mapping did not invoke the function")
	}

	var event struct {
		Records []struct {
			EventName      string `json:"eventName"`
			EventSource    string `json:"eventSource"`
			EventSourceARN string `json:"eventSourceARN"`
			AWSRegion      string `json:"awsRegion"`
			Dynamodb       struct {
				Keys           map[string]map[string]string `json:"Keys"`
				NewImage       map[string]map[string]string `json:"NewImage"`
				SequenceNumber string                       `json:"SequenceNumber"`
			} `json:"dynamodb"`
		}
	}
	if err = json.Unmarshal(call.payload, &event); err != nil || len(event.Records) != 1 {
		t.Fatalf("%s %v", call.payload, err)
	}

	record := event.Records[0]
	if record.EventName != "INSERT" || record.EventSource != "aws:dynamodb" || record.EventSourceARN != arn ||
		record.AWSRegion != "us-east-1" || record.Dynamodb.Keys["PK"]["S"] != "after" ||
		record.Dynamodb.NewImage["PK"]["S"] != "after" || record.Dynamodb.SequenceNumber != fmtSequence(2) {
		t.Fatalf("%s", call.payload)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, getErr := lc.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: mapping.UUID})
		if getErr != nil {
			t.Fatal(getErr)
		}

		if aws.ToString(got.LastProcessingResult) == "OK" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("%+v", got)
		}

		time.Sleep(20 * time.Millisecond)
	}

	// Stream mappings need a starting position; SQS ones take none.
	_, err = lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &arn,
		FunctionName:   aws.String("indexer"),
	})
	if !isType[*lambdatypes.InvalidParameterValueException](err) {
		t.Fatalf("no starting position: %v", err)
	}

	_, err = lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn:   aws.String(arn + "x"),
		FunctionName:     aws.String("indexer"),
		StartingPosition: lambdatypes.EventSourcePositionTrimHorizon,
	})
	if !isType[*lambdatypes.InvalidParameterValueException](err) {
		t.Fatalf("missing stream: %v", err)
	}

	// A failing function is retried up to MaximumRetryAttempts, then the
	// batch is skipped and later records still arrive.
	createFunction(t, lc, "broken", "app.fail")
	_, err = lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn:       &arn,
		FunctionName:         aws.String("broken"),
		StartingPosition:     lambdatypes.EventSourcePositionTrimHorizon,
		MaximumRetryAttempts: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	calls := map[string]int{}
	deadline = time.Now().Add(10 * time.Second)
	for calls["broken"] < 2 {
		select {
		case c := <-runtime.calls:
			calls[c.fn.Name]++
		case <-time.After(time.Until(deadline)):
			t.Fatalf("calls: %v", calls)
		}
	}

	if _, err = lc.DeleteEventSourceMapping(
		ctx,
		&sdklambda.DeleteEventSourceMappingInput{UUID: mapping.UUID},
	); err != nil {
		t.Fatal(err)
	}
}

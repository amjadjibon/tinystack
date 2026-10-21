package integration_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	kintypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
)

// feed creates a Kinesis stream of one shard and returns its ARN.
func feed(t *testing.T, c *kinesis.Client, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateStream(ctx, &kinesis.CreateStreamInput{
		StreamName: aws.String(name), ShardCount: aws.Int32(1),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: aws.String(name)})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.StreamDescriptionSummary.StreamARN)
}

// kinesisChange is a record of a Kinesis streaming destination, decoded.
type kinesisChange struct {
	AwsRegion    string            `json:"awsRegion"`
	EventID      string            `json:"eventID"`
	EventName    string            `json:"eventName"`
	UserIdentity map[string]string `json:"userIdentity"`
	RecordFormat string            `json:"recordFormat"`
	TableName    string            `json:"tableName"`
	EventSource  string            `json:"eventSource"`
	// partitionKey is the partition key of the Kinesis record, not part of its data.
	partitionKey string
	Dynamodb     struct {
		Keys                        map[string]map[string]string `json:"Keys"`
		NewImage                    map[string]map[string]string `json:"NewImage"`
		OldImage                    map[string]map[string]string `json:"OldImage"`
		ApproximateCreationDateTime int64                        `json:"ApproximateCreationDateTime"`
		SizeBytes                   int64                        `json:"SizeBytes"`
	} `json:"dynamodb"`
}

// changes returns every record of a one-shard stream, oldest first.
func changes(t *testing.T, c *kinesis.Client, name string) []kinesisChange {
	t.Helper()
	it, err := c.GetShardIterator(context.Background(), &kinesis.GetShardIteratorInput{
		StreamName: aws.String(name), ShardId: aws.String("shardId-000000000000"),
		ShardIteratorType: kintypes.ShardIteratorTypeTrimHorizon,
	})
	if err != nil {
		t.Fatal(err)
	}

	var out []kinesisChange
	for iterator := it.ShardIterator; iterator != nil; {
		got, getErr := c.GetRecords(context.Background(), &kinesis.GetRecordsInput{ShardIterator: iterator})
		if getErr != nil {
			t.Fatal(getErr)
		}

		for _, r := range got.Records {
			var ch kinesisChange
			if err = json.Unmarshal(r.Data, &ch); err != nil {
				t.Fatalf("%s: %v", r.Data, err)
			}

			ch.partitionKey = aws.ToString(r.PartitionKey)
			out = append(out, ch)
		}

		if len(got.Records) == 0 {
			break
		}

		iterator = got.NextShardIterator
	}

	return out
}

func jsonS(v string) map[string]string { return map[string]string{"S": v} }

func jsonN(v string) map[string]string { return map[string]string{"N": v} }

func destinations(t *testing.T, c *sdk.Client, table string) map[string]types.KinesisDataStreamDestination {
	t.Helper()
	out, err := c.DescribeKinesisStreamingDestination(
		context.Background(),
		&sdk.DescribeKinesisStreamingDestinationInput{TableName: aws.String(table)},
	)
	if err != nil || aws.ToString(out.TableName) != table[strings.LastIndex(table, "/")+1:] {
		t.Fatalf("%+v %v", out, err)
	}

	got := map[string]types.KinesisDataStreamDestination{}
	for _, d := range out.KinesisDataStreamDestinations {
		got[aws.ToString(d.StreamArn)] = d
	}

	return got
}

func TestDynamoDBKinesisDestinationLifecycle(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	kc := kinesisClient(s)
	created, err := create(ctx, c, "orders")
	if err != nil {
		t.Fatal(err)
	}

	first, second, third := feed(t, kc, "first"), feed(t, kc, "second"), feed(t, kc, "third")
	enable := func(table, arn string, precision *string) (*sdk.EnableKinesisStreamingDestinationOutput, error) {
		in := &sdk.EnableKinesisStreamingDestinationInput{TableName: aws.String(table), StreamArn: aws.String(arn)}
		if precision != nil {
			in.EnableKinesisStreamingConfiguration = &types.EnableKinesisStreamingConfiguration{
				ApproximateCreationDateTimePrecision: types.ApproximateCreationDateTimePrecision(*precision),
			}
		}

		return c.EnableKinesisStreamingDestination(ctx, in)
	}

	if got := destinations(t, c, "orders"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}

	out, err := enable("orders", first, nil)
	if err != nil || out.DestinationStatus != types.DestinationStatusEnabling || aws.ToString(out.StreamArn) != first ||
		aws.ToString(out.TableName) != "orders" || out.EnableKinesisStreamingConfiguration.
		ApproximateCreationDateTimePrecision != types.ApproximateCreationDateTimePrecisionMillisecond {
		t.Fatalf("%+v %v", out, err)
	}

	got := destinations(t, c, "orders")
	if d := got[first]; len(got) != 1 || d.DestinationStatus != types.DestinationStatusActive ||
		d.ApproximateCreationDateTimePrecision != types.ApproximateCreationDateTimePrecisionMillisecond {
		t.Fatalf("%+v", got)
	}

	// A table is named by its ARN as well.
	if got = destinations(t, c, aws.ToString(created.TableDescription.TableArn)); len(got) != 1 {
		t.Fatalf("%+v", got)
	}

	// One destination per stream, only streams that exist in this account and
	// Region, and a valid precision.
	_, err = enable("orders", first, nil)
	code(t, err, "ResourceInUseException")
	_, err = enable("orders", "arn:aws:kinesis:us-east-1:000000000000:stream/missing", nil)
	code(t, err, "ResourceNotFoundException")
	_, err = enable("orders", "arn:aws:kinesis:eu-west-1:000000000000:stream/first", nil)
	code(t, err, "ValidationException")
	_, err = enable("orders", "arn:aws:sqs:us-east-1:000000000000:first", nil)
	code(t, err, "ValidationException")
	_, err = enable("orders", second, aws.String("SECOND"))
	code(t, err, "ValidationException")
	_, err = enable("absent", second, nil)
	code(t, err, "ResourceNotFoundException")

	// A table streams to two streams at most.
	if out, err = enable("orders", second, aws.String("MICROSECOND")); err != nil ||
		out.EnableKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision !=
			types.ApproximateCreationDateTimePrecisionMicrosecond {
		t.Fatalf("%+v %v", out, err)
	}

	_, err = enable("orders", third, nil)
	code(t, err, "LimitExceededException")

	// Update changes the precision of an active destination.
	update := func(arn string, precision types.ApproximateCreationDateTimePrecision) (
		*sdk.UpdateKinesisStreamingDestinationOutput, error,
	) {
		return c.UpdateKinesisStreamingDestination(ctx, &sdk.UpdateKinesisStreamingDestinationInput{
			TableName: aws.String("orders"), StreamArn: aws.String(arn),
			UpdateKinesisStreamingConfiguration: &types.UpdateKinesisStreamingConfiguration{
				ApproximateCreationDateTimePrecision: precision,
			},
		})
	}

	updated, err := update(first, types.ApproximateCreationDateTimePrecisionMicrosecond)
	if err != nil || updated.DestinationStatus != types.DestinationStatusUpdating ||
		updated.UpdateKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision !=
			types.ApproximateCreationDateTimePrecisionMicrosecond {
		t.Fatalf("%+v %v", updated, err)
	}

	got = destinations(t, c, "orders")
	if d := got[first]; d.DestinationStatus != types.DestinationStatusActive ||
		d.ApproximateCreationDateTimePrecision != types.ApproximateCreationDateTimePrecisionMicrosecond {
		t.Fatalf("%+v", got)
	}

	_, err = update(first, "SECOND")
	code(t, err, "ValidationException")
	_, err = c.UpdateKinesisStreamingDestination(ctx, &sdk.UpdateKinesisStreamingDestinationInput{
		TableName: aws.String("orders"), StreamArn: aws.String(first),
	})
	code(t, err, "ValidationException")
	_, err = update(third, types.ApproximateCreationDateTimePrecisionMillisecond)
	code(t, err, "ResourceNotFoundException")

	// Disabling keeps the destination listed as disabled, which frees its slot.
	disable := func(arn string) (*sdk.DisableKinesisStreamingDestinationOutput, error) {
		return c.DisableKinesisStreamingDestination(ctx, &sdk.DisableKinesisStreamingDestinationInput{
			TableName: aws.String("orders"), StreamArn: aws.String(arn),
		})
	}

	disabled, err := disable(second)
	if err != nil || disabled.DestinationStatus != types.DestinationStatusDisabling ||
		aws.ToString(disabled.StreamArn) != second {
		t.Fatalf("%+v %v", disabled, err)
	}

	got = destinations(t, c, "orders")
	if len(got) != 2 || got[second].DestinationStatus != types.DestinationStatusDisabled ||
		got[first].DestinationStatus != types.DestinationStatusActive {
		t.Fatalf("%+v", got)
	}

	_, err = disable(second)
	code(t, err, "ResourceInUseException")
	_, err = disable(third)
	code(t, err, "ResourceNotFoundException")
	_, err = update(second, types.ApproximateCreationDateTimePrecisionMillisecond)
	code(t, err, "ResourceInUseException")

	if _, err = enable("orders", third, nil); err != nil {
		t.Fatal(err)
	}

	// A disabled destination can be enabled again, in any precision.
	if _, err = disable(third); err != nil {
		t.Fatal(err)
	}

	if _, err = enable("orders", second, aws.String("MILLISECOND")); err != nil {
		t.Fatal(err)
	}

	got = destinations(t, c, "orders")
	if d := got[second]; len(got) != 3 || d.DestinationStatus != types.DestinationStatusActive ||
		d.ApproximateCreationDateTimePrecision != types.ApproximateCreationDateTimePrecisionMillisecond {
		t.Fatalf("%+v", got)
	}

	// Deleting a table removes its destinations.
	if _, err = c.DeleteTable(ctx, &sdk.DeleteTableInput{TableName: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeKinesisStreamingDestination(ctx, &sdk.DescribeKinesisStreamingDestinationInput{
		TableName: aws.String("orders"),
	})
	code(t, err, "ResourceNotFoundException")
}

func TestDynamoDBKinesisDelivery(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	kc := kinesisClient(s)
	// The table's own stream keeps keys only; the Kinesis records have both
	// images regardless, and a table needs no stream to have a destination.
	streamTable(t, c, "events", types.StreamViewTypeKeysOnly)
	if _, err := create(ctx, c, "plain"); err != nil {
		t.Fatal(err)
	}

	milli, micro := feed(t, kc, "milli"), feed(t, kc, "micro")
	for table, destination := range map[string][]struct{ arn, precision string }{
		"events": {{milli, "MILLISECOND"}, {micro, "MICROSECOND"}},
		"plain":  {{micro, "MICROSECOND"}},
	} {
		for _, d := range destination {
			_, err := c.EnableKinesisStreamingDestination(ctx, &sdk.EnableKinesisStreamingDestinationInput{
				TableName: aws.String(table), StreamArn: aws.String(d.arn),
				EnableKinesisStreamingConfiguration: &types.EnableKinesisStreamingConfiguration{
					ApproximateCreationDateTimePrecision: types.ApproximateCreationDateTimePrecision(d.precision),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	before := time.Now()
	item := func(id, status string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: id}, "status": &types.AttributeValueMemberS{Value: status},
		}
	}

	putItem := func(table, id string) {
		t.Helper()
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String(table), Item: item(id, "x")}); err != nil {
			t.Fatal(err)
		}
	}

	put := &sdk.PutItemInput{TableName: aws.String("events"), Item: item("a", "new")}
	if _, err := c.PutItem(ctx, put); err != nil {
		t.Fatal(err)
	}
	// Putting the same item again changes nothing, so it is not streamed.
	if _, err := c.PutItem(ctx, put); err != nil {
		t.Fatal(err)
	}

	if _, err := c.UpdateItem(ctx, &sdk.UpdateItemInput{
		TableName: aws.String("events"), Key: key("a"), UpdateExpression: aws.String("SET #s = :s, hits = :n"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s": &types.AttributeValueMemberS{Value: "paid"}, ":n": &types.AttributeValueMemberN{Value: "3"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.DeleteItem(ctx, &sdk.DeleteItemInput{TableName: aws.String("events"), Key: key("a")}); err != nil {
		t.Fatal(err)
	}
	// Deleting an item that is not there changes nothing.
	if _, err := c.DeleteItem(ctx, &sdk.DeleteItemInput{TableName: aws.String("events"), Key: key("a")}); err != nil {
		t.Fatal(err)
	}

	got := changes(t, kc, "milli")
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}

	insert, modify, remove := got[0], got[1], got[2]
	created := map[string]map[string]string{"PK": jsonS("a"), "status": jsonS("new")}
	if insert.EventName != "INSERT" || insert.Dynamodb.OldImage != nil ||
		!reflect.DeepEqual(insert.Dynamodb.NewImage, created) {
		t.Fatalf("%+v", insert)
	}

	if modify.EventName != "MODIFY" ||
		!reflect.DeepEqual(modify.Dynamodb.OldImage, created) ||
		!reflect.DeepEqual(modify.Dynamodb.NewImage, map[string]map[string]string{
			"PK": jsonS("a"), "status": jsonS("paid"), "hits": jsonN("3"),
		}) {
		t.Fatalf("%+v", modify)
	}

	if remove.EventName != "REMOVE" || remove.Dynamodb.NewImage != nil || len(remove.Dynamodb.OldImage) != 3 {
		t.Fatalf("%+v", remove)
	}

	for _, ch := range got {
		if ch.AwsRegion != "us-east-1" || ch.TableName != "events" || ch.RecordFormat != "application/json" ||
			ch.EventSource != "aws:dynamodb" || ch.UserIdentity != nil || len(ch.EventID) != 32 ||
			!reflect.DeepEqual(ch.Dynamodb.Keys, map[string]map[string]string{"PK": jsonS("a")}) ||
			ch.Dynamodb.SizeBytes <= 0 || len(ch.partitionKey) != 64 || ch.partitionKey != insert.partitionKey {
			t.Fatalf("%+v", ch)
		}

		// Milliseconds since the epoch.
		if at := time.UnixMilli(ch.Dynamodb.ApproximateCreationDateTime); at.Before(before.Add(-time.Second)) ||
			at.After(time.Now().Add(time.Second)) {
			t.Fatalf("%d", ch.Dynamodb.ApproximateCreationDateTime)
		}
	}

	if modify.EventID == insert.EventID {
		t.Fatal("event IDs repeat")
	}

	// The same changes reach the other stream, in microseconds.
	micros := changes(t, kc, "micro")
	if len(micros) != 3 || micros[0].EventID != insert.EventID {
		t.Fatalf("%+v", micros)
	}

	if at := time.UnixMicro(micros[0].Dynamodb.ApproximateCreationDateTime); at.Before(before.Add(-time.Second)) ||
		at.After(time.Now().Add(time.Second)) || micros[0].Dynamodb.ApproximateCreationDateTime < 1e15 {
		t.Fatalf("%d", micros[0].Dynamodb.ApproximateCreationDateTime)
	}

	// Batch writes, transactions and PartiQL writes are streamed too.
	if _, err := c.BatchWriteItem(ctx, &sdk.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"events": {
			{PutRequest: &types.PutRequest{Item: item("b1", "x")}},
			{PutRequest: &types.PutRequest{Item: item("b2", "y")}},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.TransactWriteItems(ctx, &sdk.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: aws.String("events"), Item: item("t1", "z")}},
		{Update: &types.Update{
			TableName: aws.String("events"), Key: key("b1"), UpdateExpression: aws.String("SET #s = :s"),
			ExpressionAttributeNames:  map[string]string{"#s": "status"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":s": &types.AttributeValueMemberS{Value: "x2"}},
		}},
		{Delete: &types.Delete{TableName: aws.String("events"), Key: key("b2")}},
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{
		Statement: aws.String(`INSERT INTO "events" VALUE {'PK': 'p1', 'status': 'q'}`),
	}); err != nil {
		t.Fatal(err)
	}

	got = changes(t, kc, "milli")[3:]
	if len(got) != 6 {
		t.Fatalf("%+v", got)
	}

	events := map[string][]string{}
	for _, ch := range got {
		events[ch.EventName] = append(events[ch.EventName], ch.Dynamodb.Keys["PK"]["S"])
	}

	if !reflect.DeepEqual(events, map[string][]string{
		"INSERT": {"b1", "b2", "t1", "p1"}, "MODIFY": {"b1"}, "REMOVE": {"b2"},
	}) {
		t.Fatalf("%+v", events)
	}

	if tx := got[3]; tx.EventName != "MODIFY" || tx.Dynamodb.OldImage["status"]["S"] != "x" ||
		tx.Dynamodb.NewImage["status"]["S"] != "x2" {
		t.Fatalf("%+v", tx)
	}

	// A transaction that is cancelled streams nothing.
	_, err := c.TransactWriteItems(ctx, &sdk.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: aws.String("events"), Item: item("never", "x")}},
		{ConditionCheck: &types.ConditionCheck{
			TableName: aws.String("events"), Key: key("absent"),
			ConditionExpression: aws.String("attribute_exists(PK)"),
		}},
	}})
	code(t, err, "TransactionCanceledException")

	if n := len(changes(t, kc, "milli")); n != 9 {
		t.Fatalf("%d records", n)
	}

	// A table without a stream streams to Kinesis all the same.
	putItem("plain", "only")

	if micros = changes(t, kc, "micro"); len(micros) != 10 || micros[9].TableName != "plain" {
		t.Fatalf("%+v", micros)
	}

	// A disabled destination stops receiving, and the other goes on.
	if _, err = c.DisableKinesisStreamingDestination(ctx, &sdk.DisableKinesisStreamingDestinationInput{
		TableName: aws.String("events"), StreamArn: aws.String(milli),
	}); err != nil {
		t.Fatal(err)
	}

	putItem("events", "late")

	if n := len(changes(t, kc, "milli")); n != 9 {
		t.Fatalf("%d records after disabling", n)
	}

	if micros = changes(t, kc, "micro"); len(micros) != 11 || micros[10].Dynamodb.Keys["PK"]["S"] != "late" {
		t.Fatalf("%+v", micros)
	}

	// A write never fails because its stream is gone: the destination fails.
	if _, err = kc.DeleteStream(ctx, &kinesis.DeleteStreamInput{StreamName: aws.String("micro")}); err != nil {
		t.Fatal(err)
	}

	putItem("events", "orphan")

	d := destinations(t, c, "events")[micro]
	if d.DestinationStatus != types.DestinationStatusEnableFailed ||
		aws.ToString(d.DestinationStatusDescription) == "" {
		t.Fatalf("%+v", d)
	}

	orphan, err := c.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("events"), Key: key("orphan")})
	if err != nil || orphan.Item == nil {
		t.Fatalf("%+v %v", orphan, err)
	}
}

func TestDynamoDBKinesisTimeToLiveDelivery(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	kc := kinesisClient(s)
	if _, err := create(ctx, c, "sessions"); err != nil {
		t.Fatal(err)
	}

	arn := feed(t, kc, "expired")
	if _, err := c.EnableKinesisStreamingDestination(ctx, &sdk.EnableKinesisStreamingDestinationInput{
		TableName: aws.String("sessions"), StreamArn: aws.String(arn),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.UpdateTimeToLive(ctx, &sdk.UpdateTimeToLiveInput{
		TableName: aws.String("sessions"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			Enabled: aws.Bool(true), AttributeName: aws.String("expires"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	expired := map[string]types.AttributeValue{
		"PK":      &types.AttributeValueMemberS{Value: "old"},
		"expires": &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix()-60, 10)},
	}
	if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("sessions"), Item: expired}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := changes(t, kc, "expired"); len(got) == 2 {
			if got[1].EventName != "REMOVE" || got[1].UserIdentity["type"] != "Service" ||
				got[1].UserIdentity["principalId"] != "dynamodb.amazonaws.com" || got[1].Dynamodb.OldImage["expires"] == nil {
				t.Fatalf("%+v", got)
			}

			return
		}

		if time.Now().After(deadline) {
			t.Fatal("the TTL deletion was not streamed")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func TestCloudFormationDynamoDBKinesisStream(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := client(s, "test", "us-east-1")
	kc := kinesisClient(s)
	arn := feed(t, kc, "cfn-feed")
	template := func(precision string) string {
		body, err := json.Marshal(map[string]any{"Resources": map[string]any{"Table": map[string]any{
			"Type": "AWS::DynamoDB::Table",
			"Properties": map[string]any{
				"TableName":            "stacked",
				"BillingMode":          "PAY_PER_REQUEST",
				"AttributeDefinitions": []any{map[string]any{"AttributeName": "PK", "AttributeType": "S"}},
				"KeySchema":            []any{map[string]any{"AttributeName": "PK", "KeyType": "HASH"}},
				"KinesisStreamSpecification": map[string]any{
					"StreamArn": arn, "ApproximateCreationDateTimePrecision": precision,
				},
			},
		}}})
		if err != nil {
			t.Fatal(err)
		}

		return string(body)
	}

	cf := cfnClient(s)
	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("kinesis-table"), TemplateBody: aws.String(template("MICROSECOND")),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "kinesis-table", cfntypes.StackStatusCreateComplete)
	d := destinations(t, c, "stacked")[arn]
	if d.DestinationStatus != types.DestinationStatusActive ||
		d.ApproximateCreationDateTimePrecision != types.ApproximateCreationDateTimePrecisionMicrosecond {
		t.Fatalf("%+v", d)
	}

	if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("stacked"), Item: key("one")}); err != nil {
		t.Fatal(err)
	}

	if got := changes(t, kc, "cfn-feed"); len(got) != 1 || got[0].EventName != "INSERT" {
		t.Fatalf("%+v", got)
	}
}

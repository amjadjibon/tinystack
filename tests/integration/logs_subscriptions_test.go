package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	kintypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
)

// logsData is a subscription filter's DATA_MESSAGE.
type logsData struct {
	MessageType         string   `json:"messageType"`
	LogGroup            string   `json:"logGroup"`
	LogStream           string   `json:"logStream"`
	SubscriptionFilters []string `json:"subscriptionFilters"`
	LogEvents           []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"logEvents"`
}

func gunzipData(t *testing.T, zipped []byte) logsData {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	var d logsData
	if err = json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}

	return d
}

func TestLogsSubscriptionFilters(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := logsClient(s, "test")
	ctx := context.Background()
	createFunction(t, lambdaClient(s, "test"), "shipper", "app.echo")
	streams := kinesisClient(s)
	if _, err := streams.CreateStream(ctx, &kinesis.CreateStreamInput{
		StreamName: aws.String("events"), ShardCount: aws.Int32(1),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateLogGroup(
		ctx,
		&cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("app")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName: aws.String("app"), LogStreamName: aws.String("web"),
	}); err != nil {
		t.Fatal(err)
	}

	for name, arn := range map[string]string{
		"errors": "arn:aws:lambda:us-east-1:000000000000:function:shipper",
		"all":    "arn:aws:kinesis:us-east-1:000000000000:stream/events",
	} {
		pattern := ""
		if name == "errors" {
			pattern = "ERROR"
		}

		if _, err := c.PutSubscriptionFilter(ctx, &cloudwatchlogs.PutSubscriptionFilterInput{
			LogGroupName: aws.String("app"), FilterName: aws.String(name), FilterPattern: aws.String(pattern),
			DestinationArn: aws.String(arn), RoleArn: aws.String("arn:aws:iam::000000000000:role/logs"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var limit *logstypes.LimitExceededException
	if _, err := c.PutSubscriptionFilter(ctx, &cloudwatchlogs.PutSubscriptionFilterInput{
		LogGroupName: aws.String("app"), FilterName: aws.String("third"), FilterPattern: aws.String(""),
		DestinationArn: aws.String("arn:aws:lambda:us-east-1:000000000000:function:shipper"),
	}); !errors.As(err, &limit) {
		t.Fatalf("a third filter: %v", err)
	}

	described, err := c.DescribeSubscriptionFilters(ctx, &cloudwatchlogs.DescribeSubscriptionFiltersInput{
		LogGroupName: aws.String("app"),
	})
	if err != nil || len(described.SubscriptionFilters) != 2 ||
		described.SubscriptionFilters[0].Distribution != logstypes.DistributionByLogStream {
		t.Fatalf("described: %+v %v", described, err)
	}

	now := time.Now().UnixMilli()
	if _, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: aws.String("app"), LogStreamName: aws.String("web"),
		LogEvents: []logstypes.InputLogEvent{
			{Timestamp: aws.Int64(now), Message: aws.String("INFO started")},
			{Timestamp: aws.Int64(now + 1), Message: aws.String("ERROR disk full")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The function gets only the matching event, asynchronously.
	select {
	case call := <-runtime.calls:
		var event struct {
			AWSLogs struct {
				Data string `json:"data"`
			} `json:"awslogs"`
		}
		if err = json.Unmarshal(call.payload, &event); err != nil {
			t.Fatal(err)
		}

		zipped, decodeErr := base64.StdEncoding.DecodeString(event.AWSLogs.Data)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}

		d := gunzipData(t, zipped)
		if d.MessageType != "DATA_MESSAGE" || d.LogGroup != "app" || d.LogStream != "web" ||
			len(d.LogEvents) != 1 || d.LogEvents[0].Message != "ERROR disk full" || d.LogEvents[0].ID == "" ||
			len(d.SubscriptionFilters) != 1 || d.SubscriptionFilters[0] != "errors" {
			t.Fatalf("function got %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the function was not invoked")
	}

	iterator, err := streams.GetShardIterator(ctx, &kinesis.GetShardIteratorInput{
		StreamName: aws.String("events"), ShardId: aws.String("shardId-000000000000"),
		ShardIteratorType: kintypes.ShardIteratorTypeTrimHorizon,
	})
	if err != nil {
		t.Fatal(err)
	}

	records, err := streams.GetRecords(ctx, &kinesis.GetRecordsInput{ShardIterator: iterator.ShardIterator})
	if err != nil || len(records.Records) != 1 {
		t.Fatalf("records: %+v %v", records, err)
	}

	if d := gunzipData(t, records.Records[0].Data); len(d.LogEvents) != 2 ||
		aws.ToString(records.Records[0].PartitionKey) != "web" {
		t.Fatalf("stream got %+v", d)
	}

	if _, err = c.DeleteSubscriptionFilter(ctx, &cloudwatchlogs.DeleteSubscriptionFilterInput{
		LogGroupName: aws.String("app"), FilterName: aws.String("errors"),
	}); err != nil {
		t.Fatal(err)
	}

	var missing *logstypes.ResourceNotFoundException
	if _, err = c.DeleteSubscriptionFilter(ctx, &cloudwatchlogs.DeleteSubscriptionFilterInput{
		LogGroupName: aws.String("app"), FilterName: aws.String("errors"),
	}); !errors.As(err, &missing) {
		t.Fatalf("deleted twice: %v", err)
	}
}

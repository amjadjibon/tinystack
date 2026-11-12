package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestLambdaStreamFailureDestination(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	lc := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	arn := streamTable(t, c, "failing", types.StreamViewTypeNewImage)
	createFunction(t, lc, "broken", "app.fail")
	queueURL, queue := queueARN(t, q, "stream-failures")

	_, err := lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &arn, FunctionName: aws.String("broken"),
		StartingPosition: lambdatypes.EventSourcePositionTrimHorizon,
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnSuccess: &lambdatypes.OnSuccess{Destination: &queue},
		},
	})
	code(t, err, "InvalidParameterValueException")

	created, err := lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &arn, FunctionName: aws.String("broken"),
		StartingPosition:               lambdatypes.EventSourcePositionTrimHorizon,
		MaximumRetryAttempts:           aws.Int32(1),
		BisectBatchOnFunctionError:     aws.Bool(true),
		MaximumRecordAgeInSeconds:      aws.Int32(3600),
		MaximumBatchingWindowInSeconds: aws.Int32(1),
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnFailure: &lambdatypes.OnFailure{Destination: &queue},
		},
	})
	if err != nil || !aws.ToBool(created.BisectBatchOnFunctionError) ||
		aws.ToInt32(created.MaximumRecordAgeInSeconds) != 3600 ||
		aws.ToString(created.DestinationConfig.OnFailure.Destination) != queue {
		t.Fatalf("%+v %v", created, err)
	}

	for _, id := range []string{"a", "b"} {
		if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("failing"), Item: key(id)}); err != nil {
			t.Fatal(err)
		}
	}

	// The batch of two is split, and each record runs out of retries on its
	// own, so each is reported alone.
	var records []map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for len(records) < 2 && time.Now().Before(deadline) {
		out, receiveErr := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, WaitTimeSeconds: 1, MaxNumberOfMessages: 10,
		})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		for _, m := range out.Messages {
			var record map[string]any
			if err = json.Unmarshal([]byte(aws.ToString(m.Body)), &record); err != nil {
				t.Fatal(err)
			}

			records = append(records, record)
		}
	}

	if len(records) != 2 {
		t.Fatalf("records %v", records)
	}

	for _, record := range records {
		info, _ := record["DDBStreamBatchInfo"].(map[string]any)
		request, _ := record["requestContext"].(map[string]any)
		if info["batchSize"] != 1.0 || info["streamArn"] != arn || info["shardId"] == "" || record["payload"] != nil ||
			request["condition"] != "RetryAttemptsExhausted" || request["approximateInvokeCount"] != 2.0 {
			t.Fatalf("record %v", record)
		}
	}

	_, err = lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &queue, FunctionName: aws.String("broken"), BisectBatchOnFunctionError: aws.Bool(true),
	})
	code(t, err, "InvalidParameterValueException")
}

// An S3 destination also receives the original event, under AWS's per-mapping, per-shard key.
func TestLambdaStreamFailureS3Destination(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	lc := lambdaClient(s, "test")
	objects := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	arn := streamTable(t, c, "failing", types.StreamViewTypeNewImage)
	createFunction(t, lc, "broken", "app.fail")
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("stream-failures")}); err != nil {
		t.Fatal(err)
	}

	created, err := lc.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		EventSourceArn: &arn, FunctionName: aws.String("broken"),
		StartingPosition:     lambdatypes.EventSourcePositionTrimHorizon,
		MaximumRetryAttempts: aws.Int32(0),
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnFailure: &lambdatypes.OnFailure{Destination: aws.String("arn:aws:s3:::stream-failures")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("failing"), Item: key("a")}); err != nil {
		t.Fatal(err)
	}

	var listed *s3.ListObjectsV2Output
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		listed, err = objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("stream-failures")})
		if err != nil {
			t.Fatal(err)
		}

		if len(listed.Contents) > 0 {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	if len(listed.Contents) != 1 {
		t.Fatalf("objects %+v", listed.Contents)
	}

	object, err := objects.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: aws.String("stream-failures"), Key: listed.Contents[0].Key},
	)
	if err != nil {
		t.Fatal(err)
	}

	defer object.Body.Close()
	body, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}

	var record struct {
		Info    map[string]any `json:"DDBStreamBatchInfo"`
		Payload string         `json:"payload"`
	}

	if err = json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}

	shard, _ := record.Info["shardId"].(string)
	pattern := regexp.MustCompile(`^aws/lambda/` + regexp.QuoteMeta(aws.ToString(created.UUID)+"/"+shard) +
		`/\d{4}/\d{2}/\d{2}/\d{4}-\d{2}-\d{2}T\d{2}\.\d{2}\.\d{2}-[0-9a-f-]{36}$`)
	if shard == "" || !pattern.MatchString(aws.ToString(listed.Contents[0].Key)) {
		t.Fatalf("key %s, want %s", aws.ToString(listed.Contents[0].Key), pattern)
	}

	var event struct {
		Records []struct {
			EventSourceARN string `json:"eventSourceARN"`
		}
	}

	if err = json.Unmarshal([]byte(record.Payload), &event); err != nil || len(event.Records) != 1 ||
		event.Records[0].EventSourceARN != arn {
		t.Fatalf("payload %s: %v", record.Payload, err)
	}
}

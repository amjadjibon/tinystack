package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	kintypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func kinesisClient(s *httptest.Server) *kinesis.Client {
	return kinesis.New(kinesis.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func firehoseClient(s *httptest.Server) *firehose.Client {
	return firehose.New(firehose.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// shardRecords reads every record of a shard from an iterator.
func shardRecords(t *testing.T, c *kinesis.Client, iterator *string) []string {
	t.Helper()
	var out []string
	for iterator != nil {
		got, err := c.GetRecords(
			context.Background(),
			&kinesis.GetRecordsInput{ShardIterator: iterator, Limit: aws.Int32(2)},
		)
		if err != nil {
			t.Fatal(err)
		}

		for _, r := range got.Records {
			out = append(out, string(r.Data))
		}

		if len(got.Records) == 0 {
			break
		}

		iterator = got.NextShardIterator
	}

	return out
}

func iteratorFor(
	t *testing.T,
	c *kinesis.Client,
	kind kintypes.ShardIteratorType,
	seq *string,
) *string {
	t.Helper()
	out, err := c.GetShardIterator(context.Background(), &kinesis.GetShardIteratorInput{
		StreamName: aws.String("events"), ShardId: aws.String("shardId-000000000000"), ShardIteratorType: kind,
		StartingSequenceNumber: seq,
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.ShardIterator
}

func TestKinesisStreams(t *testing.T) {
	s := server(t)
	c := kinesisClient(s)
	ctx := context.Background()

	if _, err := c.CreateStream(ctx, &kinesis.CreateStreamInput{
		StreamName: aws.String("clicks"), ShardCount: aws.Int32(2), Tags: map[string]string{"team": "a"},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateStream(
		ctx,
		&kinesis.CreateStreamInput{StreamName: aws.String("clicks"), ShardCount: aws.Int32(1)},
	)
	code(t, err, "ResourceInUseException")

	if _, err = c.CreateStream(ctx, &kinesis.CreateStreamInput{
		StreamName: aws.String(
			"auto",
		),
		StreamModeDetails: &kintypes.StreamModeDetails{StreamMode: kintypes.StreamModeOnDemand},
	}); err != nil {
		t.Fatal(err)
	}

	summary, err := c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: aws.String("auto")})
	if err != nil || aws.ToInt32(summary.StreamDescriptionSummary.OpenShardCount) != 4 {
		t.Fatalf("%+v %v", summary, err)
	}

	described, err := c.DescribeStream(ctx, &kinesis.DescribeStreamInput{StreamName: aws.String("clicks")})
	if err != nil || len(described.StreamDescription.Shards) != 2 ||
		described.StreamDescription.StreamStatus != kintypes.StreamStatusActive ||
		aws.ToInt32(described.StreamDescription.RetentionPeriodHours) != 24 {
		t.Fatalf("%+v %v", described, err)
	}

	arn := described.StreamDescription.StreamARN
	listed, err := c.ListStreams(ctx, &kinesis.ListStreamsInput{})
	if err != nil || len(listed.StreamNames) != 2 || len(listed.StreamSummaries) != 2 {
		t.Fatalf("%+v %v", listed, err)
	}

	if _, err = c.IncreaseStreamRetentionPeriod(ctx, &kinesis.IncreaseStreamRetentionPeriodInput{
		StreamName: aws.String("clicks"), RetentionPeriodHours: aws.Int32(48),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DecreaseStreamRetentionPeriod(ctx, &kinesis.DecreaseStreamRetentionPeriodInput{
		StreamName: aws.String("clicks"), RetentionPeriodHours: aws.Int32(12),
	})
	code(t, err, "InvalidArgumentException")

	if _, err = c.StartStreamEncryption(ctx, &kinesis.StartStreamEncryptionInput{
		StreamName: aws.String(
			"clicks",
		),
		EncryptionType: kintypes.EncryptionTypeKms,
		KeyId:          aws.String("alias/aws/kinesis"),
	}); err != nil {
		t.Fatal(err)
	}

	monitored, err := c.EnableEnhancedMonitoring(ctx, &kinesis.EnableEnhancedMonitoringInput{
		StreamName: aws.String("clicks"), ShardLevelMetrics: []kintypes.MetricsName{kintypes.MetricsNameIncomingBytes},
	})
	if err != nil || len(monitored.DesiredShardLevelMetrics) != 1 {
		t.Fatalf("%+v %v", monitored, err)
	}

	if _, err = c.AddTagsToStream(ctx, &kinesis.AddTagsToStreamInput{
		StreamName: aws.String("clicks"), Tags: map[string]string{"env": "dev"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(
		ctx,
		&kinesis.UntagResourceInput{ResourceARN: arn, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForStream(ctx, &kinesis.ListTagsForStreamInput{StreamName: aws.String("clicks")})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "env" {
		t.Fatalf("%+v %v", tags, err)
	}

	consumer, err := c.RegisterStreamConsumer(ctx, &kinesis.RegisterStreamConsumerInput{
		StreamARN: arn, ConsumerName: aws.String("app"),
	})
	if err != nil || consumer.Consumer.ConsumerStatus != kintypes.ConsumerStatusActive {
		t.Fatalf("%+v %v", consumer, err)
	}

	_, err = c.DeleteStream(ctx, &kinesis.DeleteStreamInput{StreamName: aws.String("clicks")})
	code(t, err, "ResourceInUseException")

	if _, err = c.DeregisterStreamConsumer(ctx, &kinesis.DeregisterStreamConsumerInput{
		ConsumerARN: consumer.Consumer.ConsumerARN,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteStream(ctx, &kinesis.DeleteStreamInput{StreamName: aws.String("clicks")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: aws.String("clicks")})
	code(t, err, "ResourceNotFoundException")
}

func TestKinesisRecords(t *testing.T) {
	s := server(t)
	c := kinesisClient(s)
	ctx := context.Background()
	if _, err := c.CreateStream(
		ctx,
		&kinesis.CreateStreamInput{StreamName: aws.String("events"), ShardCount: aws.Int32(2)},
	); err != nil {
		t.Fatal(err)
	}

	// An explicit hash key routes to the first shard.
	first, err := c.PutRecord(ctx, &kinesis.PutRecordInput{
		StreamName: aws.String(
			"events",
		),
		Data:            []byte("a"),
		PartitionKey:    aws.String("k"),
		ExplicitHashKey: aws.String("0"),
	})
	if err != nil || aws.ToString(first.ShardId) != "shardId-000000000000" {
		t.Fatalf("%+v %v", first, err)
	}

	batch, err := c.PutRecords(
		ctx,
		&kinesis.PutRecordsInput{StreamName: aws.String("events"), Records: []kintypes.PutRecordsRequestEntry{
			{Data: []byte("b"), PartitionKey: aws.String("k"), ExplicitHashKey: aws.String("1")},
			{Data: []byte("c"), PartitionKey: aws.String("k"), ExplicitHashKey: aws.String("2")},
			{Data: []byte("bad"), PartitionKey: aws.String("")},
		}},
	)
	if err != nil || aws.ToInt32(batch.FailedRecordCount) != 1 ||
		aws.ToString(batch.Records[2].ErrorCode) != "InvalidArgumentException" {
		t.Fatalf("%+v %v", batch, err)
	}

	shard := "shardId-000000000000"
	if got := shardRecords(
		t,
		c,
		iteratorFor(t, c, kintypes.ShardIteratorTypeTrimHorizon, nil),
	); strings.Join(
		got,
		"",
	) != "abc" {
		t.Fatal(got)
	}

	if got := shardRecords(t, c, iteratorFor(t, c, kintypes.ShardIteratorTypeAfterSequenceNumber,
		first.SequenceNumber)); strings.Join(got, "") != "bc" {
		t.Fatal(got)
	}

	if got := shardRecords(t, c, iteratorFor(t, c, kintypes.ShardIteratorTypeAtSequenceNumber,
		batch.Records[1].SequenceNumber)); strings.Join(got, "") != "c" {
		t.Fatal(got)
	}

	latest := iteratorFor(t, c, kintypes.ShardIteratorTypeLatest, nil)
	if _, err = c.PutRecord(ctx, &kinesis.PutRecordInput{
		StreamName: aws.String(
			"events",
		),
		Data:            []byte("d"),
		PartitionKey:    aws.String("k"),
		ExplicitHashKey: aws.String("3"),
	}); err != nil {
		t.Fatal(err)
	}

	if got := shardRecords(t, c, latest); strings.Join(got, "") != "d" {
		t.Fatal(got)
	}

	// Resharding closes the old shards; a closed shard read to its end names
	// its children.
	updated, err := c.UpdateShardCount(ctx, &kinesis.UpdateShardCountInput{
		StreamName: aws.String(
			"events",
		),
		TargetShardCount: aws.Int32(4),
		ScalingType:      kintypes.ScalingTypeUniformScaling,
	})
	if err != nil || aws.ToInt32(updated.CurrentShardCount) != 2 {
		t.Fatalf("%+v %v", updated, err)
	}

	shards, err := c.ListShards(ctx, &kinesis.ListShardsInput{StreamName: aws.String("events")})
	if err != nil || len(shards.Shards) != 6 || shards.Shards[0].SequenceNumberRange.EndingSequenceNumber == nil ||
		aws.ToString(shards.Shards[2].ParentShardId) != shard {
		t.Fatalf("%+v %v", shards, err)
	}

	open, err := c.ListShards(ctx, &kinesis.ListShardsInput{
		StreamName: aws.String("events"), ShardFilter: &kintypes.ShardFilter{Type: kintypes.ShardFilterTypeAtLatest},
	})
	if err != nil || len(open.Shards) != 4 {
		t.Fatalf("%+v %v", open, err)
	}

	iterator := iteratorFor(t, c, kintypes.ShardIteratorTypeTrimHorizon, nil)
	got, err := c.GetRecords(ctx, &kinesis.GetRecordsInput{ShardIterator: iterator})
	if err != nil || len(got.Records) != 4 || got.NextShardIterator != nil || len(got.ChildShards) == 0 {
		t.Fatalf("%+v %v", got, err)
	}

	if _, err = c.SplitShard(ctx, &kinesis.SplitShardInput{
		StreamName: aws.String("events"), ShardToSplit: open.Shards[0].ShardId, NewStartingHashKey: aws.String("1000"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.MergeShards(ctx, &kinesis.MergeShardsInput{
		StreamName: aws.String(
			"events",
		),
		ShardToMerge:         open.Shards[2].ShardId,
		AdjacentShardToMerge: open.Shards[3].ShardId,
	}); err != nil {
		t.Fatal(err)
	}

	summary, err := c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: aws.String("events")})
	if err != nil || aws.ToInt32(summary.StreamDescriptionSummary.OpenShardCount) != 4 {
		t.Fatalf("%+v %v", summary, err)
	}

	_, err = c.GetRecords(ctx, &kinesis.GetRecordsInput{ShardIterator: aws.String("garbage")})
	code(t, err, "InvalidArgumentException")
}

func TestKinesisLambdaAndFirehose(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 100)}
	s := serverWith(t, runtime)
	ctx := context.Background()
	c := kinesisClient(s)
	fh := firehoseClient(s)
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "reader", "app.echo")
	processor := createFunction(t, functions, "shout", "app.firehose")
	buckets := s3Client(s, "test", "us-east-1")
	if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("lake")}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateStream(
		ctx,
		&kinesis.CreateStreamInput{StreamName: aws.String("feed"), ShardCount: aws.Int32(1)},
	); err != nil {
		t.Fatal(err)
	}

	stream, err := c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: aws.String("feed")})
	if err != nil {
		t.Fatal(err)
	}

	streamARN := stream.StreamDescriptionSummary.StreamARN
	mapping, err := functions.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: aws.String(
			"reader",
		),
		EventSourceArn:   streamARN,
		StartingPosition: lambdatypes.EventSourcePositionLatest,
		BatchSize:        aws.Int32(10),
	})
	if err != nil || aws.ToString(mapping.EventSourceArn) != aws.ToString(streamARN) {
		t.Fatalf("%+v %v", mapping, err)
	}

	// A delivery stream reads the same Kinesis stream into S3.
	role := aws.String("arn:aws:iam::000000000000:role/firehose")
	if _, err = fh.CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
		DeliveryStreamName: aws.String(
			"from-kinesis",
		),
		DeliveryStreamType: fhtypes.DeliveryStreamTypeKinesisStreamAsSource,
		KinesisStreamSourceConfiguration: &fhtypes.KinesisStreamSourceConfiguration{
			KinesisStreamARN: streamARN,
			RoleARN:          role,
		},
		ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
			BucketARN:      aws.String("arn:aws:s3:::lake"),
			RoleARN:        role,
			Prefix:         aws.String("kinesis/"),
			BufferingHints: &fhtypes.BufferingHints{IntervalInSeconds: aws.Int32(0), SizeInMBs: aws.Int32(1)},
			ProcessingConfiguration: &fhtypes.ProcessingConfiguration{
				Enabled: aws.Bool(true),
				Processors: []fhtypes.Processor{{
					Type: fhtypes.ProcessorTypeAppendDelimiterToRecord,
				}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutRecord(ctx, &kinesis.PutRecordInput{
		StreamName: aws.String("feed"), Data: []byte(`{"n":1}`), PartitionKey: aws.String("p"),
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		var event struct {
			Records []struct {
				EventSource string
				Kinesis     struct{ Data []byte }
			}
		}
		if err = json.Unmarshal(call.payload, &event); err != nil || len(event.Records) != 1 ||
			event.Records[0].EventSource != "aws:kinesis" || string(event.Records[0].Kinesis.Data) != `{"n":1}` {
			t.Fatalf("%s %v", call.payload, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the mapping did not invoke the function")
	}

	if body := waitForObject(t, buckets, "kinesis/"); body != "{\"n\":1}\n" {
		t.Fatalf("%q", body)
	}

	// A direct-put delivery stream with a Lambda processor and GZIP.
	if _, err = fh.CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
		DeliveryStreamName: aws.String("logs"),
		ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
			BucketARN:         aws.String("arn:aws:s3:::lake"),
			RoleARN:           role,
			Prefix:            aws.String("logs/!{timestamp:yyyy}/"),
			CompressionFormat: fhtypes.CompressionFormatGzip,
			BufferingHints:    &fhtypes.BufferingHints{IntervalInSeconds: aws.Int32(0), SizeInMBs: aws.Int32(1)},
			ProcessingConfiguration: &fhtypes.ProcessingConfiguration{
				Enabled: aws.Bool(true),
				Processors: []fhtypes.Processor{{
					Type: fhtypes.ProcessorTypeLambda,
					Parameters: []fhtypes.ProcessorParameter{{
						ParameterName: fhtypes.ProcessorParameterNameLambdaArn, ParameterValue: processor.FunctionArn,
					}},
				}},
			},
		},
		Tags: []fhtypes.Tag{{Key: aws.String("team"), Value: aws.String("a")}},
	}); err != nil {
		t.Fatal(err)
	}

	put, err := fh.PutRecordBatch(
		ctx,
		&firehose.PutRecordBatchInput{DeliveryStreamName: aws.String("logs"), Records: []fhtypes.Record{
			{Data: []byte("hello ")}, {Data: []byte("drop")}, {Data: []byte("world")}, {Data: []byte("fail")},
		}},
	)
	if err != nil || aws.ToInt32(put.FailedPutCount) != 0 || len(put.RequestResponses) != 4 {
		t.Fatalf("%+v %v", put, err)
	}

	year := time.Now().UTC().Format("2006")
	if body := waitForObject(t, buckets, "logs/"+year+"/"); body != "HELLO WORLD" {
		t.Fatalf("%q", body)
	}

	if body := waitForObject(
		t,
		buckets,
		"logs/"+year+"/processing-failed/",
	); !strings.Contains(
		body,
		"ProcessingFailedStatus",
	) {
		t.Fatalf("%q", body)
	}

	_, err = fh.PutRecord(ctx, &firehose.PutRecordInput{
		DeliveryStreamName: aws.String("from-kinesis"), Record: &fhtypes.Record{Data: []byte("x")},
	})
	code(t, err, "InvalidArgumentException")

	described, err := fh.DescribeDeliveryStream(
		ctx,
		&firehose.DescribeDeliveryStreamInput{DeliveryStreamName: aws.String("logs")},
	)
	if err != nil || described.DeliveryStreamDescription.DeliveryStreamStatus != fhtypes.DeliveryStreamStatusActive ||
		aws.ToString(described.DeliveryStreamDescription.VersionId) != "1" {
		t.Fatalf("%+v %v", described, err)
	}

	if _, err = fh.UpdateDestination(ctx, &firehose.UpdateDestinationInput{
		DeliveryStreamName: aws.String("logs"), CurrentDeliveryStreamVersionId: aws.String("1"),
		DestinationId:               described.DeliveryStreamDescription.Destinations[0].DestinationId,
		ExtendedS3DestinationUpdate: &fhtypes.ExtendedS3DestinationUpdate{Prefix: aws.String("other/")},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = fh.UpdateDestination(ctx, &firehose.UpdateDestinationInput{
		DeliveryStreamName: aws.String("logs"), CurrentDeliveryStreamVersionId: aws.String("1"),
		DestinationId:               described.DeliveryStreamDescription.Destinations[0].DestinationId,
		ExtendedS3DestinationUpdate: &fhtypes.ExtendedS3DestinationUpdate{Prefix: aws.String("again/")},
	})
	code(t, err, "ConcurrentModificationException")

	names, err := fh.ListDeliveryStreams(ctx, &firehose.ListDeliveryStreamsInput{})
	if err != nil || len(names.DeliveryStreamNames) != 2 {
		t.Fatalf("%+v %v", names, err)
	}

	if _, err = fh.DeleteDeliveryStream(
		ctx,
		&firehose.DeleteDeliveryStreamInput{DeliveryStreamName: aws.String("logs")},
	); err != nil {
		t.Fatal(err)
	}
}

// waitForObject waits for an object under prefix and returns its content,
// gunzipped when it is compressed.
func waitForObject(t *testing.T, c *s3.Client, prefix string) string {
	t.Helper()
	ctx := context.Background()
	for range 100 {
		listed, err := c.ListObjectsV2(
			ctx,
			&s3.ListObjectsV2Input{Bucket: aws.String("lake"), Prefix: aws.String(prefix)},
		)
		if err != nil {
			t.Fatal(err)
		}

		for _, o := range listed.Contents {
			key := aws.ToString(o.Key)
			// A deeper prefix, such as the error output, is another object's.
			if strings.Contains(strings.TrimPrefix(key, prefix), "processing-failed/") {
				continue
			}

			got, getErr := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("lake"), Key: o.Key})
			if getErr != nil {
				t.Fatal(getErr)
			}

			data, readErr := io.ReadAll(got.Body)
			_ = got.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}

			if strings.HasSuffix(key, ".gz") {
				r, gzErr := gzip.NewReader(bytes.NewReader(data))
				if gzErr != nil {
					t.Fatal(gzErr)
				}

				if data, readErr = io.ReadAll(r); readErr != nil {
					t.Fatal(readErr)
				}
			}

			return string(data)
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("no object under %s", prefix)
	return ""
}

func TestKinesisStreamEncryptionKeys(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := kinesisClient(s)
	keys := kmsClient(s, "test", "us-east-1")
	ctx := context.Background()

	if _, err := c.CreateStream(ctx, &kinesis.CreateStreamInput{
		StreamName: new("secure"), ShardCount: new(int32(1)),
	}); err != nil {
		t.Fatal(err)
	}

	start := func(key string) error {
		_, err := c.StartStreamEncryption(ctx, &kinesis.StartStreamEncryptionInput{
			StreamName: new("secure"), EncryptionType: kintypes.EncryptionTypeKms, KeyId: new(key),
		})

		return err
	}

	code(t, start("update"), "KMSNotFoundException")

	live, err := keys.CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	off, err := keys.CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = keys.DisableKey(ctx, &kms.DisableKeyInput{KeyId: off.KeyMetadata.KeyId}); err != nil {
		t.Fatal(err)
	}

	code(t, start(*off.KeyMetadata.KeyId), "KMSDisabledException")

	for _, key := range []string{*live.KeyMetadata.KeyId, *live.KeyMetadata.Arn, "alias/aws/kinesis"} {
		if err = start(key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
}

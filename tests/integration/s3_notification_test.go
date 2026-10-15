package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
)

// receiveOne returns the body of the one message waiting in the queue, or ""
// when there is none.
func receiveOne(t *testing.T, c *sqs.Client, queueURL string) string {
	t.Helper()
	out, err := c.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	switch len(out.Messages) {
	case 0:
		return ""
	case 1:
		_, err = c.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
			QueueUrl: aws.String(queueURL), ReceiptHandle: out.Messages[0].ReceiptHandle,
		})
		if err != nil {
			t.Fatal(err)
		}

		return aws.ToString(out.Messages[0].Body)
	default:
		t.Fatalf("%d messages waiting", len(out.Messages))
		return ""
	}
}

// s3Record decodes the one record of an S3 event message.
type s3Record struct {
	EventName string
	S3        struct {
		ConfigurationID string `json:"configurationId"`
		Bucket          struct{ Name string }
		Object          struct {
			Key       string
			ETag      string `json:"eTag"`
			VersionID string `json:"versionId"`
			Size      int64
		}
	}
}

func decodeRecord(t *testing.T, body string) s3Record {
	t.Helper()
	var message struct{ Records []s3Record }
	if err := json.Unmarshal([]byte(body), &message); err != nil || len(message.Records) != 1 {
		t.Fatalf("event message %q: %v", body, err)
	}

	return message.Records[0]
}

func TestS3Notifications(t *testing.T) {
	s3NotificationsScenario(t, false)
}

func TestS3NotificationsEnforced(t *testing.T) {
	s3NotificationsScenario(t, true)
}

func s3NotificationsScenario(t *testing.T, enforce bool) {
	ctx := context.Background()
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	serverCfg := testConfig()
	if enforce {
		serverCfg.Identity.Authorization = config.AuthorizationEnforce
		serverCfg.Identity.Accounts["test"] = serverCfg.Defaults.Account
	}

	s := serverConfig(t, serverCfg, runtime)
	c := s3Client(s, "test", "us-east-1")
	queues := sqsClient(s, "test")
	uploadsURL, uploadsARN := queueARN(t, queues, "uploads")
	eventsURL, eventsARN := queueARN(t, queues, "bucket-events")
	function := createFunction(t, lambdaClient(s, "test"), "thumbnails", "app.echo")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}

	filter := func(prefix, suffix string) *s3types.NotificationConfigurationFilter {
		return &s3types.NotificationConfigurationFilter{Key: &s3types.S3KeyFilter{FilterRules: []s3types.FilterRule{
			{Name: s3types.FilterRuleNamePrefix, Value: aws.String(prefix)},
			{Name: s3types.FilterRuleNameSuffix, Value: aws.String(suffix)},
		}}}
	}

	put := func(cfg *s3types.NotificationConfiguration) error {
		_, err := c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
			Bucket: aws.String("photos"), NotificationConfiguration: cfg,
		})
		return err
	}

	cfg := &s3types.NotificationConfiguration{
		QueueConfigurations: []s3types.QueueConfiguration{{
			Id:       aws.String("raw-uploads"),
			QueueArn: aws.String(uploadsARN),
			Events:   []s3types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"},
			Filter:   filter("raw/", ".jpg"),
		}},
		LambdaFunctionConfigurations: []s3types.LambdaFunctionConfiguration{{
			LambdaFunctionArn: function.FunctionArn,
			Events:            []s3types.Event{"s3:ObjectCreated:Put"},
			Filter:            filter("thumbs/", ""),
		}},
		EventBridgeConfiguration: &s3types.EventBridgeConfiguration{},
	}

	if err := put(cfg); err != nil {
		t.Fatal(err)
	}

	// Saving the configuration sends the queue a test event.
	if body := receiveOne(t, queues, uploadsURL); !strings.Contains(body, `"s3:TestEvent"`) {
		t.Fatalf("test event = %q", body)
	}

	got, err := c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{
		Bucket: aws.String("photos"),
	})
	if err != nil || len(got.QueueConfigurations) != 1 || len(got.LambdaFunctionConfigurations) != 1 ||
		got.EventBridgeConfiguration == nil || aws.ToString(got.LambdaFunctionConfigurations[0].Id) == "" ||
		*got.QueueConfigurations[0].QueueArn != uploadsARN || len(got.QueueConfigurations[0].Events) != 2 {
		t.Fatalf("configuration = %+v %v", got, err)
	}

	// EventBridge receives every object event of the bucket.
	rules := eventsClient(s, "test")
	putRule(t, rules, "photos", `{"source":["aws.s3"],"detail":{"bucket":{"name":["photos"]}}}`)
	if _, err = rules.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("photos"), Targets: []ebtypes.Target{{Id: aws.String("q"), Arn: aws.String(eventsARN)}},
	}); err != nil {
		t.Fatal(err)
	}

	putObject := func(key string) {
		t.Helper()
		if _, putErr := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("photos"), Key: aws.String(key), Body: bytes.NewReader([]byte("image")),
		}); putErr != nil {
			t.Fatal(putErr)
		}
	}

	putObject("raw/cat photo.jpg")
	r := decodeRecord(t, receiveOne(t, queues, uploadsURL))
	if r.EventName != "ObjectCreated:Put" || r.S3.ConfigurationID != "raw-uploads" || r.S3.Bucket.Name != "photos" ||
		r.S3.Object.Key != "raw%2Fcat+photo.jpg" || r.S3.Object.Size != 5 || r.S3.Object.ETag == "" {
		t.Fatalf("record = %+v", r)
	}

	var event struct {
		DetailType string `json:"detail-type"`
		Source     string
		Detail     struct {
			Object struct{ Key string }
			Reason string
		}
	}

	if err = json.Unmarshal([]byte(receiveOne(t, queues, eventsURL)), &event); err != nil ||
		event.DetailType != "Object Created" || event.Source != "aws.s3" ||
		event.Detail.Object.Key != "raw/cat photo.jpg" || event.Detail.Reason != "PutObject" {
		t.Fatalf("EventBridge event = %+v %v", event, err)
	}

	// Keys outside the filters reach only EventBridge.
	putObject("raw/notes.txt")
	putObject("thumbs/cat.jpg")
	if body := receiveOne(t, queues, uploadsURL); body != "" {
		t.Fatalf("filtered key delivered: %q", body)
	}

	select {
	case call := <-runtime.calls:
		if r = decodeRecord(t, string(call.payload)); r.S3.Object.Key != "thumbs%2Fcat.jpg" {
			t.Fatalf("function record = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("function was not invoked")
	}

	// A completed multipart upload is one creation event.
	upload, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("photos"), Key: aws.String("raw/big.jpg"),
	})
	if err != nil {
		t.Fatal(err)
	}

	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("photos"), Key: aws.String("raw/big.jpg"), UploadId: upload.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("part")),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("photos"), Key: aws.String("raw/big.jpg"), UploadId: upload.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.ETag},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if r = decodeRecord(t, receiveOne(t, queues, uploadsURL)); r.EventName != "ObjectCreated:CompleteMultipartUpload" {
		t.Fatalf("multipart record = %+v", r)
	}

	// Deleting an object notifies; deleting a missing key does not.
	for _, key := range []string{"raw/cat photo.jpg", "raw/missing.jpg"} {
		if _, err = c.DeleteObject(
			ctx,
			&s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String(key)},
		); err != nil {
			t.Fatal(err)
		}
	}

	if r = decodeRecord(t, receiveOne(t, queues, uploadsURL)); r.EventName != "ObjectRemoved:Delete" ||
		r.S3.Object.Size != 0 {
		t.Fatalf("delete record = %+v", r)
	}

	if body := receiveOne(t, queues, uploadsURL); body != "" {
		t.Fatalf("missing key delete delivered: %q", body)
	}

	// Invalid configurations are rejected and leave the saved one in place.
	for name, bad := range map[string]*s3types.NotificationConfiguration{
		"overlap": {QueueConfigurations: []s3types.QueueConfiguration{
			{QueueArn: aws.String(uploadsARN), Events: []s3types.Event{"s3:ObjectCreated:*"}, Filter: filter("raw/", "")},
			{QueueArn: aws.String(eventsARN), Events: []s3types.Event{"s3:ObjectCreated:Put"}, Filter: filter("raw/a", "")},
		}},
		"unsupported event": {QueueConfigurations: []s3types.QueueConfiguration{
			{QueueArn: aws.String(uploadsARN), Events: []s3types.Event{"s3:ObjectRestore:*"}},
		}},
		"missing queue": {QueueConfigurations: []s3types.QueueConfiguration{{
			QueueArn: aws.String(strings.Replace(uploadsARN, "uploads", "nope", 1)),
			Events:   []s3types.Event{"s3:ObjectCreated:*"},
		}}},
		"other region": {QueueConfigurations: []s3types.QueueConfiguration{{
			QueueArn: aws.String(strings.Replace(uploadsARN, "us-east-1", "eu-west-1", 1)),
			Events:   []s3types.Event{"s3:ObjectCreated:*"},
		}}},
	} {
		if code, _ := s3Code(put(bad)); code != "InvalidArgument" {
			t.Fatalf("%s: %s", name, code)
		}
	}

	// An empty configuration turns notifications off.
	if err = put(&s3types.NotificationConfiguration{}); err != nil {
		t.Fatal(err)
	}

	got, err = c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{
		Bucket: aws.String("photos"),
	})
	if err != nil || len(got.QueueConfigurations) != 0 || got.EventBridgeConfiguration != nil {
		t.Fatalf("after clearing = %+v %v", got, err)
	}

	putObject("raw/after.jpg")
	if body := receiveOne(t, queues, uploadsURL); body != "" {
		t.Fatalf("delivered after clearing: %q", body)
	}
}

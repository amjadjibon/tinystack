package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func waitS3(t *testing.T, what string, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-poll.C:
		}
	}
}

func setS3Lifecycle(t *testing.T, c *s3.Client, bucket *string, rules ...s3types.LifecycleRule) {
	t.Helper()
	_, err := c.PutBucketLifecycleConfiguration(context.Background(), &s3.PutBucketLifecycleConfigurationInput{
		Bucket: bucket, LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestS3LifecycleActions(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Storage.S3LifecycleDay = 300 * time.Millisecond
	srv := serverConfig(t, cfg, &fakeRuntime{})
	c, bucket := checksumBucket(t, srv)
	queues := sqsClient(srv, "test")
	queueURL, arn := queueARN(t, queues, "lifecycle-events")
	_, err := c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: bucket, NotificationConfiguration: &s3types.NotificationConfiguration{
			QueueConfigurations: []s3types.QueueConfiguration{{
				QueueArn: &arn,
				Events:   []s3types.Event{s3types.EventS3LifecycleExpiration, s3types.EventS3LifecycleTransition},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_ = receiveOne(t, queues, queueURL)
	enabled := s3types.ExpirationStatusEnabled
	setS3Lifecycle(t, c, bucket,
		s3types.LifecycleRule{
			ID: new("expire rule"), Status: enabled,
			Filter: &s3types.LifecycleRuleFilter{And: &s3types.LifecycleRuleAndOperator{
				Prefix: new("tmp/"), Tags: []s3types.Tag{{Key: new("expire"), Value: new("true")}},
				ObjectSizeGreaterThan: new(int64(4)), ObjectSizeLessThan: new(int64(10)),
			}},
			Expiration: &s3types.LifecycleExpiration{Days: new(int32(1))},
		},
		s3types.LifecycleRule{
			Status: enabled, Filter: &s3types.LifecycleRuleFilter{Prefix: new("abort/")},
			AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: new(int32(1))},
		},
		s3types.LifecycleRule{
			Status: enabled,
			Filter: &s3types.LifecycleRuleFilter{ObjectSizeGreaterThan: new(int64(10))},
			Transitions: []s3types.Transition{
				{Days: new(int32(0)), StorageClass: s3types.TransitionStorageClassGlacier},
			},
		},
		s3types.LifecycleRule{
			Status: s3types.ExpirationStatusDisabled, Filter: &s3types.LifecycleRuleFilter{},
			Expiration: &s3types.LifecycleExpiration{Days: new(int32(1))},
		},
	)

	put, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: new("tmp/expire"), Body: strings.NewReader("12345"), Tagging: new("expire=true"),
	})
	if err != nil || put.Expiration == nil || !strings.Contains(*put.Expiration, `rule-id="expire%20rule"`) {
		t.Fatalf("expiration header on upload: %v %v", put, err)
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: new("tmp/expire")})
	if err != nil || head.Expiration == nil || *head.Expiration != *put.Expiration {
		t.Fatalf("expiration header on head: %v %v", head, err)
	}

	get, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: new("tmp/expire")})
	if err != nil {
		t.Fatal(err)
	}

	_ = get.Body.Close()
	if get.Expiration == nil || *get.Expiration != *put.Expiration {
		t.Fatalf("expiration header on get: %v", get.Expiration)
	}

	putObject(t, c, *bucket, "tmp/keep", "12345", nil)
	putObject(t, c, *bucket, "transition", "12345678901", nil)
	upload, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: new("abort/part")})
	if err != nil {
		t.Fatal(err)
	}

	waitS3(t, "expire, transition and abort", func() bool {
		_, expired := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: new("tmp/expire")})
		transitioned, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: new("transition")})
		_, aborted := c.ListParts(
			ctx,
			&s3.ListPartsInput{Bucket: bucket, Key: new("abort/part"), UploadId: upload.UploadId},
		)
		_, status := s3Code(expired)
		abortCode, _ := s3Code(aborted)
		return status == http.StatusNotFound && err == nil &&
			transitioned.StorageClass == s3types.StorageClassGlacier &&
			abortCode == "NoSuchUpload"
	})

	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: new("tmp/keep")}); err != nil {
		t.Fatal(err)
	}

	names := map[string]string{}
	waitS3(t, "lifecycle notifications", func() bool {
		out, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, MaxNumberOfMessages: 10,
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, message := range out.Messages {
			event := decodeRecord(t, *message.Body)
			names[event.S3.Object.Key] = event.EventName
		}

		return len(names) == 2
	})

	if names["tmp%2Fexpire"] != "LifecycleExpiration:Delete" || names["transition"] != "LifecycleTransition" {
		t.Fatalf("events = %v", names)
	}
}

func TestS3LifecycleVersions(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Storage.S3LifecycleDay = 100 * time.Millisecond
	c := s3Client(serverConfig(t, cfg, &fakeRuntime{}), "test", "us-east-1")
	bucket := new("lifecycle-versions")
	versioned(t, c, *bucket)
	old := putVersion(t, c, *bucket, "history", "old")
	newestNoncurrent := putVersion(t, c, *bucket, "history", "newer")
	latest := putVersion(t, c, *bucket, "history", "latest")
	putVersion(t, c, *bucket, "marker", "live")
	enabled := s3types.ExpirationStatusEnabled
	setS3Lifecycle(t, c, bucket,
		s3types.LifecycleRule{
			ID: new("history"), Status: enabled, Filter: &s3types.LifecycleRuleFilter{Prefix: new("history")},
			NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{
				NoncurrentDays: new(int32(1)), NewerNoncurrentVersions: new(int32(1)),
			},
		},
		s3types.LifecycleRule{
			ID: new("class"), Status: enabled,
			Filter: &s3types.LifecycleRuleFilter{And: &s3types.LifecycleRuleAndOperator{
				Prefix: new("history"), ObjectSizeGreaterThan: new(int64(0)),
			}},
			NoncurrentVersionTransitions: []s3types.NoncurrentVersionTransition{{
				NoncurrentDays: new(int32(1)), StorageClass: s3types.TransitionStorageClassGlacierIr,
			}},
		},
		s3types.LifecycleRule{
			ID: new("mark"), Status: enabled, Filter: &s3types.LifecycleRuleFilter{Prefix: new("marker")},
			Expiration: &s3types.LifecycleExpiration{Date: new(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))},
		},
		s3types.LifecycleRule{
			ID: new("clean-marker"), Status: enabled, Filter: &s3types.LifecycleRuleFilter{Prefix: new("marker")},
			Expiration:                  &s3types.LifecycleExpiration{ExpiredObjectDeleteMarker: new(true)},
			NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{NoncurrentDays: new(int32(1))},
		},
	)

	waitS3(t, "noncurrent version expiration and transition", func() bool {
		versions, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket, Prefix: new("history")})
		if err != nil {
			t.Fatal(err)
		}

		return len(versions.Versions) == 2 && *versions.Versions[0].VersionId == latest &&
			*versions.Versions[1].VersionId == newestNoncurrent &&
			versions.Versions[1].StorageClass == "GLACIER_IR"
	})

	if _, err := c.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: bucket, Key: new("history"), VersionId: &old},
	); err == nil {
		t.Fatal("expired version remained readable")
	}

	waitS3(t, "expired object and its lone delete marker removed", func() bool {
		versions, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket, Prefix: new("marker")})
		return err == nil && len(versions.Versions) == 0 && len(versions.DeleteMarkers) == 0
	})
}

package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3EverydayLogging(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	bucket, target := "logging-source", "logging-target"
	for _, name := range []string{bucket, target} {
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)}); err != nil {
			t.Fatal(err)
		}
	}

	enabled := &s3types.LoggingEnabled{
		TargetBucket: new(target),
		TargetPrefix: new("logs/"),
		TargetObjectKeyFormat: &s3types.TargetObjectKeyFormat{
			PartitionedPrefix: &s3types.PartitionedPrefix{PartitionDateSource: "EventTime"},
		},
	}
	_, err := c.PutBucketLogging(ctx, &s3.PutBucketLoggingInput{
		Bucket:              new(bucket),
		BucketLoggingStatus: &s3types.BucketLoggingStatus{LoggingEnabled: enabled},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: new(bucket)})
	if err != nil || got.LoggingEnabled == nil || *got.LoggingEnabled.TargetPrefix != "logs/" ||
		got.LoggingEnabled.TargetObjectKeyFormat.PartitionedPrefix.PartitionDateSource != "EventTime" {
		t.Fatalf("logging: %+v %v", got, err)
	}

	enabled.TargetBucket = new("missing-target")
	_, err = c.PutBucketLogging(ctx, &s3.PutBucketLoggingInput{
		Bucket:              new(bucket),
		BucketLoggingStatus: &s3types.BucketLoggingStatus{LoggingEnabled: enabled},
	})
	if code, _ := s3Code(err); code != "InvalidTargetBucketForLogging" {
		t.Fatalf("target: %v", err)
	}

	_, err = c.PutBucketLogging(ctx, &s3.PutBucketLoggingInput{
		Bucket:              new(bucket),
		BucketLoggingStatus: &s3types.BucketLoggingStatus{},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err = c.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: new(bucket)})
	if err != nil || got.LoggingEnabled != nil {
		t.Fatalf("disabled logging: %+v %v", got, err)
	}
}

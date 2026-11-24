package integration_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3AccessLogDelivery(t *testing.T) {
	ctx := context.Background()
	c := s3Client(server(t), "test", "us-east-1")
	source, target, sink := "access-log-source", "access-log-target", "access-log-sink"
	for _, name := range []string{source, target, sink} {
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)}); err != nil {
			t.Fatal(err)
		}
	}

	configure := func(bucket, target, prefix string, format *s3types.TargetObjectKeyFormat) {
		t.Helper()
		_, err := c.PutBucketLogging(ctx, &s3.PutBucketLoggingInput{
			Bucket: &bucket, BucketLoggingStatus: &s3types.BucketLoggingStatus{
				LoggingEnabled: &s3types.LoggingEnabled{
					TargetBucket: &target, TargetPrefix: &prefix,
					TargetObjectKeyFormat: format,
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	configure(target, sink, "nested/", nil)
	configure(source, target, "logs/", &s3types.TargetObjectKeyFormat{
		PartitionedPrefix: &s3types.PartitionedPrefix{PartitionDateSource: s3types.PartitionDateSourceEventTime},
	})

	putObject(t, c, source, "key.txt", "body", nil)
	_, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &source, Key: new("missing.txt")})
	var missing *s3types.NoSuchKey
	if !errors.As(err, &missing) {
		t.Fatal(err)
	}

	readLogs := func(bucket, prefix string, found map[string]string) string {
		t.Helper()
		list, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range list.Contents {
			key := *entry.Key
			if _, seen := found[key]; !seen {
				out, getErr := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
				if getErr != nil {
					t.Fatal(getErr)
				}

				text, readErr := io.ReadAll(out.Body)
				_ = out.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}

				found[key] = string(text)
			}
		}

		var output strings.Builder
		for _, text := range found {
			output.WriteString(text)
		}

		return output.String()
	}

	logs := map[string]string{}
	waitS3(t, "source access log objects", func() bool {
		text := readLogs(target, "logs/000000000000/us-east-1/"+source+"/", logs)
		return strings.Contains(text, "REST.PUT.OBJECT key.txt") &&
			strings.Contains(text, "REST.GET.OBJECT missing.txt") &&
			strings.Contains(text, "404 NoSuchKey")
	})
	for key := range logs {
		if !strings.Contains(key, "-00-00-00-") {
			t.Fatalf("EventTime log key did not use midnight: %q", key)
		}
	}

	nested := map[string]string{}
	waitS3(t, "log of client requests to the destination", func() bool {
		text := readLogs(sink, "nested/", nested)
		if strings.Contains(text, "REST.PUT.OBJECT") {
			t.Fatalf("delivery recursively logged its own write: %s", text)
		}

		return strings.Contains(text, "REST.GET.OBJECT")
	})
}

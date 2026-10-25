package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	"github.com/aws/aws-sdk-go-v2/service/s3control/types"
	"github.com/aws/smithy-go"
)

func s3ControlClient(s *httptest.Server) *s3control.Client {
	// The SDK prefixes the host with the account ID, which resolves only under localhost.
	return s3control.New(s3control.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(strings.Replace(s.URL, "127.0.0.1", "localhost", 1)),
		Credentials:  creds("test"),
		Retryer:      aws.NopRetryer{},
	})
}

func TestS3ControlBucketTags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := s3ControlClient(s)
	buckets := s3Client(s, "test", "us-east-1")
	name, arn, account := new("shared-tags"), new("arn:aws:s3:::shared-tags"), new("000000000000")
	if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: name, CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
			Tags: []s3types.Tag{{Key: new("created"), Value: new("yes")}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	initial, err := buckets.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: name})
	if err != nil || len(initial.TagSet) != 1 || *initial.TagSet[0].Value != "yes" {
		t.Fatalf("create tags: %+v %v", initial, err)
	}

	if _, err = c.TagResource(ctx, &s3control.TagResourceInput{
		AccountId: account, ResourceArn: arn, Tags: []types.Tag{{Key: new("control"), Value: new("yes")}},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.ListTagsForResource(ctx, &s3control.ListTagsForResourceInput{AccountId: account, ResourceArn: arn})
	if err != nil || len(got.Tags) != 2 {
		t.Fatalf("merged tags: %+v %v", got, err)
	}

	fromS3, err := buckets.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: name})
	if err != nil || len(fromS3.TagSet) != 2 {
		t.Fatalf("shared tags: %+v %v", fromS3, err)
	}

	if _, err = buckets.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: name, Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: new("replaced"), Value: new("yes")}}},
	}); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = s3ControlClient(s)
	buckets = s3Client(s, "test", "us-east-1")
	got, err = c.ListTagsForResource(ctx, &s3control.ListTagsForResourceInput{AccountId: account, ResourceArn: arn})
	if err != nil || len(got.Tags) != 1 || *got.Tags[0].Key != "replaced" {
		t.Fatalf("restart: %+v %v", got, err)
	}

	if _, err = c.UntagResource(ctx, &s3control.UntagResourceInput{
		AccountId: account, ResourceArn: arn, TagKeys: []string{"replaced", "absent"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.ListTagsForResource(ctx, &s3control.ListTagsForResourceInput{AccountId: account, ResourceArn: arn})
	if err != nil || len(got.Tags) != 0 {
		t.Fatalf("empty tags: %+v %v", got, err)
	}

	for _, tc := range []struct{ arn, account, code string }{
		{"arn:aws:s3:::missing-bucket", "000000000000", "NoSuchBucket"},
		{"arn:aws:s3:us-east-1:000000000000:accesspoint/test", "000000000000", "NoSuchAccessPoint"},
		{*arn, "111111111111", "AccessDenied"},
	} {
		_, err = c.ListTagsForResource(
			ctx,
			&s3control.ListTagsForResourceInput{AccountId: new(tc.account), ResourceArn: new(tc.arn)},
		)
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != tc.code {
			t.Fatalf("%s: %v", tc.arn, err)
		}
		_, err = c.TagResource(ctx, &s3control.TagResourceInput{
			AccountId: new(
				tc.account,
			),
			ResourceArn: new(tc.arn),
			Tags:        []types.Tag{{Key: new("key"), Value: new("value")}},
		})
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != tc.code {
			t.Fatalf("tag %s: %v", tc.arn, err)
		}

		_, err = c.UntagResource(ctx, &s3control.UntagResourceInput{
			AccountId: new(tc.account), ResourceArn: new(tc.arn), TagKeys: []string{"key"},
		})
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != tc.code {
			t.Fatalf("untag %s: %v", tc.arn, err)
		}

	}

	// S3 paths using the same literal version name must remain S3 requests.
	if _, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("v20180820")}); err != nil {
		t.Fatal(err)
	}

	if _, err = buckets.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: new("v20180820")}); err != nil {
		t.Fatal(err)
	}
}

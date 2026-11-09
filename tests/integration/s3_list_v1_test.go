package integration_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3EverydayListV1(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	bucket := "list-everyday"
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(bucket)}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"a +", "dir/a", "dir/b", "z"} {
		if _, err := c.PutObject(
			ctx,
			&s3.PutObjectInput{Bucket: new(bucket), Key: new(key), Body: bytes.NewReader(nil)},
		); err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: new(bucket), MaxKeys: new(int32(1)),
		EncodingType: "url",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !*first.IsTruncated || first.NextMarker != nil || first.Contents[0].Owner == nil ||
		*first.Contents[0].Key != "a%20%2B" {
		t.Fatalf("first page: %+v", first)
	}

	next, err := c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: new(bucket), Marker: new("a +")})
	if err != nil || len(next.Contents) != 3 {
		t.Fatalf("next page: %+v %v", next, err)
	}

	grouped, err := c.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: new(bucket), Delimiter: new("/"),
		Marker: new("a +"), MaxKeys: new(int32(1)), EncodingType: "url",
	})
	if err != nil || len(grouped.CommonPrefixes) != 1 || *grouped.NextMarker != "dir%2F" {
		t.Fatalf("group page: %+v %v", grouped, err)
	}

	last, err := c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: new(bucket), Delimiter: new("/"), Marker: new("dir/")})
	if err != nil || len(last.Contents) != 1 || *last.Contents[0].Key != "z" || *last.IsTruncated {
		t.Fatalf("last page: %+v %v", last, err)
	}
}

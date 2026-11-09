package integration_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3EverydayVersions(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	bucket, key := "attributes-versions", "key"
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(bucket)}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  new(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: "Enabled"},
	}); err != nil {
		t.Fatal(err)
	}

	put, err := c.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new(bucket), Key: new(key), Body: bytes.NewReader([]byte("original"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: new(bucket), Key: new(key)})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		version *string
		code    string
		status  int
	}{
		{nil, "NoSuchKey", 404}, {deleted.VersionId, "MethodNotAllowed", 405}, {new("missing"), "NoSuchVersion", 404},
	} {
		_, err = c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
			Bucket: new(bucket), Key: new(key),
			VersionId: tc.version, ObjectAttributes: []s3types.ObjectAttributes{"ObjectSize"},
		})
		if code, status := s3Code(err); code != tc.code || status != tc.status {
			t.Fatalf("version error: %v", err)
		}
	}

	attrs, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: new(bucket), Key: new(key),
		VersionId: put.VersionId, ObjectAttributes: []s3types.ObjectAttributes{"ObjectSize"},
	})
	if err != nil || *attrs.ObjectSize != 8 || attrs.ETag != nil || *attrs.VersionId != *put.VersionId {
		t.Fatalf("selected version: %+v %v", attrs, err)
	}

	upload, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: new(bucket), Key: new("copy")})
	if err != nil {
		t.Fatal(err)
	}

	copied, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket:     new(bucket),
		Key:        new("copy"),
		UploadId:   upload.UploadId,
		PartNumber: new(int32(1)),
		CopySource: new(bucket + "/" + key + "?versionId=" + *put.VersionId),
	})
	if err != nil || *copied.CopySourceVersionId != *put.VersionId {
		t.Fatalf("version copy: %+v %v", copied, err)
	}

	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: new(bucket), Key: new("copy"),
		UploadId: upload.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{
			ETag: copied.CopyPartResult.ETag, PartNumber: new(int32(1)),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: new(bucket), Key: new("copy")})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil || string(body) != "original" {
		t.Fatalf("whole copy: %q %v", body, err)
	}
}

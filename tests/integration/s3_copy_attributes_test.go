package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3EverydayCopyAttributes(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	source, target, key := "copy-source", "copy-target", "space +/object"
	for _, bucket := range []string{source, target} {
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(bucket)}); err != nil {
			t.Fatal(err)
		}
	}

	data := bytes.Repeat([]byte("0123456789abcdef"), 600000)
	put, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: new(source), Key: new(key), Body: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}

	upload, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: new(target), Key: new(key)})
	if err != nil {
		t.Fatal(err)
	}

	copySource := source + "/" + url.PathEscape(key)
	var parts []s3types.CompletedPart
	for i, interval := range []string{"bytes=0-5242879", "bytes=5242880-9599999"} {
		out, copyErr := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket: new(target), Key: new(key),
			UploadId: upload.UploadId, PartNumber: new(int32(i + 1)), CopySource: new(copySource + "?versionId=null"),
			CopySourceRange: new(interval), CopySourceIfMatch: put.ETag,
		})
		if copyErr != nil {
			t.Fatal(copyErr)
		}

		if out.CopySourceVersionId == nil || *out.CopySourceVersionId != "null" {
			t.Fatalf("copy source version: %+v", out)
		}

		if out.CopyPartResult.LastModified == nil {
			t.Fatal("missing modification time")
		}

		parts = append(parts, s3types.CompletedPart{PartNumber: new(int32(i + 1)), ETag: out.CopyPartResult.ETag})
	}

	for _, tc := range []struct{ source, interval, match, code string }{
		{copySource, "bytes=99-1", "", "InvalidArgument"},
		{copySource, "bytes=0-9600000", "", "InvalidArgument"},
		{copySource, "bytes=-2", "", "InvalidArgument"},
		{source + "/missing", "", "", "NoSuchKey"},
		{copySource, "", "wrong", "PreconditionFailed"},
	} {
		input := &s3.UploadPartCopyInput{
			Bucket: new(target), Key: new(key), UploadId: upload.UploadId,
			PartNumber: new(int32(3)), CopySource: new(tc.source),
		}
		if tc.interval != "" {
			input.CopySourceRange = new(tc.interval)
		}

		if tc.match != "" {
			input.CopySourceIfMatch = new(tc.match)
		}

		_, err = c.UploadPartCopy(ctx, input)
		if code, _ := s3Code(err); code != tc.code {
			t.Fatalf("copy error: %v, want %s", err, tc.code)
		}
	}

	_, err = c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket: new(target), Key: new(key),
		UploadId: upload.UploadId, PartNumber: new(int32(3)), CopySource: new(copySource),
		CopySourceIfModifiedSince: new(time.Now().Add(time.Hour)),
	})
	if code, _ := s3Code(err); code != "PreconditionFailed" {
		t.Fatalf("date condition: %v", err)
	}

	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: new(target), Key: new(key),
		UploadId: upload.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: new(target), Key: new(key)})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil || !bytes.Equal(data, body) {
		t.Fatal("multipart copy changed bytes", err)
	}

	attrs, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: new(target), Key: new(key),
		ObjectAttributes: []s3types.ObjectAttributes{"ETag", "Checksum", "ObjectParts", "StorageClass", "ObjectSize"},
		MaxParts:         new(int32(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if *attrs.ObjectSize != int64(len(data)) || attrs.LastModified == nil || attrs.Checksum == nil ||
		attrs.ETag == nil || attrs.StorageClass != "STANDARD" || len(attrs.ObjectParts.Parts) != 1 ||
		!*attrs.ObjectParts.IsTruncated || *attrs.ObjectParts.TotalPartsCount != 2 {
		t.Fatalf("attributes: %+v", attrs)
	}

	next, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: new(target), Key: new(key),
		ObjectAttributes: []s3types.ObjectAttributes{"ObjectParts"}, PartNumberMarker: new("1"),
	})
	if err != nil || len(next.ObjectParts.Parts) != 1 || *next.ObjectParts.Parts[0].PartNumber != 2 {
		t.Fatalf("part page: %+v, %v", next, err)
	}

	// Whole-object copy uses the same part path; a single final part has no minimum size.
	whole, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket: new(target), Key: new("whole"),
		UploadId: upload.UploadId, PartNumber: new(int32(1)), CopySource: new(copySource),
	})
	if code, _ := s3Code(err); code != "NoSuchUpload" {
		t.Fatalf("completed upload accepted: %+v %v", whole, err)
	}
}

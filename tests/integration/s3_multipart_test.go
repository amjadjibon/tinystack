package integration_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3Multipart(t *testing.T) {
	ctx := context.Background()
	c := s3Client(server(t), "test", "us-east-1")
	bucket, key := aws.String("multipart-bucket"), aws.String("combined")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: key, ContentType: aws.String("text/plain"), Metadata: map[string]string{"owner": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := created.UploadId
	if _, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key}); err == nil {
		t.Fatal("unfinished upload is visible")
	}
	first := bytes.Repeat([]byte("a"), 5<<20)
	p1, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: id, PartNumber: aws.Int32(1), Body: bytes.NewReader(first),
	})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: id, PartNumber: aws.Int32(2), Body: strings.NewReader("tail"),
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: bucket, Key: key, UploadId: id, MaxParts: aws.Int32(1)})
	if err != nil || len(listed.Parts) != 1 || !aws.ToBool(listed.IsTruncated) {
		t.Fatalf("parts: %+v %v", listed, err)
	}
	nextParts, err := c.ListParts(ctx, &s3.ListPartsInput{
		Bucket: bucket, Key: key, UploadId: id, PartNumberMarker: listed.NextPartNumberMarker,
	})
	if err != nil || len(nextParts.Parts) != 1 || aws.ToInt32(nextParts.Parts[0].PartNumber) != 2 {
		t.Fatalf("second part page: %+v %v", nextParts, err)
	}
	uploads, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket})
	if err != nil || len(uploads.Uploads) != 1 || aws.ToString(uploads.Uploads[0].UploadId) != aws.ToString(id) {
		t.Fatalf("uploads: %+v %v", uploads, err)
	}
	parts := []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: p1.ETag}, {PartNumber: aws.Int32(2), ETag: p2.ETag}}
	if _, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: aws.String(`"wrong"`)}, parts[1],
		}},
	}); err == nil {
		t.Fatal("incorrect part ETag accepted")
	} else if code, _ := s3Code(err); code != "InvalidPart" {
		t.Fatal(err)
	}
	if _, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{parts[1], parts[0]}},
	}); err == nil {
		t.Fatal("out of order parts accepted")
	} else if code, _ := s3Code(err); code != "InvalidPartOrder" {
		t.Fatal(err)
	}
	completed, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil || !strings.HasSuffix(aws.ToString(completed.ETag), "-2\"") {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil || len(body) != len(first)+4 || !bytes.Equal(body[:len(first)], first) ||
		string(body[len(first):]) != "tail" || aws.ToString(got.ContentType) != "text/plain" ||
		got.Metadata["owner"] != "test" {
		t.Fatalf("object len=%d output=%+v err=%v", len(body), got, err)
	}
	uploads, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket})
	if err != nil || len(uploads.Uploads) != 0 {
		t.Fatalf("completed upload remains: %+v %v", uploads, err)
	}
	if _, err = c.ListParts(ctx, &s3.ListPartsInput{Bucket: bucket, Key: key, UploadId: id}); err == nil {
		t.Fatal("completed upload still has parts")
	}
	aborted, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: aws.String("discard"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: bucket, Key: aws.String("discard"), UploadId: aborted.UploadId,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: aws.String("discard"), UploadId: aborted.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("x"),
	}); err == nil {
		t.Fatal("aborted upload accepted a part")
	}
	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket}); err == nil {
		t.Fatal("nonempty bucket deleted")
	}
}

func TestS3MultipartRejectsSmallNonfinalPart(t *testing.T) {
	ctx := context.Background()
	c := s3Client(server(t), "test", "us-east-1")
	bucket, key := aws.String("small-part-bucket"), aws.String("file")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("short"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		PartNumber: aws.Int32(2), Body: strings.NewReader("last"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: first.ETag},
			{PartNumber: aws.Int32(2), ETag: second.ETag},
		}},
	})
	if code, _ := s3Code(err); code != "EntityTooSmall" {
		t.Fatalf("expected EntityTooSmall, got %v", err)
	}
	if _, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key}); err == nil {
		t.Fatal("failed completion exposed an object")
	}
	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket}); err == nil {
		t.Fatal("bucket with an active upload was deleted")
	}
}

func TestS3MultipartSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := s3Client(s, "test", "us-east-1")
	bucket, key := aws.String("restart-upload-bucket"), aws.String("file")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("persistent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	c = s3Client(s, "test", "us-east-1")
	listed, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: bucket, Key: key, UploadId: created.UploadId})
	if err != nil || len(listed.Parts) != 1 {
		t.Fatalf("parts after restart: %+v %v", listed, err)
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.ETag},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil || string(body) != "persistent" {
		t.Fatalf("object after restart: %q %v", body, err)
	}
}

func TestS3MultipartListingAndIsolation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	bucket := aws.String("upload-list-bucket")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a/one", "a/two", "b"} {
		if _, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: bucket, Key: aws.String(key),
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: bucket, MaxUploads: aws.Int32(1),
	})
	if err != nil || len(page.Uploads) != 1 || !aws.ToBool(page.IsTruncated) ||
		aws.ToString(page.Uploads[0].Key) != "a/one" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: bucket, MaxUploads: aws.Int32(1),
		KeyMarker: page.NextKeyMarker, UploadIdMarker: page.NextUploadIdMarker,
	})
	if err != nil || len(page.Uploads) != 1 || aws.ToString(page.Uploads[0].Key) != "a/two" {
		t.Fatalf("second page: %+v %v", page, err)
	}
	other := s3Client(s, "team-b", "us-east-1")
	if _, err = other.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket}); err == nil {
		t.Fatal("another account listed uploads")
	}
	wrongRegion := s3Client(s, "test", "eu-west-1")
	if _, err = wrongRegion.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket}); err == nil {
		t.Fatal("another region listed uploads")
	}
}

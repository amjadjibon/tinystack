package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// deterministicReader generates bytes from their offset without a large buffer.
type deterministicReader struct{ offset int64 }

func (r *deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.offset + int64(i)) % 251)
	}

	r.offset += int64(len(p))
	return len(p), nil
}

func TestS3DiskLargeObjects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(t.TempDir(), "input"))
	if err != nil {
		t.Fatal(err)
	}

	defer file.Close()
	const size = 101 << 20
	want := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(file, want), &deterministicReader{}, size); err != nil {
		t.Fatal(err)
	}

	s, stop := persistent(t, dir, &fakeRuntime{})
	c := s3Client(s, "test", "us-east-1")
	bucket := new("large-blobs")
	if _, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: new("put"), Body: io.NewSectionReader(file, 0, size),
	}); err != nil {
		t.Fatal(err)
	}

	uploader := transfermanager.New(c, func(o *transfermanager.Options) {
		o.PartSizeBytes = 5 << 20
		o.Concurrency = 2
	})
	if _, err = uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: bucket, Key: new("multipart"), Body: io.NewSectionReader(file, 0, size),
	}); err != nil {
		t.Fatal(err)
	}

	check := func(key string) {
		t.Helper()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		out, getErr := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: &key})
		if getErr != nil {
			t.Fatal(getErr)
		}

		got := sha256.New()
		n, readErr := io.Copy(got, out.Body)
		_ = out.Body.Close()
		runtime.ReadMemStats(&after)
		if readErr != nil || n != size || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			t.Fatal("large GET mismatch", n, readErr)
		}

		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
			t.Fatalf("streaming GET allocated %d bytes for %d-byte object", allocated, size)
		}
	}

	check("put")
	check("multipart")
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: new("put"), Range: new("bytes=100-199")})
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if err != nil || len(data) != 100 || data[0] != 100 || data[99] != 199 {
		t.Fatal("range mismatch", err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	c = s3Client(s, "test", "us-east-1")
	check("put")
	check("multipart")
	response, err := http.Post(s.URL+"/_tinystack/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil || len(entries) != 0 {
		t.Fatal("reset retained files", entries, err)
	}
}

func TestS3DiskReferenceLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	c := s3Client(s, "test", "us-east-1")
	bucket := new("blob-lifecycle")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	put := func(key string) *s3.PutObjectOutput {
		t.Helper()
		out, err := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: bucket, Key: &key, Body: strings.NewReader(strings.Repeat("x", 32)), ContentLength: new(int64(32)),
		})
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	count := func(want int) {
		t.Helper()
		files, err := os.ReadDir(filepath.Join(dir, "blobs"))
		if err != nil || len(files) != want {
			t.Fatalf("blob count %d, want %d: %v", len(files), want, err)
		}
	}

	put("key")
	put("key")
	count(1)
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: new("copy"), CopySource: new("blob-lifecycle/key"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: new("key")}); err != nil {
		t.Fatal(err)
	}

	count(1)
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: new("copy")}); err != nil {
		t.Fatal(err)
	}

	count(0)
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  bucket,
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}

	first, second := put("versioned"), put("versioned")
	count(2)
	marker, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: new("versioned")})
	if err != nil {
		t.Fatal(err)
	}

	count(2)
	for _, version := range []*string{marker.VersionId, second.VersionId, first.VersionId} {
		if _, err = c.DeleteObject(
			ctx,
			&s3.DeleteObjectInput{Bucket: bucket, Key: new("versioned"), VersionId: version},
		); err != nil {
			t.Fatal(err)
		}
	}

	count(0)
	upload, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: new("abort")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: new("abort"), UploadId: upload.UploadId, PartNumber: new(int32(1)),
		Body: strings.NewReader(strings.Repeat("x", 32)), ContentLength: new(int64(32)),
	}); err != nil {
		t.Fatal(err)
	}

	count(1)
	if _, err = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: bucket, Key: new("abort"), UploadId: upload.UploadId,
	}); err != nil {
		t.Fatal(err)
	}

	count(0)
	if _, err = os.Stat(filepath.Join(dir, "state.db")); errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestS3DiskMigrationAndConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	server, stop := persistent(t, dir, &fakeRuntime{})
	c := s3Client(server, "test", "us-east-1")
	bucket := new("legacy-blobs")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: new("key"), Body: strings.NewReader("legacy"),
	}); err != nil {
		t.Fatal(err)
	}

	stop()
	base, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	if err = base.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put("000000000000/us-east-1/s3/legacy-blobs/d/key", []byte("legacy"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err = base.Close(); err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(dir, "blobs", ".pending-crash"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	server, stop = persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	c = s3Client(server, "test", "us-east-1")
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: new("key")})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if err != nil || string(body) != "legacy" {
		t.Fatal("legacy migration", string(body), err)
	}

	files, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil || len(files) != 1 {
		t.Fatal("migration orphan sweep", files, err)
	}

	var group sync.WaitGroup
	failures := make(chan error, 12)
	for n := range 12 {
		group.Go(func() {
			_, putErr := c.PutObject(ctx, &s3.PutObjectInput{
				Bucket: bucket, Key: new("key"), Body: strings.NewReader(fmt.Sprintf("writer-%d", n)),
			})
			failures <- putErr
		})
	}

	group.Wait()
	close(failures)
	for failure := range failures {
		if failure != nil {
			t.Fatal(failure)
		}
	}

	out, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: new("key")})
	if err != nil {
		t.Fatal(err)
	}

	body, err = io.ReadAll(out.Body)
	_ = out.Body.Close()
	if err != nil || !strings.HasPrefix(string(body), "writer-") {
		t.Fatal("concurrent write corrupted body", string(body), err)
	}

	files, err = os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil || len(files) != 1 {
		t.Fatal("concurrent overwrite retained files", files, err)
	}
}

func TestS3ConfiguredObjectLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.MaxObjectBytes = 16
	server := serverConfig(t, cfg, &fakeRuntime{})
	c := s3Client(server, "test", "us-east-1")
	ctx := context.Background()
	bucket := new("bounded-blobs")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: new("large"), Body: strings.NewReader(strings.Repeat("x", 17)),
	})
	if code, _ := s3Code(err); code != "EntityTooLarge" {
		t.Fatal("configured limit", err)
	}
}

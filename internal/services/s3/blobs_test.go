package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

const testBodyKey = "000000000000/us-east-1/s3/bucket/db/key"

func TestBlobCommitOrdering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reject := false
	base := memory.New(nil, func(puts map[string][]byte, deletes []string) error {
		for _, data := range puts {
			b, ok := storage.BlobReference(data)
			if ok {
				got, err := os.ReadFile(filepath.Join(dir, "blobs", b.ID))
				if err != nil || string(got) != "contents" {
					t.Fatalf("reference committed before file: %q %v", got, err)
				}
			}
		}

		if reject {
			return errors.New("commit rejected")
		}

		return nil
	})
	store, err := testBlobService(base, dir)
	if err != nil {
		t.Fatal(err)
	}

	b, err := store.blobs.Write(bytes.NewBufferString("contents"))
	if err != nil {
		t.Fatal(err)
	}

	err = store.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(testBodyKey, b.Record())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	store.blobs.Release(b)
	reject = true
	err = store.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(testBodyKey)
		return nil
	})
	if err == nil {
		t.Fatal("expected failed commit")
	}

	if _, err = os.Stat(filepath.Join(dir, "blobs", b.ID)); err != nil {
		t.Fatal("failed delete removed live file", err)
	}

	reject = false
	// A pinned reader survives a committed delete until it opens the file.
	store.blobs.Pin(b)
	err = store.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(testBodyKey)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	f, err := store.blobs.Open(b)
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(got) != "contents" {
		t.Fatal(string(got), err)
	}

	store.blobs.Release(b)
	if _, err = os.Stat(filepath.Join(dir, "blobs", b.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed deletion retained file", err)
	}
}

func TestBlobMigrationAndOrphanGC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := &memory.Store{}
	if err := base.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put("000000000000/us-east-1/s3/bucket/d/key", []byte("legacy"))
		tx.Put("000000000000/us-east-1/s3/bucket/b/version/key", []byte("version"))
		tx.Put("000000000000/us-east-1/s3/bucket/p/upload/00001", []byte("part"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	store, err := testBlobService(base, dir)
	if err != nil {
		t.Fatal(err)
	}

	var blob storage.Blob
	if err = store.store.View(ctx, func(tx storage.ReadTx) error {
		data, _ := tx.Get(testBodyKey)
		var ok bool
		blob, ok = storage.BlobReference(data)
		if !ok {
			t.Fatal("legacy body was not migrated")
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	f, err := store.blobs.Open(blob)
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(got) != "legacy" {
		t.Fatal(string(got), err)
	}

	orphan, err := store.blobs.Write(bytes.NewBufferString("orphan"))
	if err != nil {
		t.Fatal(err)
	}

	store.blobs.Release(orphan)
	if err = os.WriteFile(filepath.Join(dir, "blobs", ".pending-crash"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err = testBlobService(base, dir)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("startup sweep: %v %v", entries, err)
	}

	store.Reset(ctx)
	if err = storage.Clear(ctx, base); err != nil {
		t.Fatal(err)
	}

	entries, err = os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("reset retained blobs: %v %v", entries, err)
	}
}

func TestBlobLegacyReferenceLookingBytes(t *testing.T) {
	ctx := context.Background()
	base := &memory.Store{}
	literal := storage.Blob{ID: "0123456789abcdef0123456789abcdef", Size: 123}.Record()
	if err := base.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put("000000000000/us-east-1/s3/bucket/d/key", literal)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store, err := testBlobService(base, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.blobs.Close() })
	var blob storage.Blob
	if err = store.store.View(ctx, func(tx storage.ReadTx) error {
		data, _ := tx.Get(testBodyKey)
		blob, _ = storage.BlobReference(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	file, err := store.blobs.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	got, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(got, literal) {
		t.Fatal("legacy contents treated as a reference", err)
	}
	if err = store.blobs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("memory-mode shutdown retained the directory", err)
	}
}

func testBlobService(store storage.Store, dir string) (*Service, error) {
	blobs, err := storage.OpenBlobs(dir)
	if err != nil {
		return nil, err
	}
	s := New(store, nil, nil, nil, nil, nil, blobs)
	if err = s.Resume(context.Background()); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

func TestBlobPinnedCopyAfterDelete(t *testing.T) {
	ctx := context.Background()
	s, err := testBlobService(&memory.Store{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	b, err := s.blobs.Write(bytes.NewBufferString("shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(testBodyKey, b.Record())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.blobs.Release(b)
	// A copy pins its source before leaving the read transaction.
	if err = s.store.View(ctx, func(tx storage.ReadTx) error {
		data, _ := tx.Get(testBodyKey)
		b, _ = storage.BlobReference(data)
		s.blobs.Pin(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(testBodyKey)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(testBodyKey+"-copy", b.Record())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.blobs.Release(b)
	file, err := s.blobs.Open(b)
	if err != nil {
		t.Fatal("copy lost its shared file", err)
	}
	_ = file.Close()
}

// countedStore measures recovery scans separately from request transactions.
type countedStore struct {
	storage.Store
	scans atomic.Int64
}

type countedReadTx struct {
	storage.ReadTx
	scans *atomic.Int64
}

func (t countedReadTx) Keys(prefix string) []string {
	if prefix == "" {
		t.scans.Add(1)
	}

	return t.ReadTx.Keys(prefix)
}

type countedWriteTx struct {
	countedReadTx
	storage.WriteTx
}

func (t countedWriteTx) Get(key string) ([]byte, bool) { return t.WriteTx.Get(key) }
func (t countedWriteTx) Keys(prefix string) []string   { return t.countedReadTx.Keys(prefix) }

func (s *countedStore) View(ctx context.Context, fn func(storage.ReadTx) error) error {
	return s.Store.View(ctx, func(tx storage.ReadTx) error {
		return fn(countedReadTx{ReadTx: tx, scans: &s.scans})
	})
}

func (s *countedStore) Update(ctx context.Context, fn func(storage.WriteTx) error) error {
	return s.Store.Update(ctx, func(tx storage.WriteTx) error {
		return fn(countedWriteTx{countedReadTx: countedReadTx{ReadTx: tx, scans: &s.scans}, WriteTx: tx})
	})
}

func TestBlobReferenceCountsLifecycle(t *testing.T) {
	base := &countedStore{Store: &memory.Store{}}
	sDir := t.TempDir()
	s, err := testBlobService(base, sDir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	bucket := new("counted-bucket")
	if _, err = s.CreateBucket(ctx, &api.CreateBucketRequest{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	// Unrelated services and accounts must never be enumerated on the hot path.
	if err = base.Update(ctx, func(tx storage.WriteTx) error {
		for i := range 1000 {
			tx.Put(fmt.Sprintf("111111111111/eu-west-1/dynamodb/table/%d", i), []byte("unrelated"))
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	base.scans.Store(0)
	counts := func(want ...int) {
		t.Helper()
		var got []int
		var ids []string
		if viewErr := base.View(ctx, func(tx storage.ReadTx) error {
			for _, key := range tx.Keys(blobCountPrefix) {
				data, _ := tx.Get(key)
				n, parseErr := strconv.Atoi(string(data))
				if parseErr != nil {
					return parseErr
				}

				got = append(got, n)
				ids = append(ids, strings.TrimPrefix(key, blobCountPrefix))
			}

			return nil
		}); viewErr != nil {
			t.Fatal(viewErr)
		}

		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("reference counts %v, want %v", got, want)
		}

		entries, readErr := os.ReadDir(filepath.Join(sDir, "blobs"))
		if readErr != nil || len(entries) != len(ids) {
			t.Fatalf("files outlive last reference: %v %v (live IDs %v)", entries, readErr, ids)
		}

		for _, id := range ids {
			if _, statErr := os.Stat(filepath.Join(sDir, "blobs", id)); statErr != nil {
				t.Fatal("referenced file missing", statErr)
			}
		}
	}
	put := func(key, body string) *api.PutObjectOutput {
		t.Helper()
		out, putErr := s.PutObject(ctx, &api.PutObjectRequest{Bucket: bucket, Key: &key, Body: []byte(body)})
		if putErr != nil {
			t.Fatal(putErr)
		}

		return out
	}
	del := func(key string, version *string) *api.DeleteObjectOutput {
		t.Helper()
		out, deleteErr := s.DeleteObject(ctx, &api.DeleteObjectRequest{Bucket: bucket, Key: &key, VersionId: version})
		if deleteErr != nil {
			t.Fatal(deleteErr)
		}

		return out
	}

	put("key", "first")
	counts(1)
	if _, err = s.CopyObject(ctx, &api.CopyObjectRequest{
		Bucket: bucket, Key: new("copy"), CopySource: new("counted-bucket/key"),
	}); err != nil {
		t.Fatal(err)
	}

	counts(2)
	put("key", "replacement")
	counts(1, 1)
	del("key", nil)
	counts(1)
	del("copy", nil)
	counts()
	if _, err = s.PutBucketVersioning(ctx, &api.PutBucketVersioningRequest{
		Bucket: bucket, VersioningConfiguration: &api.VersioningConfiguration{Status: new("Enabled")},
	}); err != nil {
		t.Fatal(err)
	}

	first, second := put("versioned", "first"), put("versioned", "second")
	counts(1, 1)
	marker := del("versioned", nil)
	counts(1, 1)
	del("versioned", marker.VersionId)
	counts(1, 1)
	del("versioned", second.VersionId)
	counts(1)
	del("versioned", first.VersionId)
	counts()
	upload, err := s.CreateMultipartUpload(ctx, &api.CreateMultipartUploadRequest{
		Bucket: bucket, Key: new("multipart"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.UploadPart(ctx, &api.UploadPartRequest{
		Bucket:     bucket,
		Key:        new("multipart"),
		UploadId:   upload.UploadId,
		PartNumber: new(int64(1)),
		Body:       []byte("part"),
	})
	if err != nil {
		t.Fatal(err)
	}

	counts(1)
	part, err := s.UploadPart(ctx, &api.UploadPartRequest{
		Bucket:     bucket,
		Key:        new("multipart"),
		UploadId:   upload.UploadId,
		PartNumber: new(int64(1)),
		Body:       []byte("new-part"),
	})
	if err != nil {
		t.Fatal(err)
	}

	counts(1)
	completed, err := s.CompleteMultipartUpload(ctx, &api.CompleteMultipartUploadRequest{
		Bucket: bucket, Key: new("multipart"), UploadId: upload.UploadId,
		MultipartUpload: &api.CompletedMultipartUpload{
			Parts: []api.CompletedPart{{PartNumber: new(int64(1)), ETag: part.ETag}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	counts(1)
	del("multipart", completed.VersionId)
	counts()
	upload, err = s.CreateMultipartUpload(ctx, &api.CreateMultipartUploadRequest{Bucket: bucket, Key: new("abort")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.UploadPart(ctx, &api.UploadPartRequest{
		Bucket: bucket, Key: new("abort"), UploadId: upload.UploadId, PartNumber: new(int64(1)), Body: []byte("part"),
	}); err != nil {
		t.Fatal(err)
	}

	counts(1)
	if _, err = s.AbortMultipartUpload(ctx, &api.AbortMultipartUploadRequest{
		Bucket: bucket, Key: new("abort"), UploadId: upload.UploadId,
	}); err != nil {
		t.Fatal(err)
	}

	counts()
	if scans := base.scans.Load(); scans != 0 {
		t.Fatalf("hot path scanned unrelated keys %d times", scans)
	}
}

func TestBlobConcurrentReadersAndRetirement(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("overwrite=%t", overwrite), func(t *testing.T) {
			ctx := request.WithMetadata(context.Background(), request.Metadata{
				Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
			})
			s, err := testBlobService(&memory.Store{}, "")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(s.Stop)
			if _, err = s.CreateBucket(ctx, &api.CreateBucketRequest{Bucket: new("bucket")}); err != nil {
				t.Fatal(err)
			}

			if _, err = s.PutObject(ctx, &api.PutObjectRequest{
				Bucket: new("bucket"), Key: new("key"), Body: []byte("old"),
			}); err != nil {
				t.Fatal(err)
			}

			pinned, finishRead, readerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				readerDone <- s.store.View(ctx, func(storage.ReadTx) error {
					close(pinned)
					<-finishRead
					return nil
				})
			}()
			<-pinned
			// A second read must run while the first transaction is still active.
			secondDone := make(chan error, 1)
			var snapshot *found
			go func() {
				var readErr error
				snapshot, readErr = s.read(ctx, target{bucket: new("bucket"), key: new("key")}, nil, nil, true)
				secondDone <- readErr
			}()
			select {
			case err = <-secondDone:
			case <-time.After(5 * time.Second):
				close(finishRead)
				<-readerDone
				t.Fatal("S3 serialized independent read transactions")
			}

			if err != nil {
				t.Fatal(err)
			}

			old, _ := storage.BlobReference(snapshot.body)
			var replacement storage.Blob
			if overwrite {
				replacement, err = s.blobs.Write(bytes.NewBufferString("new"))
				if err != nil {
					t.Fatal(err)
				}

				defer s.blobs.Release(replacement)
			}

			writerDone := make(chan error, 1)
			go func() {
				writerDone <- s.store.Update(ctx, func(tx storage.WriteTx) error {
					if overwrite {
						tx.Put(testBodyKey, replacement.Record())
					} else {
						tx.Delete(testBodyKey)
					}

					return nil
				})
			}()
			close(finishRead)
			if err = <-readerDone; err != nil {
				t.Fatal(err)
			}

			if err = <-writerDone; err != nil {
				t.Fatal(err)
			}

			// Delay opening until retirement has completed: the transaction's pin
			// must keep the old file available through this entire interval.
			file, err := s.blobs.Open(old)
			if err != nil {
				t.Fatal("referenced file disappeared before open", err)
			}

			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || string(data) != "old" {
				t.Fatal(string(data), err)
			}

			s.blobs.Release(old)
			if _, err = os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("last reader retained retired file", err)
			}
		})
	}
}

func TestBlobResumeRebuildsCounts(t *testing.T) {
	ctx := context.Background()
	base := &memory.Store{}
	s, err := testBlobService(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	b, err := s.blobs.Write(bytes.NewBufferString("shared"))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a round-2 store, plus stale bookkeeping from an interrupted run.
	if err = base.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(testBodyKey, b.Record())
		tx.Put(testBodyKey+"-copy", b.Record())
		tx.Put(blobCountPrefix+"0123456789abcdef0123456789abcdef", []byte("999"))
		tx.Put("111111111111/eu-west-1/s3/other/p/upload/00001", []byte("legacy-part"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	s.blobs.Release(b)
	if err = s.Resume(ctx); err != nil {
		t.Fatal(err)
	}

	if err = base.View(ctx, func(tx storage.ReadTx) error {
		data, _ := tx.Get(blobCountPrefix + b.ID)
		if string(data) != "2" || len(tx.Keys(blobCountPrefix)) != 2 {
			t.Fatalf("resume counts: %q, %v", data, tx.Keys(blobCountPrefix))
		}

		data, _ = tx.Get("111111111111/eu-west-1/s3/other/pb/upload/00001")
		migrated, ok := storage.BlobReference(data)
		count, _ := tx.Get(blobCountPrefix + migrated.ID)
		if !ok || string(count) != "1" {
			t.Fatal("legacy migration did not initialize its count")
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Repeated resume neither double-counts nor loses shared references.
	if err = s.Resume(ctx); err != nil {
		t.Fatal(err)
	}

	if err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(testBodyKey)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	file, err := s.blobs.Open(b)
	if err != nil {
		t.Fatal("rebuild lost shared reference", err)
	}

	_ = file.Close()
	if err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(testBodyKey + "-copy")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rebuilt count retained file after last delete", err)
	}
}

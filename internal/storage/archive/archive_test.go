package archive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func createTestStorage(t *testing.T) (dir string, cleanup func()) {
	t.Helper()
	dir = t.TempDir()
	store, err := sqlite.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}

	return dir, func() {
		_ = store.Close()
	}
}

func populateTestStore(
	t *testing.T,
	storageDir string,
	records map[string][]byte,
	blobs map[string][]byte,
) {
	t.Helper()

	store, err := sqlite.Open(context.Background(), storageDir)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = store.Close() }()

	err = store.Update(context.Background(), func(tx storage.WriteTx) error {
		for k, v := range records {
			tx.Put(k, v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("update sqlite: %v", err)
	}

	if len(blobs) > 0 {
		blobsDir := filepath.Join(storageDir, "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatalf("mkdir blobs: %v", err)
		}
		for id, data := range blobs {
			blobPath := filepath.Join(blobsDir, id)
			if err := os.WriteFile(blobPath, data, 0o600); err != nil {
				t.Fatalf("write blob %s: %v", id, err)
			}
		}
	}
}

func TestValidateExportPaths_Hazards(t *testing.T) {
	tempDir := t.TempDir()
	storageDir := filepath.Join(tempDir, "data")
	if err := os.MkdirAll(filepath.Join(storageDir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "state.db"), []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Missing storage dir
	nonexistentDir := filepath.Join(tempDir, "nonexistent")
	_, _, err := archive.ValidateExportPaths(nonexistentDir, filepath.Join(tempDir, "out.tar.gz"), false)
	if err == nil || !strings.Contains(err.Error(), "storage directory") ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing storage dir error, got %v", err)
	}

	// Verify no directory was created
	if _, statErr := os.Stat(nonexistentDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected nonexistent directory to not be created")
	}

	// 2. Missing state.db
	emptyDir := filepath.Join(tempDir, "empty")
	if mkdirErr := os.MkdirAll(emptyDir, 0o755); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	_, _, err = archive.ValidateExportPaths(emptyDir, filepath.Join(tempDir, "out.tar.gz"), false)
	if err == nil || !strings.Contains(err.Error(), "storage database file") ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing state.db error, got %v", err)
	}

	// 3. Overlap with state.db
	_, _, err = archive.ValidateExportPaths(storageDir, filepath.Join(storageDir, "state.db"), true)
	if err == nil || !strings.Contains(err.Error(), "overlaps with storage database") {
		t.Fatalf("expected state.db overlap error, got %v", err)
	}

	// 4. Overlap with state.db-wal
	_, _, err = archive.ValidateExportPaths(storageDir, filepath.Join(storageDir, "state.db-wal"), true)
	if err == nil || !strings.Contains(err.Error(), "overlaps with storage database sidecar") {
		t.Fatalf("expected state.db-wal overlap error, got %v", err)
	}

	// 5. Overlap inside blobs dir
	_, _, err = archive.ValidateExportPaths(storageDir, filepath.Join(storageDir, "blobs", "out.tar.gz"), true)
	if err == nil || !strings.Contains(err.Error(), "blobs directory") {
		t.Fatalf("expected blobs dir overlap error, got %v", err)
	}
}

func TestExport_EmptyState(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "empty.tar.gz")

	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	fi, err := os.Stat(outFile)
	if err != nil {
		t.Fatalf("stat output file: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatalf("expected non-empty archive file, got 0 bytes")
	}

	// Inspect archive
	manifest, records, err := archive.InspectArchive(context.Background(), outFile)
	if err != nil {
		t.Fatalf("InspectArchive failed: %v", err)
	}

	if manifest.FormatVersion != archive.FormatVersion {
		t.Fatalf("expected format version %d, got %d", archive.FormatVersion, manifest.FormatVersion)
	}
	if manifest.Records.Count != 0 {
		t.Fatalf("expected 0 records, got %d", manifest.Records.Count)
	}
	if manifest.Totals.BlobCount != 0 {
		t.Fatalf("expected 0 blobs, got %d", manifest.Totals.BlobCount)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records slice, got %d", len(records))
	}
}

func TestExport_Deterministic(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	blob1Data := []byte("hello blob 1")
	blob2Data := []byte("hello blob 2")
	blob1ID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blob2ID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	records := map[string][]byte{
		"zeta":         []byte("val zeta"),
		"alpha":        []byte("val alpha"),
		"blob:ref:two": (storage.Blob{ID: blob2ID, Size: int64(len(blob2Data))}).Record(),
		"blob:ref:one": (storage.Blob{ID: blob1ID, Size: int64(len(blob1Data))}).Record(),
	}
	blobs := map[string][]byte{
		blob2ID: blob2Data,
		blob1ID: blob1Data,
	}
	populateTestStore(t, storageDir, records, blobs)

	outDir := t.TempDir()
	out1 := filepath.Join(outDir, "out1.tar.gz")
	out2 := filepath.Join(outDir, "out2.tar.gz")

	if err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: out1,
	}); err != nil {
		t.Fatalf("Export 1 failed: %v", err)
	}

	if err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: out2,
	}); err != nil {
		t.Fatalf("Export 2 failed: %v", err)
	}

	b1, err := os.ReadFile(out1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(out2)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("expected deterministic byte-for-byte identical archives, lengths %d vs %d", len(b1), len(b2))
	}

	// Validate contents of out1
	manifest, recs, err := archive.InspectArchive(context.Background(), out1)
	if err != nil {
		t.Fatalf("InspectArchive: %v", err)
	}

	if len(recs) != 4 {
		t.Fatalf("expected 4 records, got %d", len(recs))
	}
	// Verify sorted order
	if recs[0].Key != "alpha" || recs[1].Key != "blob:ref:one" || recs[2].Key != "blob:ref:two" ||
		recs[3].Key != "zeta" {
		t.Fatalf("records not sorted: %v", recs)
	}

	if len(manifest.Blobs) != 2 {
		t.Fatalf("expected 2 blobs in manifest, got %d", len(manifest.Blobs))
	}
	if manifest.Blobs[0].ID != blob1ID || manifest.Blobs[1].ID != blob2ID {
		t.Fatalf("blobs not sorted: %v", manifest.Blobs)
	}
}

func TestExport_BlobDeduplicationAndZeroByteBodies(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	sharedBlobID := "11111111111111111111111111111111"
	sharedBlobData := []byte("shared content")

	zeroBlobID := "00000000000000000000000000000000"
	zeroBlobData := []byte("")

	records := map[string][]byte{
		"s3:bucket/obj1": (storage.Blob{ID: sharedBlobID, Size: int64(len(sharedBlobData))}).Record(),
		"s3:bucket/obj2": (storage.Blob{ID: sharedBlobID, Size: int64(len(sharedBlobData))}).Record(),
		"s3:bucket/zero": (storage.Blob{ID: zeroBlobID, Size: 0}).Record(),
	}
	blobs := map[string][]byte{
		sharedBlobID: sharedBlobData,
		zeroBlobID:   zeroBlobData,
	}
	populateTestStore(t, storageDir, records, blobs)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "dedup.tar.gz")

	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	manifest, recs, err := archive.InspectArchive(context.Background(), outFile)
	if err != nil {
		t.Fatalf("InspectArchive failed: %v", err)
	}

	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}

	// Ensure exactly 2 blobs deduplicated
	if len(manifest.Blobs) != 2 {
		t.Fatalf("expected 2 deduplicated blobs in manifest, got %d", len(manifest.Blobs))
	}

	if manifest.Blobs[0].ID != zeroBlobID || manifest.Blobs[0].Size != 0 {
		t.Fatalf("unexpected zero blob meta: %+v", manifest.Blobs[0])
	}
	if manifest.Blobs[1].ID != sharedBlobID || manifest.Blobs[1].Size != int64(len(sharedBlobData)) {
		t.Fatalf("unexpected shared blob meta: %+v", manifest.Blobs[1])
	}

	if manifest.Totals.TotalBlobBytes != int64(len(sharedBlobData)) {
		t.Fatalf("expected total blob bytes %d, got %d", len(sharedBlobData), manifest.Totals.TotalBlobBytes)
	}
}

func TestExport_ActiveStoreLockRefusal(t *testing.T) {
	dir := t.TempDir()
	// Open store and keep it open (holding exclusive connection lock).
	store, err := sqlite.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "locked.tar.gz")

	err = archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: dir,
		OutputFile: outFile,
	})
	if err == nil {
		t.Fatalf("expected Export to fail due to active store exclusive lock, got nil")
	}

	// Verify destination file was not created
	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected destination file to not exist after locked failure")
	}
}

func TestExport_MissingReferencedBlob(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	missingBlobID := "99999999999999999999999999999999"
	records := map[string][]byte{
		"s3:bucket/missing": (storage.Blob{ID: missingBlobID, Size: 123}).Record(),
	}
	populateTestStore(t, storageDir, records, nil) // No blobs directory / file populated

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "missing.tar.gz")

	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	if err == nil || !strings.Contains(err.Error(), "referenced blobs directory") {
		t.Fatalf("expected missing blob directory/file error, got %v", err)
	}

	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected destination file to not exist after missing blob error")
	}
}

func TestExport_MismatchedBlobSize(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	blobID := "88888888888888888888888888888888"
	records := map[string][]byte{
		"s3:bucket/mismatch": (storage.Blob{ID: blobID, Size: 100}).Record(),
	}
	blobs := map[string][]byte{
		blobID: []byte("only 14 bytes!"),
	}
	populateTestStore(t, storageDir, records, blobs)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "mismatch.tar.gz")

	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected size mismatch error, got %v", err)
	}

	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected destination file to not exist after size mismatch error")
	}
}

func TestExport_OverwriteRefusalAndPreservation(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "existing.tar.gz")

	originalContent := []byte("original protected content")
	if err := os.WriteFile(outFile, originalContent, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Export without overwrite -> should fail and preserve original content
	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
		Overwrite:  false,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got %v", err)
	}

	afterContent, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalContent, afterContent) {
		t.Fatalf("expected original content to be preserved, got %q", string(afterContent))
	}

	// 2. Export with overwrite -> should succeed
	err = archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
		Overwrite:  true,
	})
	if err != nil {
		t.Fatalf("expected Export with overwrite to succeed, got %v", err)
	}

	overwrittenContent, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(originalContent, overwrittenContent) {
		t.Fatalf("expected destination file to be overwritten")
	}
}

func TestExport_Cancellation(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "cancelled.tar.gz")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got %v", err)
	}

	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected destination file to not exist after cancellation")
	}

	assertNoTempFiles(t, outDir)
}

// testFaultTempFile is an unexported test double implementing archive.ExportTempFile
// to exercise real I/O failure boundaries.
type testFaultTempFile struct {
	file    *os.File
	writeFn func(p []byte) (int, error)
	syncFn  func() error
	closeFn func() error
	closed  bool
}

func (tf *testFaultTempFile) Write(p []byte) (int, error) {
	if tf.writeFn != nil {
		return tf.writeFn(p)
	}
	return tf.file.Write(p)
}

func (tf *testFaultTempFile) Sync() error {
	if tf.syncFn != nil {
		return tf.syncFn()
	}
	return tf.file.Sync()
}

func (tf *testFaultTempFile) Close() error {
	var closeErr error
	if tf.closeFn != nil {
		closeErr = tf.closeFn()
	}
	if !tf.closed {
		tf.closed = true
		if err := tf.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (tf *testFaultTempFile) Name() string {
	return tf.file.Name()
}

func TestExport_DuringCopyCancellation(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	blobID := "0123456789abcdef0123456789abcdef"
	// Blob size 192 KiB exceeds the 64 KiB StreamBufferSize, ensuring multiple streaming copy iterations.
	blobData := bytes.Repeat([]byte("X"), 192*1024)
	records := map[string][]byte{
		"s3:bucket/key": (storage.Blob{ID: blobID, Size: int64(len(blobData))}).Record(),
	}
	blobs := map[string][]byte{
		blobID: blobData,
	}
	populateTestStore(t, storageDir, records, blobs)

	outDir := t.TempDir()
	// Use uncompressed .tar output so tar.Writer writes directly to the temp file without gzip buffering.
	outFile := filepath.Join(outDir, "interrupted.tar")

	// Pre-create destination to verify destination preservation upon cancellation.
	originalDestContent := []byte("pre-existing destination archive")
	if err := os.WriteFile(outFile, originalDestContent, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		copiedBodyBytes atomic.Int64
		cancelCalled    atomic.Bool
	)

	cleanupFactory := archive.SetCreateTempFileForTesting(func(dir, pattern string) (archive.ExportTempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &testFaultTempFile{
			file: f,
			writeFn: func(p []byte) (int, error) {
				// Forward write to disk first so bytes actually land in the temp file.
				n, werr := f.Write(p)
				if werr != nil {
					return n, werr
				}
				// Tar headers (manifest, records, blob header) are small (< 4 KiB).
				// Blob streaming delivers 64 KiB chunks.
				// Once the first full 64 KiB chunk of blob body data has landed, cancel the context.
				if n >= 64*1024 && cancelCalled.CompareAndSwap(false, true) {
					copiedBodyBytes.Add(int64(n))
					cancel()
				}
				return n, nil
			},
		}, nil
	})
	defer cleanupFactory()

	err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
		Overwrite:  true,
	})
	if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled")) {
		t.Fatalf("expected context canceled error, got %v", err)
	}

	// Verify positive body bytes were written before cancellation occurred.
	if copied := copiedBodyBytes.Load(); copied < 64*1024 {
		t.Fatalf("expected at least 64 KiB body bytes to be copied before cancellation, got %d", copied)
	}

	// Verify destination was preserved.
	after, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatalf("read preserved destination: %v", readErr)
	}
	if !bytes.Equal(after, originalDestContent) {
		t.Fatalf("destination was modified or corrupted upon cancellation: expected %q, got %q",
			string(originalDestContent), string(after))
	}

	// Verify no temporary files remain.
	assertNoTempFiles(t, outDir)
}

func TestExport_StreamFailure(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	blobID := "0123456789abcdef0123456789abcdef"
	blobData := bytes.Repeat([]byte("Y"), 128*1024)
	records := map[string][]byte{
		"s3:bucket/key": (storage.Blob{ID: blobID, Size: int64(len(blobData))}).Record(),
	}
	blobs := map[string][]byte{
		blobID: blobData,
	}
	populateTestStore(t, storageDir, records, blobs)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "stream-fail.tar")

	originalDestContent := []byte("pre-existing destination for stream fail test")
	if err := os.WriteFile(outFile, originalDestContent, 0o600); err != nil {
		t.Fatal(err)
	}

	simulatedErr := errors.New("simulated I/O disk full on write")
	cleanupFactory := archive.SetCreateTempFileForTesting(func(dir, pattern string) (archive.ExportTempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &testFaultTempFile{
			file: f,
			writeFn: func(p []byte) (int, error) {
				// Allow writing metadata headers, then trigger real writer failure on blob chunk
				if len(p) >= 64*1024 {
					return 0, simulatedErr
				}
				return f.Write(p)
			},
		}, nil
	})
	defer cleanupFactory()

	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
		Overwrite:  true,
	})
	if err == nil || !strings.Contains(err.Error(), simulatedErr.Error()) {
		t.Fatalf("expected stream failure error containing %q, got %v", simulatedErr.Error(), err)
	}

	after, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatalf("read preserved destination: %v", readErr)
	}
	if !bytes.Equal(after, originalDestContent) {
		t.Fatalf("destination was modified upon stream failure")
	}

	assertNoTempFiles(t, outDir)
}

func TestExport_CloseFinalizeFailure(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	records := map[string][]byte{
		"k": []byte("v"),
	}
	populateTestStore(t, storageDir, records, nil)

	t.Run("sync_error", func(t *testing.T) {
		outDir := t.TempDir()
		outFile := filepath.Join(outDir, "sync-fail.tar.gz")

		originalDestContent := []byte("pre-existing destination for sync fail test")
		if err := os.WriteFile(outFile, originalDestContent, 0o600); err != nil {
			t.Fatal(err)
		}

		syncErr := errors.New("simulated finalize sync I/O failure")
		cleanupFactory := archive.SetCreateTempFileForTesting(
			func(dir, pattern string) (archive.ExportTempFile, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return &testFaultTempFile{
					file: f,
					syncFn: func() error {
						return syncErr
					},
				}, nil
			},
		)
		defer cleanupFactory()

		err := archive.Export(context.Background(), archive.ExportOptions{
			StorageDir: storageDir,
			OutputFile: outFile,
			Overwrite:  true,
		})
		if err == nil || !strings.Contains(err.Error(), syncErr.Error()) {
			t.Fatalf("expected sync error containing %q, got %v", syncErr.Error(), err)
		}

		after, readErr := os.ReadFile(outFile)
		if readErr != nil {
			t.Fatalf("read preserved destination: %v", readErr)
		}
		if !bytes.Equal(after, originalDestContent) {
			t.Fatalf("destination was modified upon sync failure")
		}

		assertNoTempFiles(t, outDir)
	})

	t.Run("close_error", func(t *testing.T) {
		outDir := t.TempDir()
		outFile := filepath.Join(outDir, "close-fail.tar.gz")

		originalDestContent := []byte("pre-existing destination for close fail test")
		if err := os.WriteFile(outFile, originalDestContent, 0o600); err != nil {
			t.Fatal(err)
		}

		closeErr := errors.New("simulated finalize close disk failure")
		cleanupFactory := archive.SetCreateTempFileForTesting(
			func(dir, pattern string) (archive.ExportTempFile, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return &testFaultTempFile{
					file: f,
					closeFn: func() error {
						return closeErr
					},
				}, nil
			},
		)
		defer cleanupFactory()

		err := archive.Export(context.Background(), archive.ExportOptions{
			StorageDir: storageDir,
			OutputFile: outFile,
			Overwrite:  true,
		})
		if err == nil || !strings.Contains(err.Error(), closeErr.Error()) {
			t.Fatalf("expected close error containing %q, got %v", closeErr.Error(), err)
		}

		after, readErr := os.ReadFile(outFile)
		if readErr != nil {
			t.Fatalf("read preserved destination: %v", readErr)
		}
		if !bytes.Equal(after, originalDestContent) {
			t.Fatalf("destination was modified upon close failure")
		}

		assertNoTempFiles(t, outDir)
	})
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir for temp files: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tinystack-export-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("found uncleaned temporary file: %s", entry.Name())
		}
	}
}

func TestInspectArchive_ValidationFailures(t *testing.T) {
	// 1. Missing file
	_, _, err := archive.InspectArchive(context.Background(), "/nonexistent/file.tar.gz")
	if err == nil {
		t.Fatalf("expected missing file error, got nil")
	}

	// 2. Corrupted tar header
	tempDir := t.TempDir()
	badTar := filepath.Join(tempDir, "bad.tar")
	if writeErr := os.WriteFile(badTar, []byte("not a tar file"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	_, _, err = archive.InspectArchive(context.Background(), badTar)
	if err == nil {
		t.Fatalf("expected corrupted tar error, got nil")
	}
}

func TestExport_PublicationRace(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	records := map[string][]byte{
		"k1": []byte("v1"),
	}
	populateTestStore(t, storageDir, records, nil)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "race.tar.gz")

	// Install hook to simulate another process creating the destination file right before publication
	cleanup := archive.SetBeforePublishHookForTesting(func(tmpPath string) error {
		return os.WriteFile(outFile, []byte("concurrently created destination file"), 0o600)
	})
	defer cleanup()

	// Export with Overwrite: false -> must refuse to overwrite and preserve existing file
	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
		Overwrite:  false,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected destination file already exists error during publication race, got %v", err)
	}

	// Verify pre-existing content is preserved
	content, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatalf("read destination: %v", readErr)
	}
	if string(content) != "concurrently created destination file" {
		t.Fatalf("destination was overwritten during publication race: %s", string(content))
	}

	// Verify temp file was cleaned up
	assertNoTempFiles(t, outDir)
}

func TestExport_LimitsExceeded(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	records := map[string][]byte{
		"k1": []byte("v1"),
	}
	populateTestStore(t, storageDir, records, nil)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "limit.tar.gz")

	// 1. Total records limit exceeded
	restore := archive.SetLimitsForTesting(10, 32*1024*1024)
	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	restore()
	if err == nil || !strings.Contains(err.Error(), "records payload size") {
		t.Fatalf("expected records payload size limit exceeded error, got %v", err)
	}

	// 2. Manifest limit exceeded
	restore = archive.SetLimitsForTesting(1024*1024, 10)
	err = archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	restore()
	if err == nil || !strings.Contains(err.Error(), "manifest size") {
		t.Fatalf("expected manifest size limit exceeded error, got %v", err)
	}
}

func TestExport_BinaryKeysComplex(t *testing.T) {
	storageDir, closeStore := createTestStorage(t)
	closeStore()

	// Keys containing raw binary bytes, non-UTF8 bytes, and NUL characters
	k1 := "global/\x00/first"
	k2 := "global/\x00/second"
	k3 := "global/\xff\xfe/highbytes"
	k4 := "global/\xfe\xff/reversed"

	records := map[string][]byte{
		k1: []byte("val1"),
		k2: []byte("val2"),
		k3: []byte("val3"),
		k4: []byte("val4"),
	}
	populateTestStore(t, storageDir, records, nil)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "binary_keys.tar.gz")

	err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: outFile,
	})
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	manifest, recs, inspectErr := archive.InspectArchive(context.Background(), outFile)
	if inspectErr != nil {
		t.Fatalf("InspectArchive failed: %v", inspectErr)
	}

	if manifest.Records.Count != 4 {
		t.Fatalf("expected 4 records, got %d", manifest.Records.Count)
	}

	foundKeys := make(map[string][]byte)
	for _, r := range recs {
		foundKeys[r.Key] = r.Value
	}

	for k, expectedVal := range records {
		actualVal, ok := foundKeys[k]
		if !ok {
			t.Fatalf("key %q not found in inspected records", k)
		}
		if !bytes.Equal(actualVal, expectedVal) {
			t.Fatalf("value mismatch for key %q: expected %q, got %q", k, string(expectedVal), string(actualVal))
		}
	}
}

func TestInspectArchive_ContractCompleteness(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Unsupported FormatVersion
	t.Run("unsupported_format_version", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "v2.tar.gz")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			m.FormatVersion = 99
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "unsupported format version") {
			t.Fatalf("expected unsupported format version error, got %v", err)
		}
	})

	// 2. Negative counts or sizes
	t.Run("negative_counts", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "neg_count.tar.gz")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			m.Records.Count = -1
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "negative counts") {
			t.Fatalf("expected negative counts error, got %v", err)
		}
	})

	// 3. Non-hex or invalid length SHA-256
	t.Run("invalid_sha256_format", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "bad_sha.tar.gz")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			m.Records.SHA256 = "NOT_A_VALID_HEX_STRING"
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "records SHA-256 is invalid") {
			t.Fatalf("expected invalid sha256 error, got %v", err)
		}
	})

	// 4. Duplicate keys in records.json
	t.Run("duplicate_record_keys", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "dup_keys.tar.gz")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			*recs = append(*recs,
				archive.Record{Key: "duplicate_key", Value: []byte("val1")},
				archive.Record{Key: "duplicate_key", Value: []byte("val2")},
			)
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "duplicate record key") {
			t.Fatalf("expected duplicate record key error, got %v", err)
		}
	})

	// 5. Unsorted records in records.json
	t.Run("unsorted_records", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "unsorted_recs.tar.gz")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			*recs = []archive.Record{
				{Key: "z_key", Value: []byte("v1")},
				{Key: "a_key", Value: []byte("v2")},
			}
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "unsorted record keys") {
			t.Fatalf("expected unsorted records error, got %v", err)
		}
	})

	// 6. Manifest total_blob_bytes mismatch
	t.Run("total_blob_bytes_mismatch", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "bytes_mismatch.tar.gz")
		blobID := "0123456789abcdef0123456789abcdef"
		blobData := []byte("some blob data")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			*recs = append(*recs, archive.Record{
				Key:   "k1",
				Value: (storage.Blob{ID: blobID, Size: int64(len(blobData))}).Record(),
			})
			blobs[blobID] = blobData
			m.Totals.TotalBlobBytes = 999999 // deliberate mismatch
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "total_blob_bytes mismatch") {
			t.Fatalf("expected total_blob_bytes mismatch error, got %v", err)
		}
	})

	// 7. Unreferenced orphan blob in manifest
	t.Run("unreferenced_orphan_blob", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "orphan_blob.tar.gz")
		blobID := "0123456789abcdef0123456789abcdef"
		blobData := []byte("orphan blob data")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			// Do not reference blobID in recs
			blobs[blobID] = blobData
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "not referenced by any record") {
			t.Fatalf("expected unreferenced orphan blob error, got %v", err)
		}
	})

	// 8. Size mismatch between record reference and manifest
	t.Run("record_manifest_size_mismatch", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "size_ref_mismatch.tar.gz")
		blobID := "0123456789abcdef0123456789abcdef"
		blobData := []byte("real blob data")
		createCustomArchive(t, archivePath, func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte) {
			*recs = append(*recs, archive.Record{
				Key:   "k1",
				Value: (storage.Blob{ID: blobID, Size: int64(len(blobData)) + 10}).Record(), // size mismatch
			})
			blobs[blobID] = blobData
		})
		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil || !strings.Contains(err.Error(), "manifest declares") {
			t.Fatalf("expected reference size mismatch error, got %v", err)
		}
	})
}

// Helper to construct custom archives for testing validation edge cases.
func createCustomArchive(
	t *testing.T,
	archivePath string,
	mutator func(m *archive.Manifest, recs *[]archive.Record, blobs map[string][]byte),
) {
	t.Helper()

	manifest := archive.Manifest{
		FormatVersion:    archive.FormatVersion,
		TinyStackVersion: archive.TinyStackVersion,
	}
	records := []archive.Record{}
	blobs := make(map[string][]byte)

	mutator(&manifest, &records, blobs)

	// Build records.json
	recsBytes, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal records: %v", err)
	}

	if manifest.Records.TotalBytes == 0 {
		manifest.Records.TotalBytes = int64(len(recsBytes))
	}
	if manifest.Records.Count == 0 && len(records) > 0 {
		manifest.Records.Count = len(records)
	}
	if manifest.Records.SHA256 == "" {
		h := sha256.Sum256(recsBytes)
		manifest.Records.SHA256 = hex.EncodeToString(h[:])
	}

	// Build blobs metadata if not set
	if len(manifest.Blobs) == 0 && len(blobs) > 0 {
		blobIDs := make([]string, 0, len(blobs))
		for id := range blobs {
			blobIDs = append(blobIDs, id)
		}
		for _, id := range blobIDs {
			bData := blobs[id]
			h := sha256.Sum256(bData)
			manifest.Blobs = append(manifest.Blobs, archive.BlobMeta{
				ID:     id,
				Size:   int64(len(bData)),
				SHA256: hex.EncodeToString(h[:]),
			})
			manifest.Totals.TotalBlobBytes += int64(len(bData))
		}
		manifest.Totals.BlobCount = len(manifest.Blobs)
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create custom archive: %v", err)
	}
	defer func() { _ = f.Close() }()

	isGzip := strings.HasSuffix(archivePath, ".tar.gz")
	var (
		tw *tar.Writer
		gw *gzip.Writer
	)
	if isGzip {
		gw = gzip.NewWriter(f)
		gw.ModTime = time.Unix(0, 0)
		gw.OS = 255
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(f)
	}

	// 1. manifest.json
	_ = tw.WriteHeader(&tar.Header{
		Name:     archive.ManifestEntryName,
		Mode:     0o600,
		Size:     int64(len(manifestBytes)),
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	})
	_, _ = tw.Write(manifestBytes)

	// 2. records.json
	_ = tw.WriteHeader(&tar.Header{
		Name:     archive.RecordsEntryName,
		Mode:     0o600,
		Size:     int64(len(recsBytes)),
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	})
	_, _ = tw.Write(recsBytes)

	// 3. Blobs
	for _, bm := range manifest.Blobs {
		data := blobs[bm.ID]
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.BlobsPrefix + bm.ID,
			Mode:     0o600,
			Size:     int64(len(data)),
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		})
		_, _ = tw.Write(data)
	}

	_ = tw.Close()
	if gw != nil {
		_ = gw.Close()
	}
}

package archive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func assertNoLeakedStagingDirs(t *testing.T, parentDir string) {
	t.Helper()
	parentEntries, err := os.ReadDir(parentDir)
	if err != nil {
		t.Fatalf("read parent directory %s: %v", parentDir, err)
	}
	for _, e := range parentEntries {
		if strings.HasPrefix(e.Name(), ".tinystack-import-") {
			t.Fatalf("staging directory was leaked: %s", e.Name())
		}
	}
}

func TestImport_FullLifecycleAndEquality(t *testing.T) {
	ctx := context.Background()
	srcDir, closeSrc := createTestStorage(t)
	closeSrc()

	blob1ID := "0123456789abcdef0123456789abcdef"
	blob1Data := []byte("standard s3 object blob payload")

	blob2ID := "fedcba9876543210fedcba9876543210"
	blob2Data := []byte("") // 0-byte blob

	blob3ID := "11112222333344445555666677778888"
	blob3Data := []byte{0x00, 0xff, 0xfe, 0x42, 0xaa, 0x55, 0x00, 0x12} // binary blob with NUL and non-ASCII

	blobs := map[string][]byte{
		blob1ID: blob1Data,
		blob2ID: blob2Data,
		blob3ID: blob3Data,
	}

	records := map[string][]byte{
		// Global IAM role
		"global/iam/role/test-role": []byte(
			`{"RoleName":"test-role","Arn":"arn:aws:iam::000000000000:role/test-role"}`,
		),
		// Multi-scope DynamoDB tables
		"123456789012/us-east-1/dynamodb/tables/items/data/user-1": []byte(`{"id":"user-1","name":"Alice"}`),
		"123456789012/us-west-2/dynamodb/tables/items/data/user-2": []byte(`{"id":"user-2","name":"Bob"}`),
		// SQS queue message
		"123456789012/us-east-1/sqs/queue-1/msg-1": []byte("persisted-message"),
		// S3 object with standard blob reference
		"123456789012/us-east-1/s3/bucket-1/db/hello.txt": (storage.Blob{ID: blob1ID, Size: int64(len(blob1Data))}).Record(),
		// S3 0-byte object
		"123456789012/us-east-1/s3/bucket-1/db/empty.bin": (storage.Blob{ID: blob2ID, Size: int64(len(blob2Data))}).Record(),
		// S3 binary object
		"123456789012/us-east-1/s3/bucket-1/db/binary.bin": (storage.Blob{ID: blob3ID, Size: int64(len(blob3Data))}).Record(),
		// Non-blob record
		"config/system/version": []byte("1.0.0"),
	}

	populateTestStore(t, srcDir, records, blobs)

	// Step 1: Export to a state fixture archive.
	archivePath := filepath.Join(t.TempDir(), "exported-fixture.tar.gz")
	err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: archivePath,
	})
	if err != nil {
		t.Fatalf("export fixture: %v", err)
	}

	// Step 2: Inspect archive to verify manifest matches expected structure.
	manifest, archiveRecords, err := archive.InspectArchive(ctx, archivePath)
	if err != nil {
		t.Fatalf("inspect fixture: %v", err)
	}

	if manifest.Records.Count != len(records) {
		t.Fatalf("records count mismatch in manifest: got %d, want %d", manifest.Records.Count, len(records))
	}
	if manifest.Totals.BlobCount != len(blobs) {
		t.Fatalf("blobs count mismatch in manifest: got %d, want %d", manifest.Totals.BlobCount, len(blobs))
	}
	if len(archiveRecords) != len(records) {
		t.Fatalf("unmarshaled records count: got %d, want %d", len(archiveRecords), len(records))
	}

	// Step 3: Import into a fresh target storage directory.
	destParentDir := t.TempDir()
	destDir := filepath.Join(destParentDir, "imported-store")

	err = archive.Import(ctx, archive.ImportOptions{
		InputFile:  archivePath,
		StorageDir: destDir,
	})
	if err != nil {
		t.Fatalf("import fixture: %v", err)
	}

	// Verify target directory permissions are private (0700).
	destStat, err := os.Stat(destDir)
	if err != nil {
		t.Fatalf("stat imported directory: %v", err)
	}
	if destStat.Mode().Perm() != 0o700 {
		t.Fatalf("imported storage dir permissions: got %#o, want 0700", destStat.Mode().Perm())
	}

	// Step 4: Verify all records in SQLite database match.
	destStore, err := sqlite.Open(ctx, destDir)
	if err != nil {
		t.Fatalf("open imported sqlite store: %v", err)
	}
	defer func() { _ = destStore.Close() }()

	err = destStore.View(ctx, func(tx storage.ReadTx) error {
		for k, wantVal := range records {
			gotVal, ok := tx.Get(k)
			if !ok {
				t.Fatalf("missing record key in imported sqlite: %q", k)
			}
			if !bytes.Equal(gotVal, wantVal) {
				t.Fatalf("mismatched record value for key %q: got %q, want %q", k, gotVal, wantVal)
			}
		}
		if len(tx.Keys("")) != len(records) {
			t.Fatalf("unexpected record count in imported sqlite: got %d, want %d", len(tx.Keys("")), len(records))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view imported sqlite: %v", err)
	}

	// Step 5: Verify all blobs on disk match exactly with 0600 permissions.
	blobsDir := filepath.Join(destDir, "blobs")
	blobsStat, err := os.Stat(blobsDir)
	if err != nil {
		t.Fatalf("stat imported blobs directory: %v", err)
	}
	if blobsStat.Mode().Perm() != 0o700 {
		t.Fatalf("blobs directory permissions: got %#o, want 0700", blobsStat.Mode().Perm())
	}

	for id, wantPayload := range blobs {
		blobPath := filepath.Join(blobsDir, id)
		blobStat, err := os.Stat(blobPath)
		if err != nil {
			t.Fatalf("stat imported blob %s: %v", id, err)
		}
		if blobStat.Mode().Perm() != 0o600 {
			t.Fatalf("blob %s permissions: got %#o, want 0600", id, blobStat.Mode().Perm())
		}
		gotPayload, err := os.ReadFile(blobPath)
		if err != nil {
			t.Fatalf("read imported blob %s: %v", id, err)
		}
		if !bytes.Equal(gotPayload, wantPayload) {
			t.Fatalf("blob %s payload mismatch: got %q, want %q", id, gotPayload, wantPayload)
		}
	}

	// Step 6: Verify no staging directory was leaked in destParentDir.
	assertNoLeakedStagingDirs(t, destParentDir)
}

func TestImport_RefusalWhenDestinationExists(t *testing.T) {
	ctx := context.Background()
	srcDir, closeSrc := createTestStorage(t)
	closeSrc()
	populateTestStore(t, srcDir, map[string][]byte{"key": []byte("val")}, nil)

	archivePath := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: archivePath,
	}); err != nil {
		t.Fatalf("export fixture: %v", err)
	}

	t.Run("destination is an existing empty directory", func(t *testing.T) {
		existingDir := t.TempDir()
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: existingDir,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected error indicating destination already exists, got %v", err)
		}
	})

	t.Run("destination is an existing populated directory", func(t *testing.T) {
		existingDir := t.TempDir()
		sentinel := filepath.Join(existingDir, "sentinel.txt")
		if err := os.WriteFile(sentinel, []byte("precious-data"), 0o600); err != nil {
			t.Fatalf("write sentinel: %v", err)
		}

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: existingDir,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected error indicating destination already exists, got %v", err)
		}

		// Verify existing data was untouched.
		data, err := os.ReadFile(sentinel)
		if err != nil || string(data) != "precious-data" {
			t.Fatalf("existing directory data was modified or removed")
		}
	})

	t.Run("destination is an existing regular file", func(t *testing.T) {
		parentDir := t.TempDir()
		filePath := filepath.Join(parentDir, "file-not-dir")
		if err := os.WriteFile(filePath, []byte("i am a file"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: filePath,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected error indicating destination already exists, got %v", err)
		}
	})

	t.Run("parent directory does not exist", func(t *testing.T) {
		nonexistentParent := filepath.Join(t.TempDir(), "no-such-parent", "dest")
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: nonexistentParent,
		})
		if err == nil || !strings.Contains(err.Error(), "parent directory") {
			t.Fatalf("expected error indicating parent directory does not exist, got %v", err)
		}
	})
}

func TestImport_RefusalWhenInputInvalid(t *testing.T) {
	ctx := context.Background()
	destDir := filepath.Join(t.TempDir(), "dest")

	t.Run("input archive does not exist", func(t *testing.T) {
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  "/path/does/not/exist/fixture.tar.gz",
			StorageDir: destDir,
		})
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("expected input archive error, got %v", err)
		}
	})

	t.Run("input archive is a directory", func(t *testing.T) {
		inputDir := t.TempDir()
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  inputDir,
			StorageDir: destDir,
		})
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("expected directory error, got %v", err)
		}
	})

	t.Run("empty input path", func(t *testing.T) {
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  "",
			StorageDir: destDir,
		})
		if err == nil || !strings.Contains(err.Error(), "input file is required") {
			t.Fatalf("expected required input error, got %v", err)
		}
	})

	t.Run("empty storage directory path", func(t *testing.T) {
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  "some-file.tar.gz",
			StorageDir: "",
		})
		if err == nil || !strings.Contains(err.Error(), "storage directory is required") {
			t.Fatalf("expected required storage-dir error, got %v", err)
		}
	})
}

func TestImport_AtomicNoReplacePublicationRace(t *testing.T) {
	ctx := context.Background()
	srcDir, closeSrc := createTestStorage(t)
	closeSrc()
	populateTestStore(t, srcDir, map[string][]byte{"key": []byte("value")}, nil)

	archivePath := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: archivePath,
	}); err != nil {
		t.Fatalf("export fixture: %v", err)
	}

	t.Run("concurrent process creates empty destination directory before publication", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "raced-empty-destination")

		cleanupHook := archive.SetBeforePublishDirHookForTesting(func(stagingDir, targetDest string) error {
			if targetDest == destDir {
				// Concurrent process creates empty directory at destDir.
				if mkErr := os.Mkdir(destDir, 0o755); mkErr != nil {
					return mkErr
				}
			}
			return nil
		})
		defer cleanupHook()

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected publication race refusal with 'already exists', got %v", err)
		}

		// Verify the concurrently created destination directory was NOT replaced.
		entries, readErr := os.ReadDir(destDir)
		if readErr != nil {
			t.Fatalf("read destination dir: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("expected raced destination to remain unchanged empty dir, found %d entries", len(entries))
		}

		// Verify no staging directory leaked in parentDir.
		assertNoLeakedStagingDirs(t, parentDir)
	})

	t.Run("concurrent process creates populated destination directory before publication", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "raced-populated-destination")

		cleanupHook := archive.SetBeforePublishDirHookForTesting(func(stagingDir, targetDest string) error {
			if targetDest == destDir {
				// Concurrent process creates directory and places a sentinel file.
				if mkErr := os.Mkdir(destDir, 0o755); mkErr != nil {
					return mkErr
				}
				sentinelPath := filepath.Join(destDir, "sentinel.txt")
				if writeErr := os.WriteFile(sentinelPath, []byte("preserve-race-sentinel"), 0o600); writeErr != nil {
					return writeErr
				}
			}
			return nil
		})
		defer cleanupHook()

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected publication race refusal with 'already exists', got %v", err)
		}

		// Verify the concurrently created destination directory and sentinel file were preserved.
		sentinelContent, readErr := os.ReadFile(filepath.Join(destDir, "sentinel.txt"))
		if readErr != nil {
			t.Fatalf("read sentinel file: %v", readErr)
		}
		if string(sentinelContent) != "preserve-race-sentinel" {
			t.Fatalf("sentinel file was modified: got %q, want preserve-race-sentinel", string(sentinelContent))
		}

		// Verify no staging directory leaked in parentDir.
		assertNoLeakedStagingDirs(t, parentDir)
	})
}

func TestImport_CancellationAndFailureRecovery(t *testing.T) {
	srcDir, closeSrc := createTestStorage(t)
	closeSrc()

	blobID := "abcdef0123456789abcdef0123456789"
	// Create a large 192 KiB blob so multiple 64 KiB buffer chunks are streamed.
	largeBlobData := bytes.Repeat([]byte("X"), 192*1024)
	blobs := map[string][]byte{blobID: largeBlobData}
	records := map[string][]byte{
		"body": (storage.Blob{ID: blobID, Size: int64(len(largeBlobData))}).Record(),
	}
	populateTestStore(t, srcDir, records, blobs)

	archivePath := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := archive.Export(context.Background(), archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: archivePath,
	}); err != nil {
		t.Fatalf("export fixture: %v", err)
	}

	t.Run("cancellation after positive partial blob write", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "canceled-dest")

		var written atomic.Int64
		resetBlobWriter := archive.SetCreateBlobFileForTesting(
			func(path string, perm os.FileMode) (io.WriteCloser, error) {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
				if err != nil {
					return nil, err
				}
				return &cancelingTestWriter{file: f, written: &written, cancel: cancel}, nil
			},
		)
		defer resetBlobWriter()

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}

		if written.Load() < 64*1024 {
			t.Fatalf("expected positive partial bytes written before cancellation, got %d", written.Load())
		}

		// Verify destination was never created.
		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("expected destination to not exist after cancellation")
		}

		// Verify staging directory was cleaned up.
		assertNoLeakedStagingDirs(t, parentDir)
	})

	t.Run("blob write failure seam", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "write-fail-dest")

		resetBlobWriter := archive.SetCreateBlobFileForTesting(
			func(path string, perm os.FileMode) (io.WriteCloser, error) {
				return &faultyTestWriter{failWrite: true}, nil
			},
		)
		defer resetBlobWriter()

		err := archive.Import(context.Background(), archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "simulated write failure") {
			t.Fatalf("expected simulated write failure, got %v", err)
		}

		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("destination should not exist on write failure")
		}

		assertNoLeakedStagingDirs(t, parentDir)
	})

	t.Run("blob sync failure seam", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "sync-fail-dest")

		resetBlobWriter := archive.SetCreateBlobFileForTesting(
			func(path string, perm os.FileMode) (io.WriteCloser, error) {
				return &faultyTestWriter{failSync: true}, nil
			},
		)
		defer resetBlobWriter()

		err := archive.Import(context.Background(), archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "simulated sync failure") {
			t.Fatalf("expected simulated sync failure, got %v", err)
		}

		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("destination should not exist on sync failure")
		}

		assertNoLeakedStagingDirs(t, parentDir)
	})

	t.Run("blob directory sync failure seam", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "dir-sync-fail-dest")

		resetSyncDir := archive.SetSyncDirForTesting(func(dir string) error {
			return errors.New("simulated directory sync failure")
		})
		defer resetSyncDir()

		err := archive.Import(context.Background(), archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "simulated directory sync failure") {
			t.Fatalf("expected simulated directory sync failure, got %v", err)
		}

		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("destination should not exist on directory sync failure")
		}

		assertNoLeakedStagingDirs(t, parentDir)
	})

	t.Run("staging directory creation failure seam", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "stage-fail-dest")

		resetStaging := archive.SetCreateStagingDirForTesting(func(dir, pattern string) (string, error) {
			return "", errors.New("simulated staging directory creation failure")
		})
		defer resetStaging()

		err := archive.Import(context.Background(), archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "simulated staging directory creation failure") {
			t.Fatalf("expected staging failure, got %v", err)
		}

		assertNoLeakedStagingDirs(t, parentDir)
	})

	t.Run("sqlite open failure seam", func(t *testing.T) {
		parentDir := t.TempDir()
		destDir := filepath.Join(parentDir, "sqlite-fail-dest")

		resetSQLite := archive.SetOpenSQLiteStoreForTesting(
			func(ctx context.Context, dir string) (*sqlite.Store, error) {
				return nil, errors.New("simulated sqlite open failure")
			},
		)
		defer resetSQLite()

		err := archive.Import(context.Background(), archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: destDir,
		})

		if err == nil || !strings.Contains(err.Error(), "simulated sqlite open failure") {
			t.Fatalf("expected sqlite open failure, got %v", err)
		}

		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("destination should not exist on sqlite open failure")
		}

		assertNoLeakedStagingDirs(t, parentDir)
	})
}

type cancelingTestWriter struct {
	file    *os.File
	written *atomic.Int64
	cancel  context.CancelFunc
}

func (w *cancelingTestWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if n > 0 {
		total := w.written.Add(int64(n))
		if total >= 64*1024 {
			w.cancel()
		}
	}
	return n, err
}

func (w *cancelingTestWriter) Close() error {
	return w.file.Close()
}

type faultyTestWriter struct {
	failWrite bool
	failSync  bool
}

func (w *faultyTestWriter) Write(p []byte) (int, error) {
	if w.failWrite {
		return 0, errors.New("simulated write failure")
	}
	return len(p), nil
}

func (w *faultyTestWriter) Close() error {
	if w.failSync {
		return errors.New("simulated sync failure")
	}
	return nil
}

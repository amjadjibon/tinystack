package archive_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file for sha256 %s: %v", path, err)
	}
	return sha256.Sum256(data)
}

func TestImport_Preservation_FullLifecycle(t *testing.T) {
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
		// S3 0-byte object with empty blob reference
		"123456789012/us-east-1/s3/bucket-1/db/empty.bin": (storage.Blob{ID: blob2ID, Size: int64(len(blob2Data))}).Record(),
		// S3 binary object with NUL and non-ASCII blob reference
		"123456789012/us-east-1/s3/bucket-1/db/binary.bin": (storage.Blob{ID: blob3ID, Size: int64(len(blob3Data))}).Record(),
		// Shared blob reference (deduplication)
		"123456789012/us-east-1/s3/bucket-1/bb/v1/hello.txt": (storage.Blob{
			ID:   blob1ID,
			Size: int64(len(blob1Data)),
		}).Record(),
		// Legacy inline S3 body
		"123456789012/us-east-1/s3/bucket-1/d/legacy.txt": []byte("raw legacy unescaped inline body"),
		// Non-UTF8 and binary keys
		"binary-key/" + string([]byte{0x00, 0xff, 0xfe}): []byte("binary key payload"),
		"binary-nul-key/\x00\x01\xff\xfe":                []byte("nul and arbitrary binary bytes in key"),
	}

	populateTestStore(t, srcDir, records, blobs)

	// Step 1: Export compressed archive (.tar.gz).
	archivePath := filepath.Join(t.TempDir(), "fixture.tar.gz")
	err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: archivePath,
	})
	if err != nil {
		t.Fatalf("export fixture: %v", err)
	}

	preImportSHA := fileSHA256(t, archivePath)

	// Step 2: Import compressed archive into a fresh destination directory.
	parentDir := t.TempDir()
	restoredDir := filepath.Join(parentDir, "restored-state")

	err = archive.Import(ctx, archive.ImportOptions{
		InputFile:  archivePath,
		StorageDir: restoredDir,
	})
	if err != nil {
		t.Fatalf("import fixture: %v", err)
	}

	// Verify input archive file is completely unchanged.
	postImportSHA := fileSHA256(t, archivePath)
	if preImportSHA != postImportSHA {
		t.Fatalf("input archive was modified during import: pre %x != post %x", preImportSHA, postImportSHA)
	}

	// Step 3: Verify destination directory permissions (0700).
	dirInfo, err := os.Stat(restoredDir)
	if err != nil {
		t.Fatalf("stat restored directory: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("restored directory permissions: got %o, want 0700", perm)
	}

	// Step 4: Verify state.db permissions (0600).
	dbPath := filepath.Join(restoredDir, "state.db")
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat restored state.db: %v", err)
	}
	if perm := dbInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restored state.db permissions: got %o, want 0600", perm)
	}

	// Step 5: Verify blobs directory (0700) and blob file permissions (0600) and content.
	blobsDir := filepath.Join(restoredDir, "blobs")
	blobsInfo, err := os.Stat(blobsDir)
	if err != nil {
		t.Fatalf("stat restored blobs dir: %v", err)
	}
	if perm := blobsInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("restored blobs dir permissions: got %o, want 0700", perm)
	}

	for id, expectedContent := range blobs {
		bPath := filepath.Join(blobsDir, id)
		bInfo, bErr := os.Stat(bPath)
		if bErr != nil {
			t.Fatalf("stat restored blob %s: %v", id, bErr)
		}
		if perm := bInfo.Mode().Perm(); perm != 0o600 {
			t.Fatalf("restored blob %s permissions: got %o, want 0600", id, perm)
		}
		content, rErr := os.ReadFile(bPath)
		if rErr != nil {
			t.Fatalf("read restored blob %s: %v", id, rErr)
		}
		if !bytes.Equal(content, expectedContent) {
			t.Fatalf("restored blob %s content mismatch: got %q, want %q", id, content, expectedContent)
		}
	}

	// Step 6: Open restored SQLite store and verify 100% record equality.
	restoredStore, err := sqlite.Open(ctx, restoredDir)
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer func() { _ = restoredStore.Close() }()

	err = restoredStore.View(ctx, func(tx storage.ReadTx) error {
		restoredKeys := tx.Keys("")
		if len(restoredKeys) != len(records) {
			t.Fatalf("restored record count mismatch: got %d, want %d", len(restoredKeys), len(records))
		}

		for key, expectedVal := range records {
			val, ok := tx.Get(key)
			if !ok {
				t.Fatalf("missing expected key %q in restored store", key)
			}
			if !bytes.Equal(val, expectedVal) {
				t.Fatalf("restored value mismatch for key %q: got %q, want %q", key, val, expectedVal)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view restored store: %v", err)
	}

	// Step 7: Test uncompressed tar fixture export and import.
	tarPath := filepath.Join(t.TempDir(), "fixture.tar")
	if expErr := archive.Export(ctx, archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: tarPath,
	}); expErr != nil {
		t.Fatalf("export uncompressed tar: %v", expErr)
	}

	preTarSHA := fileSHA256(t, tarPath)
	restoredTarDir := filepath.Join(parentDir, "restored-tar-state")
	if impErr := archive.Import(ctx, archive.ImportOptions{
		InputFile:  tarPath,
		StorageDir: restoredTarDir,
	}); impErr != nil {
		t.Fatalf("import uncompressed tar: %v", impErr)
	}

	postTarSHA := fileSHA256(t, tarPath)
	if preTarSHA != postTarSHA {
		t.Fatalf("input tar archive was modified during import: pre %x != post %x", preTarSHA, postTarSHA)
	}

	// Verify tar restored directory permissions.
	tarDirInfo, err := os.Stat(restoredTarDir)
	if err != nil {
		t.Fatalf("stat restored tar dir: %v", err)
	}
	if perm := tarDirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("restored tar dir permissions: got %o, want 0700", perm)
	}

	tarDBInfo, err := os.Stat(filepath.Join(restoredTarDir, "state.db"))
	if err != nil {
		t.Fatalf("stat restored tar state.db: %v", err)
	}
	if perm := tarDBInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restored tar state.db permissions: got %o, want 0600", perm)
	}

	restoredTarStore, err := sqlite.Open(ctx, restoredTarDir)
	if err != nil {
		t.Fatalf("open restored tar store: %v", err)
	}
	defer func() { _ = restoredTarStore.Close() }()

	err = restoredTarStore.View(ctx, func(tx storage.ReadTx) error {
		restoredKeys := tx.Keys("")
		if len(restoredKeys) != len(records) {
			t.Fatalf("restored tar record count mismatch: got %d, want %d", len(restoredKeys), len(records))
		}
		for key, expectedVal := range records {
			val, ok := tx.Get(key)
			if !ok {
				t.Fatalf("missing expected key %q in restored tar store", key)
			}
			if !bytes.Equal(val, expectedVal) {
				t.Fatalf("restored tar value mismatch for key %q: got %q, want %q", key, val, expectedVal)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view restored tar store: %v", err)
	}
}

func TestImport_Preservation_EmptyState(t *testing.T) {
	ctx := context.Background()
	srcDir, closeSrc := createTestStorage(t)
	closeSrc()

	archivePath := filepath.Join(t.TempDir(), "empty.tar.gz")
	if err := archive.Export(ctx, archive.ExportOptions{
		StorageDir: srcDir,
		OutputFile: archivePath,
	}); err != nil {
		t.Fatalf("export empty store: %v", err)
	}

	preImportSHA := fileSHA256(t, archivePath)
	restoredDir := filepath.Join(t.TempDir(), "restored-empty")
	if err := archive.Import(ctx, archive.ImportOptions{
		InputFile:  archivePath,
		StorageDir: restoredDir,
	}); err != nil {
		t.Fatalf("import empty store: %v", err)
	}

	postImportSHA := fileSHA256(t, archivePath)
	if preImportSHA != postImportSHA {
		t.Fatalf("input empty archive modified: pre %x != post %x", preImportSHA, postImportSHA)
	}

	// Verify no blobs directory was created for empty store.
	blobsDir := filepath.Join(restoredDir, "blobs")
	if _, err := os.Stat(blobsDir); !os.IsNotExist(err) {
		t.Fatalf("expected blobs directory to not exist for empty store, got %v", err)
	}

	// Verify state.db permissions (0600).
	dbInfo, err := os.Stat(filepath.Join(restoredDir, "state.db"))
	if err != nil {
		t.Fatalf("stat empty restored state.db: %v", err)
	}
	if perm := dbInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("empty restored state.db permissions: got %o, want 0600", perm)
	}

	// Verify restored SQLite store reopens cleanly with 0 keys.
	store, err := sqlite.Open(ctx, restoredDir)
	if err != nil {
		t.Fatalf("open restored empty store: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.View(ctx, func(tx storage.ReadTx) error {
		keys := tx.Keys("")
		if len(keys) != 0 {
			t.Fatalf("expected 0 keys, got %d", len(keys))
		}
		return nil
	})
}

//nolint:contextcheck // store.Close does not accept a context parameter.
func TestImport_Preservation_DestinationRefusalAndIntegrity(t *testing.T) {
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

	preSHA := fileSHA256(t, archivePath)
	tempDir := t.TempDir()

	t.Run("destination exists as empty directory", func(t *testing.T) {
		existingEmptyDir := filepath.Join(tempDir, "existing-empty")
		if err := os.Mkdir(existingEmptyDir, 0o755); err != nil {
			t.Fatal(err)
		}

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: existingEmptyDir,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected destination already exists error, got %v", err)
		}

		// Ensure existing empty directory remains empty.
		entries, _ := os.ReadDir(existingEmptyDir)
		if len(entries) != 0 {
			t.Fatalf("expected existing empty directory to remain empty, found %d entries", len(entries))
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during empty dir refusal")
		}
	})

	t.Run("destination exists as populated directory", func(t *testing.T) {
		existingPopulated := filepath.Join(tempDir, "existing-populated")
		if err := os.Mkdir(existingPopulated, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinelFile := filepath.Join(existingPopulated, "sentinel.txt")
		if err := os.WriteFile(sentinelFile, []byte("preserve-me"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: existingPopulated,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected destination already exists error, got %v", err)
		}

		content, _ := os.ReadFile(sentinelFile)
		if string(content) != "preserve-me" {
			t.Fatalf("sentinel file was corrupted or overwritten: %s", string(content))
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during populated dir refusal")
		}
	})

	t.Run("destination exists as regular file", func(t *testing.T) {
		existingFile := filepath.Join(tempDir, "existing-file.txt")
		if err := os.WriteFile(existingFile, []byte("preserve-file"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: existingFile,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected destination already exists error, got %v", err)
		}

		content, _ := os.ReadFile(existingFile)
		if string(content) != "preserve-file" {
			t.Fatalf("file was modified: %s", string(content))
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during regular file refusal")
		}
	})

	t.Run("destination exists as symlink", func(t *testing.T) {
		targetDir := filepath.Join(tempDir, "symlink-target")
		if err := os.Mkdir(targetDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinelFile := filepath.Join(targetDir, "target-sentinel.txt")
		if err := os.WriteFile(sentinelFile, []byte("target-preserve"), 0o600); err != nil {
			t.Fatal(err)
		}

		symlinkPath := filepath.Join(tempDir, "dest-symlink")
		if err := os.Symlink(targetDir, symlinkPath); err != nil {
			t.Fatal(err)
		}

		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: symlinkPath,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected destination already exists error for symlink, got %v", err)
		}

		content, _ := os.ReadFile(sentinelFile)
		if string(content) != "target-preserve" {
			t.Fatalf("symlink target file modified: %s", string(content))
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during symlink refusal")
		}
	})

	t.Run("destination parent directory does not exist", func(t *testing.T) {
		missingParent := filepath.Join(tempDir, "no-such-parent", "restored")
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: missingParent,
		})
		if err == nil || !strings.Contains(err.Error(), "parent directory") {
			t.Fatalf("expected parent directory does not exist error, got %v", err)
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during missing parent refusal")
		}
	})

	t.Run("destination parent is a regular file", func(t *testing.T) {
		fileParent := filepath.Join(tempDir, "file-parent")
		if err := os.WriteFile(fileParent, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(fileParent, "child")
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: dest,
		})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("expected parent is not a directory error, got %v", err)
		}

		content, _ := os.ReadFile(fileParent)
		if string(content) != "data" {
			t.Fatalf("parent file modified: %s", string(content))
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during file parent refusal")
		}
	})

	t.Run("destination matches input archive", func(t *testing.T) {
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: archivePath,
		})
		if err == nil ||
			(!strings.Contains(err.Error(), "already exists") &&
				!strings.Contains(err.Error(), "matches input archive")) {
			t.Fatalf("expected error when destination matches input archive, got %v", err)
		}
		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during archive collision refusal")
		}
	})

	t.Run("input archive does not exist", func(t *testing.T) {
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  filepath.Join(tempDir, "nonexistent.tar.gz"),
			StorageDir: filepath.Join(tempDir, "fresh-1"),
		})
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("expected input does not exist error, got %v", err)
		}
		assertNoLeakedStagingDirs(t, tempDir)
	})

	t.Run("input archive is a directory", func(t *testing.T) {
		err := archive.Import(ctx, archive.ImportOptions{
			InputFile:  tempDir,
			StorageDir: filepath.Join(tempDir, "fresh-2"),
		})
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("expected input is a directory error, got %v", err)
		}
		assertNoLeakedStagingDirs(t, tempDir)
	})

	t.Run("active SQLite destination remains untouched", func(t *testing.T) {
		activeStoreDir := filepath.Join(tempDir, "active-sqlite-store")
		activeStore, err := sqlite.Open(ctx, activeStoreDir)
		if err != nil {
			t.Fatalf("create active sqlite store: %v", err)
		}
		defer func() { _ = activeStore.Close() }() //nolint:contextcheck // store.Close does not accept a context parameter.

		sentinelVal := []byte("active-payload-must-survive")
		err = activeStore.Update(ctx, func(tx storage.WriteTx) error {
			tx.Put("active-sentinel-key", sentinelVal)
			return nil
		})
		if err != nil {
			t.Fatalf("populate active store: %v", err)
		}

		// Attempt to import into the active SQLite store directory.
		impErr := archive.Import(ctx, archive.ImportOptions{
			InputFile:  archivePath,
			StorageDir: activeStoreDir,
		})
		if impErr == nil || !strings.Contains(impErr.Error(), "already exists") {
			t.Fatalf("expected import into active sqlite store to fail with 'already exists', got %v", impErr)
		}

		// Verify the active store was completely untouched and its data survives.
		err = activeStore.View(ctx, func(tx storage.ReadTx) error {
			val, ok := tx.Get("active-sentinel-key")
			if !ok {
				t.Fatalf("active sentinel key missing after import refusal")
			}
			if !bytes.Equal(val, sentinelVal) {
				t.Fatalf("active sentinel value modified: got %q, want %q", val, sentinelVal)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("view active store after refusal: %v", err)
		}

		assertNoLeakedStagingDirs(t, tempDir)
		if fileSHA256(t, archivePath) != preSHA {
			t.Fatalf("archive modified during active sqlite refusal")
		}
	})
}

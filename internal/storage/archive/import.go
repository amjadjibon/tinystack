package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// ImportOptions defines the parameters for importing an offline state fixture.
type ImportOptions struct {
	InputFile  string
	StorageDir string
}

// Test hooks for failure and race testing.
var (
	testBeforePublishDirHook func(stagingDir, destDir string) error
	createStagingDir         = os.MkdirTemp
	createBlobFile           = func(path string, perm os.FileMode) (io.WriteCloser, error) {
		// #nosec G304 -- path is strictly within the temporary staging directory.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err != nil {
			return nil, err
		}
		return &syncedBlobFile{file: f}, nil
	}
	openSQLiteStore = sqlite.Open
	syncDir         = syncDirectory
)

type syncedBlobFile struct {
	file *os.File
}

func (s *syncedBlobFile) Write(p []byte) (int, error) {
	return s.file.Write(p)
}

func (s *syncedBlobFile) Close() error {
	syncErr := s.file.Sync()
	closeErr := s.file.Close()
	return errors.Join(syncErr, closeErr)
}

type stagingBlobSink struct {
	blobsDir string
}

func (s *stagingBlobSink) CreateBlob(ctx context.Context, id string, _ int64) (io.WriteCloser, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err := os.MkdirAll(s.blobsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create blobs directory: %w", err)
	}
	blobPath := filepath.Join(s.blobsDir, id)
	return createBlobFile(blobPath, 0o600)
}

// Import imports an offline state fixture archive into a new, fresh storage directory.
//
// Semantics:
//  1. StorageDir must not already exist (even if empty).
//  2. Parent directory of StorageDir must exist.
//  3. Staging occurs in a private 0700 sibling directory on the same filesystem.
//  4. Archive format, records, digests, closure, and trailers are validated in a single pass.
//  5. Blobs are streamed and written to staging/blobs/<id> with 0600 permissions.
//  6. SQLite database is opened in staging, current schema created, records committed via tx.Put,
//     and closed cleanly before publication.
//  7. Practical durability flushes all blob files, SQLite database, and directory metadata.
//  8. Atomic publication renames the staging directory to StorageDir without replacing.
//  9. On failure or cancellation, staging directory is cleanly removed, leaving the destination untouched.
//
//nolint:contextcheck // store.Close does not accept a context parameter.
func Import(ctx context.Context, opts ImportOptions) (err error) {
	absInputFile, absStorageDir, err := ValidateImportPaths(opts.InputFile, opts.StorageDir)
	if err != nil {
		return err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	parentDir := filepath.Dir(absStorageDir)
	stagingDir, err := createStagingDir(parentDir, ".tinystack-import-*")
	if err != nil {
		return fmt.Errorf("create temporary staging directory: %w", err)
	}
	_ = os.Chmod(stagingDir, 0o700)

	var succeeded bool
	defer func() {
		if !succeeded {
			if removeErr := os.RemoveAll(stagingDir); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("clean up temporary staging directory %s: %w", stagingDir, removeErr))
			}
		}
	}()

	blobsDir := filepath.Join(stagingDir, "blobs")
	sink := &stagingBlobSink{blobsDir: blobsDir}

	manifest, records, err := readAndValidateArchive(ctx, absInputFile, sink)
	if err != nil {
		return err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// Populate SQLite store with current schema and committed records.
	store, err := openSQLiteStore(ctx, stagingDir)
	if err != nil {
		return fmt.Errorf("initialize storage database: %w", err)
	}

	var storeClosed bool
	defer func() {
		if !storeClosed {
			_ = store.Close()
		}
	}()

	err = store.Update(ctx, func(tx storage.WriteTx) error {
		for _, r := range records {
			tx.Put(r.Key, r.Value)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("commit state records: %w", err)
	}

	storeClosed = true
	if closeErr := store.Close(); closeErr != nil {
		return fmt.Errorf("close storage database: %w", closeErr)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// Practical durability: sync staging blobs directory (if created) and staging directory.
	if len(manifest.Blobs) > 0 {
		if syncErr := syncDir(blobsDir); syncErr != nil {
			return fmt.Errorf("sync staging blobs directory: %w", syncErr)
		}
	}
	if syncErr := syncDir(stagingDir); syncErr != nil {
		return fmt.Errorf("sync staging directory: %w", syncErr)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// Atomic publication without replacement.
	if pubErr := publishDirectoryNoReplace(stagingDir, absStorageDir); pubErr != nil {
		return pubErr
	}

	succeeded = true
	return nil
}

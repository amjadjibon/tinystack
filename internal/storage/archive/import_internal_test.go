package archive

import (
	"context"
	"io"
	"os"

	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// SetBeforePublishDirHookForTesting sets a hook called immediately before directory rename.
func SetBeforePublishDirHookForTesting(hook func(stagingDir, destDir string) error) func() {
	prev := testBeforePublishDirHook
	testBeforePublishDirHook = hook
	return func() {
		testBeforePublishDirHook = prev
	}
}

// SetCreateBlobFileForTesting replaces the blob file creation function for tests.
func SetCreateBlobFileForTesting(fn func(path string, perm os.FileMode) (io.WriteCloser, error)) func() {
	prev := createBlobFile
	createBlobFile = fn
	return func() {
		createBlobFile = prev
	}
}

// SetCreateStagingDirForTesting replaces the staging directory creation function for tests.
func SetCreateStagingDirForTesting(fn func(dir, pattern string) (string, error)) func() {
	prev := createStagingDir
	createStagingDir = fn
	return func() {
		createStagingDir = prev
	}
}

// SetOpenSQLiteStoreForTesting replaces the SQLite store opener for tests.
func SetOpenSQLiteStoreForTesting(fn func(ctx context.Context, dir string) (*sqlite.Store, error)) func() {
	prev := openSQLiteStore
	openSQLiteStore = fn
	return func() {
		openSQLiteStore = prev
	}
}

// SetSyncDirForTesting replaces the directory sync function for tests.
func SetSyncDirForTesting(fn func(dir string) error) func() {
	prev := syncDir
	syncDir = fn
	return func() {
		syncDir = prev
	}
}

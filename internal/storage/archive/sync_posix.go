//go:build !windows

package archive

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// syncDirectory flushes directory entry metadata to disk on POSIX systems.
// It joins any sync error (except EINVAL and ENOTSUP on filesystems where directory
// fsync is unsupported) with the directory handle Close error.
func syncDirectory(dir string) error {
	// #nosec G304 -- directory is within the staging directory path.
	f, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}

	syncErr := f.Sync()
	closeErr := f.Close()

	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, syscall.ENOTSUP) {
		return errors.Join(syncErr, closeErr)
	}
	return closeErr
}

//go:build windows

package archive

import (
	"os"
	"path/filepath"
)

// syncDirectory on Windows. Windows does not support FlushFileBuffers on directory handles
// (which requires GENERIC_WRITE and returns ERROR_ACCESS_DENIED or ERROR_INVALID_FUNCTION).
// Durability of file payloads and metadata is guaranteed by syncing individual blob files
// and the SQLite database upon write/close. We verify directory existence and accessibility.
func syncDirectory(dir string) error {
	stat, err := os.Stat(filepath.Clean(dir))
	if err != nil {
		return err
	}
	if !stat.IsDir() {
		return &os.PathError{Op: "sync", Path: dir, Err: os.ErrInvalid}
	}
	return nil
}

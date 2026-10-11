//go:build windows

package archive

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// publishDirectoryNoReplace atomically publishes stagingDir to destDir on Windows
// using MoveFileExW with MOVEFILE_WRITE_THROUGH and explicitly without MOVEFILE_REPLACE_EXISTING.
// When MOVEFILE_REPLACE_EXISTING is omitted, MoveFileExW strictly refuses to replace an
// existing file or directory, returning ERROR_ALREADY_EXISTS (183) or ERROR_FILE_EXISTS (80).
func publishDirectoryNoReplace(stagingDir, destDir string) error {
	if testBeforePublishDirHook != nil {
		if err := testBeforePublishDirHook(stagingDir, destDir); err != nil {
			return err
		}
	}

	fromPtr, err := windows.UTF16PtrFromString(stagingDir)
	if err != nil {
		return fmt.Errorf("invalid staging directory path: %w", err)
	}
	toPtr, err := windows.UTF16PtrFromString(destDir)
	if err != nil {
		return fmt.Errorf("invalid destination directory path: %w", err)
	}

	// MOVEFILE_WRITE_THROUGH requests the file move to be committed to disk before returning.
	// Omitting MOVEFILE_REPLACE_EXISTING guarantees Windows will fail if destDir already exists.
	err = windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_WRITE_THROUGH)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
			errors.Is(err, windows.ERROR_FILE_EXISTS) ||
			errors.Is(err, os.ErrExist) {
			return fmt.Errorf(
				"destination storage directory %q already exists: import requires a new destination directory",
				destDir,
			)
		}
		return fmt.Errorf("publish imported storage directory: %w", err)
	}

	return nil
}

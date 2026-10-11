//go:build darwin

package archive

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// publishDirectoryNoReplace atomically publishes stagingDir to destDir on Darwin
// using renamex_np with RENAME_EXCL (0x4). It fails if destDir already exists,
// even if destDir is an empty directory.
func publishDirectoryNoReplace(stagingDir, destDir string) error {
	if testBeforePublishDirHook != nil {
		if err := testBeforePublishDirHook(stagingDir, destDir); err != nil {
			return err
		}
	}

	err := unix.RenamexNp(stagingDir, destDir, unix.RENAME_EXCL)
	if err != nil {
		if errors.Is(err, unix.EEXIST) ||
			errors.Is(err, syscall.EEXIST) ||
			errors.Is(err, os.ErrExist) {
			return fmt.Errorf(
				"destination storage directory %q already exists: import requires a new destination directory",
				destDir,
			)
		}
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
			return fmt.Errorf(
				"atomic directory publication without replacement is not supported on this filesystem: %w",
				err,
			)
		}
		return fmt.Errorf("publish imported storage directory: %w", err)
	}

	return nil
}

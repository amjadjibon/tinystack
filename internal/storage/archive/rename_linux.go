//go:build linux

package archive

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// publishDirectoryNoReplace atomically publishes stagingDir to destDir on Linux
// using renameat2 with RENAME_NOREPLACE (0x1). It fails if destDir already exists,
// even if destDir is an empty directory.
func publishDirectoryNoReplace(stagingDir, destDir string) error {
	if testBeforePublishDirHook != nil {
		if err := testBeforePublishDirHook(stagingDir, destDir); err != nil {
			return err
		}
	}

	err := unix.Renameat2(unix.AT_FDCWD, stagingDir, unix.AT_FDCWD, destDir, unix.RENAME_NOREPLACE)
	if err != nil {
		if errors.Is(err, unix.EEXIST) ||
			errors.Is(err, syscall.EEXIST) ||
			errors.Is(err, os.ErrExist) {
			return fmt.Errorf(
				"destination storage directory %q already exists: import requires a new destination directory",
				destDir,
			)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf(
				"atomic directory publication without replacement is not supported on this kernel or filesystem: %w",
				err,
			)
		}
		return fmt.Errorf("publish imported storage directory: %w", err)
	}

	return nil
}

//go:build !darwin && !linux && !windows

package archive

import (
	"fmt"
	"runtime"
)

// publishDirectoryNoReplace returns an actionable error on unsupported platforms
// where atomic directory rename without replacement cannot be guaranteed.
func publishDirectoryNoReplace(stagingDir, destDir string) error {
	return fmt.Errorf(
		"atomic directory publication without replacement is not supported on %s/%s",
		runtime.GOOS,
		runtime.GOARCH,
	)
}

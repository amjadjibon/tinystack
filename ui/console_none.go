//go:build !console

package ui

import "io/fs"

// Files returns nil: this binary was built without the console tag.
func Files() fs.FS {
	return nil
}

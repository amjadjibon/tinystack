//go:build console

// Package ui embeds the console's build (task ui:build) into binaries built
// with the console tag (task build).
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist/client
var dist embed.FS

// Files returns the built console.
func Files() fs.FS {
	files, err := fs.Sub(dist, "dist/client")
	if err != nil {
		panic(err)
	}

	return files
}

package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SupportsDirectories advertises live bind-mounted deployment support.
func (r *Runtime) SupportsDirectories() bool { return true }

// fingerprint walks in lexical order, including directory entries so additions,
// deletions and renames invalidate warm environments. No contents are read.
// Symlinks are rejected: external targets cannot be tracked by this tree walk.
func fingerprint(directory string) (string, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return "", fmt.Errorf("hot reload directory: %w", err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("hot reload path is not a directory: %s", directory)
	}

	sum := sha256.New()
	err = filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("hot reload does not support symlinks: %s", name)
		}

		stat, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}

		if !stat.IsDir() && !stat.Mode().IsRegular() {
			return fmt.Errorf("hot reload does not support special files: %s", name)
		}

		relative, relErr := filepath.Rel(directory, name)
		if relErr != nil {
			return relErr
		}

		_, _ = fmt.Fprintf(sum, "%q/%d/%d/%d\n", relative, stat.Size(), stat.ModTime().UnixNano(), stat.Mode())
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint hot reload directory: %w", err)
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

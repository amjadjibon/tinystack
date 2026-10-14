package local

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AWS's uncompressed deployment limit includes the function and all layers.
const maxExtracted = 250 << 20

func extract(data []byte, root string, remaining *int64) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}

	for _, file := range archive.File {
		name := strings.TrimSuffix(file.Name, "/")
		if name == "" || !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe zip path %q", file.Name)
		}

		if file.Mode()&os.ModeSymlink != 0 || (!file.FileInfo().IsDir() && !file.Mode().IsRegular()) {
			return fmt.Errorf("unsupported zip file %q (symlinks and special files are forbidden)", file.Name)
		}

		target := filepath.Join(root, name)
		if file.FileInfo().IsDir() {
			if mkdirErr := os.MkdirAll(target, 0o700); mkdirErr != nil {
				return mkdirErr
			}

			continue
		}

		if *remaining < 0 || file.UncompressedSize64 > uint64(*remaining) {
			return fmt.Errorf("deployment exceeds %d extracted bytes", maxExtracted)
		}

		if mkdirErr := os.MkdirAll(filepath.Dir(target), 0o700); mkdirErr != nil {
			return mkdirErr
		}

		reader, openErr := file.Open()
		if openErr != nil {
			return openErr
		}

		// #nosec G304 -- target is under a private directory; all archive paths and symlinks are checked above.
		output, createErr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600|file.Mode().Perm()&0o100)
		if createErr != nil {
			_ = reader.Close()
			return createErr
		}

		written, copyErr := io.Copy(output, io.LimitReader(reader, *remaining+1))
		closeErr := output.Close()
		_ = reader.Close()
		*remaining -= written
		if copyErr != nil {
			return copyErr
		}

		if closeErr != nil {
			return closeErr
		}

		if *remaining < 0 {
			return fmt.Errorf("deployment exceeds %d extracted bytes", maxExtracted)
		}
	}

	return nil
}

// Content hashes avoid missing edits that preserve timestamps and file size.
func fingerprint(root string) (string, error) {
	sum := sha256.New()
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("hot reload does not support symlinks: %s", name)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("hot reload does not support special files: %s", name)
		}

		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}

		_, _ = fmt.Fprintf(sum, "%q/%d/%d\n", relative, info.Mode(), info.Size())
		if !info.IsDir() {
			// #nosec G304,G122 -- user-selected live code is not sandboxed; symlinks are rejected during the tree walk.
			file, openErr := os.Open(name)
			if openErr != nil {
				return openErr
			}

			_, err = io.Copy(sum, file)
			_ = file.Close()
			return err
		}

		return nil
	})
	return hex.EncodeToString(sum.Sum(nil)), err
}

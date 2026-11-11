package qemu

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
)

// fetch serializes writers across processes. An interrupted lock times out explicitly;
// it is never stolen from a possibly live downloader.
func fetch(ctx context.Context, dir string, image config.VMImage) (string, error) {
	algorithm, digest, _ := strings.Cut(image.Digest, ":")
	raw, err := hex.DecodeString(digest)
	valid := (algorithm == "sha256" && len(raw) == 32) || (algorithm == digestSHA512 && len(raw) == 64)
	if err != nil || !valid {
		return "", fmt.Errorf("invalid image digest %q", image.Digest)
	}

	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return "", mkdirErr
	}

	path := filepath.Join(dir, algorithm+"-"+strings.ToLower(digest)+".qcow2")
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = os.Mkdir(path+".lock", 0o700)
		if err == nil {
			break
		}

		if !os.IsExist(err) {
			return "", err
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}

	defer func() { _ = os.Remove(path + ".lock") }()
	// Recheck published cache entries before handing them to qemu-img as backing files.
	// #nosec G304 -- digest-derived file within the configured TinyStack cache.
	cached, cacheErr := os.Open(path)
	if cacheErr == nil {
		h := sha256.New()
		if algorithm == digestSHA512 {
			h = sha512.New()
		}

		_, readErr := io.Copy(h, cached)
		closeErr := cached.Close()
		if readErr != nil {
			return "", readErr
		}

		if closeErr != nil {
			return "", closeErr
		}

		if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(digest) {
			_ = os.Remove(path)
			return "", fmt.Errorf("cached image %s: digest mismatch; file discarded", path)
		}

		return path, nil
	}

	if !os.IsNotExist(cacheErr) {
		return "", cacheErr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, image.URL, http.NoBody)
	if err != nil {
		return "", err
	}

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}

	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image download: %s", response.Status)
	}

	file, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}

	defer func() { _ = os.Remove(file.Name()) }()
	h := sha256.New()
	if algorithm == digestSHA512 {
		h = sha512.New()
	}

	_, copyErr := io.Copy(io.MultiWriter(file, h), response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}

	if closeErr != nil {
		return "", closeErr
	}

	if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(digest) {
		return "", fmt.Errorf("image %s: %s digest mismatch; downloaded file discarded", image.URL, algorithm)
	}

	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}

	return path, nil
}

const digestSHA512 = "sha512"

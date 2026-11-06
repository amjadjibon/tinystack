package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	releaseRepo = "https://github.com/amjadjibon/tinystack"
	// maxDownload bounds an archive and the binary inside it; release archives are tens of megabytes.
	maxDownload = 512 << 20
	windows     = "windows"
)

func newUpdateCommand() *cobra.Command {
	var to string
	update := &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with the latest release (or --to a given one)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}

			exe, err = filepath.EvalSymlinks(exe)
			if err != nil {
				return err
			}

			u := &updater{
				client:  &http.Client{Timeout: 5 * time.Minute},
				repo:    releaseRepo,
				current: version,
				goos:    runtime.GOOS,
				goarch:  runtime.GOARCH,
				out:     cmd.OutOrStdout(),
			}
			return u.run(cmd.Context(), exe, to)
		},
	}

	update.Flags().StringVar(&to, "to", "", "release to install, e.g. 0.0.1 (default the latest)")
	return update
}

// updater downloads a release archive from repo, verifies it against the release's checksums.txt, and replaces exe.
type updater struct {
	client  *http.Client
	out     io.Writer
	repo    string
	current string
	goos    string
	goarch  string
}

func (u *updater) run(ctx context.Context, exe, to string) error {
	if to == "" && strings.HasSuffix(u.current, "-dev") {
		return fmt.Errorf("tinystack %s is a development build; rebuild it from source, or pass --to to install a "+
			"release over it", u.current)
	}

	target := strings.TrimPrefix(to, "v")
	if target == "" {
		latest, err := u.latest(ctx)
		if err != nil {
			return err
		}

		target = latest
	}

	if target == u.current {
		_, err := fmt.Fprintf(u.out, "tinystack %s is already installed at %s\n", target, exe)
		return err
	}

	name := fmt.Sprintf("tinystack_%s_%s_%s", target, u.goos, u.goarch)
	archive := name + ".tar.gz"
	if u.goos == windows {
		archive = name + ".zip"
	}

	base := fmt.Sprintf("%s/releases/download/v%s/", u.repo, target)
	data, err := u.download(ctx, base+archive)
	if err != nil {
		return err
	}

	sums, err := u.download(ctx, base+"checksums.txt")
	if err != nil {
		return err
	}

	if checkErr := verifyChecksum(sums, archive, data); checkErr != nil {
		return checkErr
	}

	binary, err := extractBinary(archive, data, name+"/"+binaryName(u.goos))
	if err != nil {
		return err
	}

	if replaceErr := replaceExecutable(exe, binary); replaceErr != nil {
		return replaceErr
	}

	_, err = fmt.Fprintf(u.out, "Updated tinystack %s to %s at %s\n", u.current, target, exe)
	return err
}

// latest reads the release tag from the redirect of /releases/latest, which, unlike the API, is not rate limited.
func (u *updater) latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.repo+"/releases/latest", http.NoBody)
	if err != nil {
		return "", err
	}

	client := *u.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve the latest release: %w", err)
	}

	_ = resp.Body.Close()
	tag := path.Base(resp.Header.Get("Location"))
	if resp.StatusCode/100 != 3 || !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("resolve the latest release: no published release (status %d)", resp.StatusCode)
	}

	return strings.TrimPrefix(tag, "v"), nil
}

func (u *updater) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}

	return readLimited(resp.Body)
}

func verifyChecksum(sums []byte, archive string, data []byte) error {
	for line := range strings.Lines(string(sums)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != archive {
			continue
		}

		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != fields[0] {
			return fmt.Errorf("checksum mismatch for %s", archive)
		}

		return nil
	}

	return fmt.Errorf("%s is not listed in checksums.txt", archive)
}

// extractBinary returns the file at member inside a release .tar.gz or .zip archive.
func extractBinary(archive string, data []byte, member string) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}

		for _, f := range zr.File {
			if f.Name != member {
				continue
			}

			r, err := f.Open()
			if err != nil {
				return nil, err
			}

			binary, err := readLimited(r)
			_ = r.Close()
			return binary, err
		}

		return nil, fmt.Errorf("%s has no %s", archive, member)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no %s", archive, member)
		}

		if err != nil {
			return nil, err
		}

		if hdr.Name == member && hdr.Typeflag == tar.TypeReg {
			return readLimited(tr)
		}
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDownload+1))
	if err != nil {
		return nil, err
	}

	if len(data) > maxDownload {
		return nil, fmt.Errorf("download exceeds %d MiB", maxDownload>>20)
	}

	return data, nil
}

func binaryName(goos string) string {
	if goos == windows {
		return "tinystack.exe"
	}

	return "tinystack"
}

// replaceExecutable writes binary next to exe and renames it into place, so a running process keeps its old file.
// Windows cannot replace a running executable, but it can rename it, so the old one moves aside to exe.old first.
func replaceExecutable(exe string, binary []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".tinystack-update-*")
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("cannot write to %s; rerun with sudo: %w", dir, err)
	}

	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(binary); err != nil {
		_ = tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	// #nosec G302 -- the file is the tinystack executable, which every user must be able to run.
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}

	if runtime.GOOS == windows {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}

		if err := os.Rename(tmp.Name(), exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}

		return nil
	}

	return os.Rename(tmp.Name(), exe)
}

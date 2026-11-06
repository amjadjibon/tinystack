package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves /releases/latest (redirecting to v0.0.2) and one release's archive and checksums.txt.
func fakeRelease(t *testing.T, archive string, data []byte, sum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v0.0.2", http.StatusFound)
	})

	mux.HandleFunc("GET /releases/download/v0.0.2/"+archive, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /releases/download/v0.0.2/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  other.tar.gz\n%s  %s\n", strings.Repeat("0", 64), sum, archive)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{name + "/LICENSE", []byte("license")}, {name + "/tinystack", body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func installed(t *testing.T, body string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "tinystack")
	if err := os.WriteFile(exe, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return exe
}

func testUpdater(server *httptest.Server, current string, out *bytes.Buffer) *updater {
	return &updater{
		client: server.Client(), repo: server.URL, current: current, goos: "linux", goarch: "arm64", out: out,
	}
}

func TestUpdateReplacesTheBinaryWithTheLatestRelease(t *testing.T) {
	archive := "tinystack_0.0.2_linux_arm64.tar.gz"
	data := tarGz(t, "tinystack_0.0.2_linux_arm64", []byte("new binary"))
	server := fakeRelease(t, archive, data, sha(data))
	exe := installed(t, "old binary")
	var out bytes.Buffer

	if err := testUpdater(server, "0.0.1", &out).run(t.Context(), exe, ""); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(exe) // #nosec G304 -- a test temp file.
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "new binary" || info.Mode().Perm() != 0o755 {
		t.Fatalf("binary = %q mode %v, want the release binary with mode 0755", got, info.Mode().Perm())
	}

	if !strings.Contains(out.String(), "Updated tinystack 0.0.1 to 0.0.2") {
		t.Fatalf("output = %q", out.String())
	}

	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory entries = %v (%v), want only the binary", entries, err)
	}
}

func TestUpdateKeepsTheBinaryWhenNothingChangesOrVerificationFails(t *testing.T) {
	archive := "tinystack_0.0.2_linux_arm64.tar.gz"
	data := tarGz(t, "tinystack_0.0.2_linux_arm64", []byte("new binary"))
	cases := []struct {
		name, current, sum, to, want string
		ok                           bool
	}{
		{name: "already latest", current: "0.0.2", sum: sha(data), want: "already installed", ok: true},
		{name: "checksum mismatch", current: "0.0.1", sum: strings.Repeat("a", 64), want: "checksum mismatch"},
		{name: "unstamped build", current: "dev", sum: sha(data), want: "development build"},
		{name: "build after a tag", current: "0.0.1-3-g1a2b3c4d5e6f", sum: sha(data), want: "development build"},
		{name: "build with local edits", current: "0.0.2-dirty", sum: sha(data), want: "development build"},
		{name: "missing release", current: "0.0.1", sum: sha(data), to: "9.9.9", want: "404"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := fakeRelease(t, archive, data, c.sum)
			exe := installed(t, "old binary")
			var out bytes.Buffer
			err := testUpdater(server, c.current, &out).run(t.Context(), exe, c.to)
			if c.ok != (err == nil) || !strings.Contains(fmt.Sprint(err)+out.String(), c.want) {
				t.Fatalf("err = %v, output = %q, want %q", err, out.String(), c.want)
			}

			got, readErr := os.ReadFile(exe) // #nosec G304 -- a test temp file.
			if readErr != nil || string(got) != "old binary" {
				t.Fatalf("binary = %q (%v), want it unchanged", got, readErr)
			}
		})
	}
}

func TestUpdateReadsWindowsZipArchives(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("tinystack_0.0.2_windows_amd64/tinystack.exe")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = w.Write([]byte("windows binary")); err != nil {
		t.Fatal(err)
	}

	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := extractBinary("tinystack_0.0.2_windows_amd64.zip", buf.Bytes(),
		"tinystack_0.0.2_windows_amd64/"+binaryName("windows"))
	if err != nil || string(got) != "windows binary" {
		t.Fatalf("binary = %q (%v)", got, err)
	}
}

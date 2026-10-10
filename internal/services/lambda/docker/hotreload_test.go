package docker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHotReloadFingerprint(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.py")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func() string {
		t.Helper()
		got, err := fingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}
	changed := func(previous string) string {
		t.Helper()
		next := stamp()
		if next == previous {
			t.Fatal("tree change did not change fingerprint")
		}

		return next
	}
	write("one")
	first := stamp()
	if stamp() != first {
		t.Fatal("unchanged tree has unstable fingerprint")
	}

	write("two")
	// Force a distinct mtime even on filesystems with coarse timestamp resolution.
	at := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, at, at); err != nil {
		t.Fatal(err)
	}

	first = changed(first)
	write("longer")
	first = changed(first)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}

	first = changed(first)
	renamed := filepath.Join(dir, "nested", "app.py")
	if err := os.Rename(file, renamed); err != nil {
		t.Fatal(err)
	}

	first = changed(first)
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}

	changed(first)
	if err := os.Symlink(t.TempDir(), file); err != nil {
		t.Fatal(err)
	}

	if _, err := fingerprint(dir); err == nil {
		t.Fatal("symlink accepted")
	}

	if _, err := fingerprint(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
}

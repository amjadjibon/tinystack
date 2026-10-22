package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCAExport(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  mode: persistent\n  path: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	root := newRootCommand()
	root.SetOut(&stdout)
	root.SetArgs([]string{"ca", "export", "--config", cfg})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "export.pem")
	root = newRootCommand()
	root.SetArgs([]string{"ca", "export", "--config", cfg, "--out", out})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, stdout.Bytes()) || !bytes.Contains(got, []byte("BEGIN CERTIFICATE")) {
		t.Fatal("export differs between stdout and file")
	}
}

package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestStateExportCommand(t *testing.T) {
	storageDir := t.TempDir()
	store, openErr := sqlite.Open(context.Background(), storageDir)
	if openErr != nil {
		t.Fatalf("sqlite.Open: %v", openErr)
	}

	_ = store.Update(context.Background(), func(tx storage.WriteTx) error {
		tx.Put("test-key", []byte("test-value"))
		return nil
	})
	_ = store.Close()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "state-export.tar.gz")

	t.Run("missing required flags", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.SetArgs([]string{"state", "export"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--storage-dir") {
			t.Fatalf("expected error for missing --storage-dir, got %v", err)
		}

		cmd = newRootCommand()
		cmd.SetArgs([]string{"state", "export", "--storage-dir", storageDir})
		err = cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--output") {
			t.Fatalf("expected error for missing --output, got %v", err)
		}
	})

	t.Run("successful export and overwrite handling", func(t *testing.T) {
		cmd := newRootCommand()
		buf := new(bytes.Buffer)
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{
			"state", "export",
			"--storage-dir", storageDir,
			"--output", outFile,
		})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("state export failed: %v", err)
		}

		// Re-running without --overwrite should fail.
		cmd = newRootCommand()
		cmd.SetArgs([]string{
			"state", "export",
			"--storage-dir", storageDir,
			"--output", outFile,
		})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected refusal when destination exists, got %v", err)
		}

		// Re-running with --overwrite should succeed.
		cmd = newRootCommand()
		cmd.SetArgs([]string{
			"state", "export",
			"--storage-dir", storageDir,
			"--output", outFile,
			"--overwrite",
		})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("state export with --overwrite failed: %v", err)
		}
	})
}

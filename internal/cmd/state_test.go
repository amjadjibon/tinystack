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

		// Re-export without --overwrite should fail.
		cmd = newRootCommand()
		cmd.SetArgs([]string{
			"state", "export",
			"--storage-dir", storageDir,
			"--output", outFile,
		})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected error when destination exists without --overwrite, got %v", err)
		}

		// Re-export with --overwrite should succeed.
		cmd = newRootCommand()
		cmd.SetOut(buf)
		cmd.SetErr(buf)
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

func TestStateImportCommand(t *testing.T) {
	// 1. Create a populated store and export it to an archive.
	srcDir := t.TempDir()
	store, openErr := sqlite.Open(context.Background(), srcDir)
	if openErr != nil {
		t.Fatalf("sqlite.Open: %v", openErr)
	}

	_ = store.Update(context.Background(), func(tx storage.WriteTx) error {
		tx.Put("imported-key-1", []byte("value-1"))
		tx.Put("imported-key-2", []byte("value-2"))
		return nil
	})
	_ = store.Close()

	archiveDir := t.TempDir()
	archiveFile := filepath.Join(archiveDir, "export.tar.gz")

	exportCmd := newRootCommand()
	exportCmd.SetArgs([]string{
		"state", "export",
		"--storage-dir", srcDir,
		"--output", archiveFile,
	})
	if err := exportCmd.Execute(); err != nil {
		t.Fatalf("export fixture: %v", err)
	}

	parentDir := t.TempDir()
	freshDest := filepath.Join(parentDir, "restored-state")

	t.Run("missing required flags", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.SetArgs([]string{"state", "import"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--input") {
			t.Fatalf("expected error for missing --input, got %v", err)
		}

		cmd = newRootCommand()
		cmd.SetArgs([]string{"state", "import", "--input", archiveFile})
		err = cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--storage-dir") {
			t.Fatalf("expected error for missing --storage-dir, got %v", err)
		}
	})

	t.Run("successful import into fresh destination", func(t *testing.T) {
		cmd := newRootCommand()
		buf := new(bytes.Buffer)
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{
			"state", "import",
			"--input", archiveFile,
			"--storage-dir", freshDest,
		})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("state import failed: %v", err)
		}

		// Verify restored database contents.
		restoredStore, err := sqlite.Open(context.Background(), freshDest)
		if err != nil {
			t.Fatalf("open restored store: %v", err)
		}
		defer func() { _ = restoredStore.Close() }()

		_ = restoredStore.View(context.Background(), func(tx storage.ReadTx) error {
			val1, ok1 := tx.Get("imported-key-1")
			if !ok1 || string(val1) != "value-1" {
				t.Fatalf("restored key-1 mismatch: ok=%v, val=%q", ok1, string(val1))
			}
			val2, ok2 := tx.Get("imported-key-2")
			if !ok2 || string(val2) != "value-2" {
				t.Fatalf("restored key-2 mismatch: ok=%v, val=%q", ok2, string(val2))
			}
			return nil
		})
	})

	t.Run("refusal when destination already exists", func(t *testing.T) {
		// freshDest now exists from the previous run; import must refuse.
		cmd := newRootCommand()
		cmd.SetArgs([]string{
			"state", "import",
			"--input", archiveFile,
			"--storage-dir", freshDest,
		})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected refusal when destination exists, got %v", err)
		}
	})
}

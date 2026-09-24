package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
	"github.com/amjadjibon/tinystack/internal/storage/storagetest"
)

func open(t *testing.T, dir string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestContract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store { return open(t, t.TempDir()) })
}

func TestReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	for _, fn := range []func(storage.WriteTx) error{
		func(tx storage.WriteTx) error {
			tx.Put("kept", []byte("one"))
			tx.Put("replaced", []byte("old"))
			tx.Put("deleted", []byte("x"))
			return nil
		},
		func(tx storage.WriteTx) error {
			tx.Put("replaced", []byte("new"))
			tx.Delete("deleted")
			tx.Put("empty", []byte{})
			return nil
		},
		func(tx storage.WriteTx) error {
			tx.Put("rolled back", []byte("x"))
			return errors.New("abort")
		},
	} {
		if err := s.Update(ctx, fn); err != nil && err.Error() != "abort" {
			t.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	err := s.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put("after close", []byte("x"))
		return nil
	})
	if _, ok := storagetest.Snapshot(t, s)["after close"]; err == nil || ok {
		t.Fatalf("update after Close: %v", err)
	}

	got := storagetest.Snapshot(t, open(t, dir))
	_, empty := got["empty"]
	if len(got) != 3 || string(got["kept"]) != "one" || string(got["replaced"]) != "new" || !empty {
		t.Fatalf("reopened %q", got)
	}

	info, err := os.Stat(filepath.Join(dir, sqlite.File))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v", info.Mode())
	}
}

func TestExclusive(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	if _, err := sqlite.Open(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second open: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	open(t, dir)
}

func TestUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, sqlite.File))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(context.Background(), "PRAGMA user_version = 99")
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}

	if _, err = sqlite.Open(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("open: %v", err)
	}
}

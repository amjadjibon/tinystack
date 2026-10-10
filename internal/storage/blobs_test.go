package storage_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
)

func TestBlobPinsAndCollection(t *testing.T) {
	blobs, err := storage.OpenBlobs("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	b, err := blobs.Write(bytes.NewBufferString("contents"))
	if err != nil {
		t.Fatal(err)
	}
	if err = blobs.Collect(nil); err != nil {
		t.Fatal(err)
	}
	// Collection must not retire a pending write that has not committed yet.
	blobs.Release(b)
	if err = blobs.Collect(map[string]bool{b.ID: true}); err != nil {
		t.Fatal(err)
	}
	blobs.Pin(b)
	blobs.Retire(map[string]bool{b.ID: true}, nil)
	file, err := blobs.Open(b)
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "contents" {
		t.Fatal(string(data), err)
	}
	blobs.Release(b)
	if _, err = os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retired file retained", err)
	}
	orphan, err := blobs.Write(bytes.NewBufferString("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	blobs.Release(orphan)
	if err = blobs.Collect(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = blobs.Open(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan retained", err)
	}
	if err = blobs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary directory retained", err)
	}
}

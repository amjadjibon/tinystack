package archive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func TestParentExportPreservesBinaryKey(t *testing.T) {
	dir, closeStore := createTestStorage(t)
	closeStore()
	key := "global/" + string([]byte{0xff, 0xfe})
	populateTestStore(t, dir, map[string][]byte{key: {0, 255}}, nil)
	out := filepath.Join(t.TempDir(), "fixture.tar")
	if err := archive.Export(
		context.Background(),
		archive.ExportOptions{StorageDir: dir, OutputFile: out},
	); err != nil {
		t.Fatal(err)
	}
	_, records, err := archive.InspectArchive(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Key != key {
		t.Fatalf("opaque key changed: got %q, want %q", records[0].Key, key)
	}
}

func TestParentExportRefusesSymlinkedBlobOutput(t *testing.T) {
	dir, closeStore := createTestStorage(t)
	closeStore()
	id := "0123456789abcdef0123456789abcdef"
	body := []byte("preserve source body")
	populateTestStore(t, dir, map[string][]byte{"body": (storage.Blob{ID: id, Size: int64(len(body))}).Record()}, nil)
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(dir, "blobs")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(external, id)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	err := archive.Export(
		context.Background(),
		archive.ExportOptions{StorageDir: dir, OutputFile: path, Overwrite: true},
	)
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, body) {
		t.Fatalf("source body was overwritten: export error %v, read error %v", err, readErr)
	}
	if err == nil {
		t.Fatal("export must reject an output in the resolved blob directory")
	}
}

func TestParentInspectRejectsMissingReferencedBlob(t *testing.T) {
	ref := storage.Blob{ID: "0123456789abcdef0123456789abcdef", Size: 3}
	recordBytes, err := json.Marshal([]archive.Record{{Key: "body", Value: ref.Record()}})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(recordBytes)
	manifestBytes, err := json.Marshal(archive.Manifest{
		FormatVersion: archive.FormatVersion,
		Records: archive.RecordsMeta{
			Count:      1,
			TotalBytes: int64(len(recordBytes)),
			SHA256:     hex.EncodeToString(hash[:]),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, member := range []struct {
		name string
		data []byte
	}{{"manifest.json", manifestBytes}, {"records.json", recordBytes}} {
		hdr := &tar.Header{
			Name:     member.name,
			Size:     int64(len(member.data)),
			Mode:     0o600,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "missing-body.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.ValidateArchive(context.Background(), path); err == nil {
		t.Fatal("archive with a body reference but no corresponding blob was accepted")
	}
}

func TestParentInspectRejectsTruncatedGzipTrailer(t *testing.T) {
	dir, closeStore := createTestStorage(t)
	closeStore()
	out := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := archive.Export(
		context.Background(),
		archive.ExportOptions{StorageDir: dir, OutputFile: out},
	); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, data[:len(data)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := archive.InspectArchive(context.Background(), out); err == nil {
		t.Fatal("archive with a truncated gzip trailer was accepted")
	}
}

func TestParentExportRefusesSymlinkedBlobFile(t *testing.T) {
	dir, closeStore := createTestStorage(t)
	closeStore()
	id := "0123456789abcdef0123456789abcdef"
	body := []byte("source bytes outside blob directory")
	populateTestStore(t, dir, map[string][]byte{"body": (storage.Blob{ID: id, Size: int64(len(body))}).Record()}, nil)
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "original-body")
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "blobs", id)); err != nil {
		t.Fatal(err)
	}
	err := archive.Export(
		context.Background(),
		archive.ExportOptions{StorageDir: dir, OutputFile: target, Overwrite: true},
	)
	after, readErr := os.ReadFile(target)
	if readErr != nil || !bytes.Equal(after, body) {
		t.Fatalf("symlinked source body was overwritten: export error %v, read error %v", err, readErr)
	}
	if err == nil {
		t.Fatal("export must refuse a symlinked body escaping source ownership")
	}
}

func TestParentInspectRejectsAppendedGzipData(t *testing.T) {
	dir, closeStore := createTestStorage(t)
	closeStore()
	out := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := archive.Export(
		context.Background(),
		archive.ExportOptions{StorageDir: dir, OutputFile: out},
	); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var extraMember bytes.Buffer
	gw := gzip.NewWriter(&extraMember)
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		trailer []byte
	}{
		{"junk", []byte("unexpected appended data")},
		{"second member", data},
		{"empty second member", extraMember.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "appended.tar.gz")
			payload := append(append([]byte(nil), data...), tc.trailer...)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := archive.ValidateArchive(context.Background(), path); err == nil {
				t.Fatal("archive with appended compressed data was accepted")
			}
		})
	}
}

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
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func writeValidArchiveBytes(t *testing.T) []byte {
	t.Helper()
	records := []archive.Record{
		{Key: "k1", Value: []byte("v1")},
	}
	recBytes, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	recHash := sha256.Sum256(recBytes)

	manifest := archive.Manifest{
		FormatVersion:    archive.FormatVersion,
		TinyStackVersion: archive.TinyStackVersion,
		Records: archive.RecordsMeta{
			Count:      len(records),
			TotalBytes: int64(len(recBytes)),
			SHA256:     hex.EncodeToString(recHash[:]),
		},
		Totals: archive.TotalsMeta{
			BlobCount:      0,
			TotalBlobBytes: 0,
		},
		Blobs: nil,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Manifest
	if err := tw.WriteHeader(&tar.Header{
		Name:     archive.ManifestEntryName,
		Size:     int64(len(manifestBytes)),
		Mode:     0o600,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		t.Fatal(err)
	}

	// Records
	if err := tw.WriteHeader(&tar.Header{
		Name:     archive.RecordsEntryName,
		Size:     int64(len(recBytes)),
		Mode:     0o600,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(recBytes); err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func assertImportRejection(t *testing.T, archiveData []byte, expectedErrSubstring string) {
	t.Helper()
	parentDir := t.TempDir()
	archiveFile := filepath.Join(parentDir, "malicious.tar.gz")
	if err := os.WriteFile(archiveFile, archiveData, 0o600); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(parentDir, "should-not-exist")
	err := archive.Import(context.Background(), archive.ImportOptions{
		InputFile:  archiveFile,
		StorageDir: destDir,
	})

	if err == nil {
		t.Fatalf("expected import to fail with substring %q, got nil error", expectedErrSubstring)
	}

	if expectedErrSubstring != "" && !strings.Contains(err.Error(), expectedErrSubstring) {
		t.Fatalf("expected error containing %q, got %v", expectedErrSubstring, err)
	}

	// Verify destination was never created.
	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Fatalf("destination directory %s should not exist after failed import", destDir)
	}

	// Verify no staging directory was leaked in parentDir.
	entries, _ := os.ReadDir(parentDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tinystack-import-") {
			t.Fatalf("temporary staging directory %s leaked after failed import", e.Name())
		}
	}
}

func TestImport_AdversarialTarStructure(t *testing.T) {
	t.Run("symlink tar entry", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Typeflag: tar.TypeSymlink,
			Linkname: "/etc/passwd",
		})
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "non-regular file type flag")
	})

	t.Run("hardlink tar entry", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Typeflag: tar.TypeLink,
			Linkname: "target.txt",
		})
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "non-regular file type flag")
	})

	t.Run("device tar entry", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Typeflag: tar.TypeChar,
		})
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "non-regular file type flag")
	})

	t.Run("directory tar entry", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     "somedir/",
			Typeflag: tar.TypeDir,
		})
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "non-regular file type flag")
	})

	t.Run("directory traversal entry name", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     "../escape.txt",
			Size:     4,
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte("evil"))
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "not a canonical relative path")
	})

	t.Run("absolute path entry name", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     "/etc/evil",
			Size:     4,
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte("evil"))
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "not a canonical relative path")
	})

	t.Run("duplicate archive entry", func(t *testing.T) {
		records := []archive.Record{{Key: "k1", Value: []byte("v1")}}
		recBytes, _ := json.Marshal(records)
		recHash := sha256.Sum256(recBytes)

		manifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      1,
				TotalBytes: int64(len(recBytes)),
				SHA256:     hex.EncodeToString(recHash[:]),
			},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Size:     int64(len(mData)),
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write(mData)
		// Duplicate entry with same name
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Size:     int64(len(mData)),
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write(mData)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "duplicate archive entry")
	})

	t.Run("first entry not manifest", func(t *testing.T) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.RecordsEntryName,
			Size:     2,
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte("[]"))
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "invalid first archive entry")
	})

	t.Run("unsupported format version", func(t *testing.T) {
		manifest := archive.Manifest{
			FormatVersion: 99,
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Size:     int64(len(mData)),
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write(mData)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "unsupported format version")
	})

	t.Run("negative counts in manifest", func(t *testing.T) {
		manifest := archive.Manifest{
			FormatVersion: archive.FormatVersion,
			Records: archive.RecordsMeta{
				Count: -1,
			},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{
			Name:     archive.ManifestEntryName,
			Size:     int64(len(mData)),
			Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write(mData)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "manifest contains negative counts or sizes")
	})
}

func TestImport_AdversarialIntegrityAndClosure(t *testing.T) {
	t.Run("tampered records payload SHA-256", func(t *testing.T) {
		records := []archive.Record{{Key: "k1", Value: []byte("tampered-value")}}
		recBytes, _ := json.Marshal(records)

		manifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      1,
				TotalBytes: int64(len(recBytes)),
				SHA256:     strings.Repeat("0", 64), // Mismatched SHA256
			},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.ManifestEntryName, Size: int64(len(mData)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(mData)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.RecordsEntryName, Size: int64(len(recBytes)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(recBytes)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "records SHA-256 mismatch")
	})

	t.Run("unsorted record keys in records payload", func(t *testing.T) {
		records := []archive.Record{
			{Key: "z_key", Value: []byte("1")},
			{Key: "a_key", Value: []byte("2")},
		}
		recBytes, _ := json.Marshal(records)
		recHash := sha256.Sum256(recBytes)

		manifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      len(records),
				TotalBytes: int64(len(recBytes)),
				SHA256:     hex.EncodeToString(recHash[:]),
			},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.ManifestEntryName, Size: int64(len(mData)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(mData)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.RecordsEntryName, Size: int64(len(recBytes)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(recBytes)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "unsorted record keys")
	})

	t.Run("tampered blob payload SHA-256", func(t *testing.T) {
		blobID := "0123456789abcdef0123456789abcdef"
		blobData := []byte("original content")
		tamperedData := []byte("tampered content")
		actualHash := sha256.Sum256(blobData)

		ref := storage.Blob{ID: blobID, Size: int64(len(blobData))}
		records := []archive.Record{{Key: "blob-record", Value: ref.Record()}}
		recBytes, _ := json.Marshal(records)
		recHash := sha256.Sum256(recBytes)

		manifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      1,
				TotalBytes: int64(len(recBytes)),
				SHA256:     hex.EncodeToString(recHash[:]),
			},
			Totals: archive.TotalsMeta{
				BlobCount:      1,
				TotalBlobBytes: int64(len(blobData)),
			},
			Blobs: []archive.BlobMeta{
				{
					ID:     blobID,
					Size:   int64(len(blobData)),
					SHA256: hex.EncodeToString(actualHash[:]), // Original hash declared
				},
			},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.ManifestEntryName, Size: int64(len(mData)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(mData)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.RecordsEntryName, Size: int64(len(recBytes)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(recBytes)
		// Write tampered data of same length
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.BlobsPrefix + blobID, Size: int64(len(tamperedData)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(tamperedData)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "SHA-256 mismatch")
	})

	t.Run("unlisted orphan blob in archive", func(t *testing.T) {
		records := []archive.Record{{Key: "k1", Value: []byte("v1")}}
		recBytes, _ := json.Marshal(records)
		recHash := sha256.Sum256(recBytes)

		manifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      1,
				TotalBytes: int64(len(recBytes)),
				SHA256:     hex.EncodeToString(recHash[:]),
			},
			Totals: archive.TotalsMeta{BlobCount: 0, TotalBlobBytes: 0},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.ManifestEntryName, Size: int64(len(mData)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(mData)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.RecordsEntryName, Size: int64(len(recBytes)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(recBytes)
		// Extra orphan blob not declared in manifest
		_ = tw.WriteHeader(
			&tar.Header{
				Name:     archive.BlobsPrefix + "0123456789abcdef0123456789abcdef",
				Size:     4,
				Typeflag: tar.TypeReg,
			},
		)
		_, _ = tw.Write([]byte("data"))
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "unlisted orphan blob")
	})

	t.Run("broken closure: manifest blob not referenced by any record", func(t *testing.T) {
		blobID := "0123456789abcdef0123456789abcdef"
		records := []archive.Record{{Key: "k1", Value: []byte("plain-value")}} // No reference to blobID
		recBytes, _ := json.Marshal(records)
		recHash := sha256.Sum256(recBytes)

		manifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      1,
				TotalBytes: int64(len(recBytes)),
				SHA256:     hex.EncodeToString(recHash[:]),
			},
			Totals: archive.TotalsMeta{BlobCount: 1, TotalBlobBytes: 4},
			Blobs: []archive.BlobMeta{
				{ID: blobID, Size: 4, SHA256: strings.Repeat("a", 64)},
			},
		}
		mData, _ := json.Marshal(manifest)

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.ManifestEntryName, Size: int64(len(mData)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(mData)
		_ = tw.WriteHeader(
			&tar.Header{Name: archive.RecordsEntryName, Size: int64(len(recBytes)), Typeflag: tar.TypeReg},
		)
		_, _ = tw.Write(recBytes)
		_ = tw.Close()
		_ = gw.Close()

		assertImportRejection(t, buf.Bytes(), "not referenced by any record")
	})

	t.Run("appended second gzip member", func(t *testing.T) {
		validData := writeValidArchiveBytes(t)

		// Create a second gzip member
		var extraBuf bytes.Buffer
		extraGw := gzip.NewWriter(&extraBuf)
		_, _ = extraGw.Write([]byte("extra compressed junk"))
		_ = extraGw.Close()

		concatenated := append([]byte(nil), validData...)
		concatenated = append(concatenated, extraBuf.Bytes()...)
		assertImportRejection(t, concatenated, "unexpected trailing data")
	})

	t.Run("appended garbage bytes after gzip", func(t *testing.T) {
		validData := writeValidArchiveBytes(t)
		concatenated := append([]byte(nil), validData...)
		concatenated = append(concatenated, []byte("MALICIOUS TRAILING JUNK")...)
		assertImportRejection(t, concatenated, "unexpected trailing data")
	})

	t.Run("truncated gzip trailer", func(t *testing.T) {
		validData := writeValidArchiveBytes(t)
		// Truncate trailer bytes from gzip stream
		truncated := validData[:len(validData)-4]
		assertImportRejection(t, truncated, "")
	})
}

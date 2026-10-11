package archive_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

type safetyTarEntry struct {
	HeaderSize *int64
	Name       string
	Linkname   string
	Data       []byte
	Typeflag   byte
}

func createSafetyControlFixtures(t *testing.T) (archive.Manifest, []archive.Record, []safetyTarEntry) {
	t.Helper()

	const (
		blob1ID = "00000000000000000000000000000001"
		blob2ID = "00000000000000000000000000000002"
	)

	blob1Data := []byte("payload-1")
	blob2Data := []byte("payload-2")
	h1 := sha256.Sum256(blob1Data)
	h2 := sha256.Sum256(blob2Data)

	blob1Meta := archive.BlobMeta{
		ID:     blob1ID,
		Size:   int64(len(blob1Data)),
		SHA256: hex.EncodeToString(h1[:]),
	}
	blob2Meta := archive.BlobMeta{
		ID:     blob2ID,
		Size:   int64(len(blob2Data)),
		SHA256: hex.EncodeToString(h2[:]),
	}

	records := []archive.Record{
		{
			Key:   "k1",
			Value: (storage.Blob{ID: blob1ID, Size: int64(len(blob1Data))}).Record(),
		},
		{
			Key:   "k2",
			Value: (storage.Blob{ID: blob2ID, Size: int64(len(blob2Data))}).Record(),
		},
	}
	recordsBytes, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal records: %v", err)
	}
	recordsHash := sha256.Sum256(recordsBytes)

	manifest := archive.Manifest{
		FormatVersion:    archive.FormatVersion,
		TinyStackVersion: archive.TinyStackVersion,
		Records: archive.RecordsMeta{
			Count:      len(records),
			TotalBytes: int64(len(recordsBytes)),
			SHA256:     hex.EncodeToString(recordsHash[:]),
		},
		Blobs: []archive.BlobMeta{blob1Meta, blob2Meta},
		Totals: archive.TotalsMeta{
			BlobCount:      2,
			TotalBlobBytes: int64(len(blob1Data) + len(blob2Data)),
		},
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	entries := []safetyTarEntry{
		{Name: archive.ManifestEntryName, Data: manifestBytes},
		{Name: archive.RecordsEntryName, Data: recordsBytes},
		{Name: archive.BlobsPrefix + blob1ID, Data: blob1Data},
		{Name: archive.BlobsPrefix + blob2ID, Data: blob2Data},
	}

	return manifest, records, entries
}

func writeSafetyArchive(
	t *testing.T,
	archivePath string,
	entries []safetyTarEntry,
	trailingBytes []byte,
) {
	t.Helper()

	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create test archive file %s: %v", archivePath, err)
	}

	isGzip := strings.HasSuffix(archivePath, ".tar.gz")
	var (
		tw *tar.Writer
		gw *gzip.Writer
	)

	if isGzip {
		gw = gzip.NewWriter(f)
		gw.ModTime = time.Unix(0, 0).UTC()
		gw.OS = 255
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(f)
	}

	for _, entry := range entries {
		hdrSize := int64(len(entry.Data))
		if entry.HeaderSize != nil {
			hdrSize = *entry.HeaderSize
		}

		typeflag := entry.Typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		hdr := &tar.Header{
			Name:     entry.Name,
			Mode:     0o600,
			Size:     hdrSize,
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: typeflag,
			Linkname: entry.Linkname,
			Format:   tar.FormatPAX,
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header for %s: %v", entry.Name, err)
		}

		if len(entry.Data) > 0 {
			if _, err := tw.Write(entry.Data); err != nil {
				t.Fatalf("write tar data for %s: %v", entry.Name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	if gw != nil {
		if err := gw.Close(); err != nil {
			t.Fatalf("close gzip writer: %v", err)
		}
	}

	if len(trailingBytes) > 0 {
		if _, err := f.Write(trailingBytes); err != nil {
			t.Fatalf("write trailing bytes: %v", err)
		}
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close archive file: %v", err)
	}
}

func TestInspectArchive_Safety_Control(t *testing.T) {
	manifest, records, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	for _, tc := range []struct {
		name     string
		fileName string
	}{
		{"compressed_tar_gz", "control.tar.gz"},
		{"uncompressed_tar", "control.tar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archivePath := filepath.Join(tempDir, tc.fileName)
			writeSafetyArchive(t, archivePath, entries, nil)

			inspectedManifest, inspectedRecords, err := archive.InspectArchive(
				context.Background(),
				archivePath,
			)
			if err != nil {
				t.Fatalf("expected valid control archive to pass inspection, got: %v", err)
			}
			if inspectedManifest.FormatVersion != manifest.FormatVersion {
				t.Fatalf(
					"format version mismatch: got %d, want %d",
					inspectedManifest.FormatVersion,
					manifest.FormatVersion,
				)
			}
			if len(inspectedRecords) != len(records) {
				t.Fatalf("records count mismatch: got %d, want %d", len(inspectedRecords), len(records))
			}
			if len(inspectedManifest.Blobs) != len(manifest.Blobs) {
				t.Fatalf("blobs count mismatch: got %d, want %d", len(inspectedManifest.Blobs), len(manifest.Blobs))
			}
		})
	}
}

func TestInspectArchive_Safety_WrongMemberOrder(t *testing.T) {
	_, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	t.Run("records_first_before_manifest", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[1], // records.json
			entries[0], // manifest.json
			entries[2], // blob 1
			entries[3], // blob 2
		}
		archivePath := filepath.Join(tempDir, "records_first.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure when records.json is first entry, got nil")
		}
		if !strings.Contains(err.Error(), "invalid first archive entry: expected manifest.json, got records.json") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("blob_first_before_manifest", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[2], // blob 1
			entries[0], // manifest.json
			entries[1], // records.json
			entries[3], // blob 2
		}
		archivePath := filepath.Join(tempDir, "blob_first.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure when blob is first entry, got nil")
		}
		if !strings.Contains(err.Error(), "invalid first archive entry: expected manifest.json") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("blob_second_before_records", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[0], // manifest.json
			entries[2], // blob 1
			entries[1], // records.json
			entries[3], // blob 2
		}
		archivePath := filepath.Join(tempDir, "blob_second.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure when blob is second entry, got nil")
		}
		if !strings.Contains(err.Error(), "invalid second archive entry: expected records.json") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

func TestInspectArchive_Safety_MemberPaths(t *testing.T) {
	_, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	for _, tc := range []struct {
		name       string
		badPath    string
		entryIndex int
	}{
		{"relative_traversal_manifest", "../manifest.json", 0},
		{"relative_traversal_blob", "blobs/../00000000000000000000000000000001", 2},
		{"absolute_manifest", "/manifest.json", 0},
		{"absolute_records", "/records.json", 1},
		{"noncanonical_dot_manifest", "./manifest.json", 0},
		{"noncanonical_double_slash_blob", "blobs//00000000000000000000000000000001", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := make([]safetyTarEntry, len(entries))
			copy(mutated, entries)
			mutated[tc.entryIndex].Name = tc.badPath

			archivePath := filepath.Join(tempDir, tc.name+".tar.gz")
			writeSafetyArchive(t, archivePath, mutated, nil)

			_, _, err := archive.InspectArchive(context.Background(), archivePath)
			if err == nil {
				t.Fatalf("expected path validation failure for %s, got nil", tc.badPath)
			}
			if !strings.Contains(err.Error(), "not a canonical relative path") {
				t.Fatalf("unexpected error message for %s: %v", tc.badPath, err)
			}
		})
	}
}

func TestInspectArchive_Safety_NonRegularFileTypes(t *testing.T) {
	_, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	for _, tc := range []struct {
		name     string
		linkname string
		typeflag byte
	}{
		{"symlink", "symlink-target", tar.TypeSymlink},
		{"hardlink", "link-target", tar.TypeLink},
		{"directory", "", tar.TypeDir},
		{"char_device", "", tar.TypeChar},
		{"block_device", "", tar.TypeBlock},
		{"fifo", "", tar.TypeFifo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := make([]safetyTarEntry, len(entries))
			copy(mutated, entries)
			mutated[2].Typeflag = tc.typeflag
			mutated[2].Linkname = tc.linkname
			mutated[2].Data = nil

			archivePath := filepath.Join(tempDir, tc.name+".tar.gz")
			writeSafetyArchive(t, archivePath, mutated, nil)

			_, _, err := archive.InspectArchive(context.Background(), archivePath)
			if err == nil {
				t.Fatalf("expected non-regular file failure for type %c, got nil", tc.typeflag)
			}
			if !strings.Contains(err.Error(), "non-regular file type flag") {
				t.Fatalf("unexpected error message for type %c: %v", tc.typeflag, err)
			}
		})
	}
}

func TestInspectArchive_Safety_DuplicateMembers(t *testing.T) {
	_, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	t.Run("duplicate_manifest", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[0], // manifest.json
			entries[0], // duplicate manifest.json
			entries[1], // records.json
			entries[2], // blob 1
			entries[3], // blob 2
		}
		archivePath := filepath.Join(tempDir, "dup_manifest.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on duplicate manifest.json, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate archive entry: manifest.json") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("duplicate_records", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[0], // manifest.json
			entries[1], // records.json
			entries[1], // duplicate records.json
			entries[2], // blob 1
			entries[3], // blob 2
		}
		archivePath := filepath.Join(tempDir, "dup_records.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on duplicate records.json, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate archive entry: records.json") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("duplicate_blob", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[0], // manifest.json
			entries[1], // records.json
			entries[2], // blob 1
			entries[2], // duplicate blob 1
			entries[3], // blob 2
		}
		archivePath := filepath.Join(tempDir, "dup_blob.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on duplicate blob entry, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate archive entry: blobs/00000000000000000000000000000001") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

func TestInspectArchive_Safety_TamperedDigestsAndTruncation(t *testing.T) {
	manifest, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	t.Run("tampered_records_digest", func(t *testing.T) {
		badManifest := manifest
		badManifest.Records.SHA256 = strings.Repeat("a", 64)
		badManifestBytes, err := json.MarshalIndent(badManifest, "", "  ")
		if err != nil {
			t.Fatalf("marshal bad manifest: %v", err)
		}
		badManifestBytes = append(badManifestBytes, '\n')

		mutated := make([]safetyTarEntry, len(entries))
		copy(mutated, entries)
		mutated[0].Data = badManifestBytes

		archivePath := filepath.Join(tempDir, "tampered_records.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err = archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on tampered records digest, got nil")
		}
		if !strings.Contains(err.Error(), "records SHA-256 mismatch") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("tampered_blob_digest", func(t *testing.T) {
		mutated := make([]safetyTarEntry, len(entries))
		copy(mutated, entries)
		corruptedData := append([]byte(nil), mutated[2].Data...)
		corruptedData[0] ^= 0xff
		mutated[2].Data = corruptedData

		archivePath := filepath.Join(tempDir, "tampered_blob.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on tampered blob digest, got nil")
		}
		if !strings.Contains(err.Error(), "SHA-256 mismatch") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("blob_header_size_mismatch", func(t *testing.T) {
		mutated := make([]safetyTarEntry, len(entries))
		copy(mutated, entries)
		wrongHeaderSize := int64(len(mutated[2].Data)) - 2
		mutated[2].HeaderSize = &wrongHeaderSize
		mutated[2].Data = mutated[2].Data[:wrongHeaderSize]

		archivePath := filepath.Join(tempDir, "blob_header_size_mismatch.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on blob header size mismatch, got nil")
		}
		if !strings.Contains(err.Error(), "size mismatch: tar header") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("blob_stream_truncated", func(t *testing.T) {
		const singleBlobID = "00000000000000000000000000000001"
		singleBlobData := []byte("payload-1")
		singleH := sha256.Sum256(singleBlobData)
		singleBlobMeta := archive.BlobMeta{
			ID:     singleBlobID,
			Size:   int64(len(singleBlobData)),
			SHA256: hex.EncodeToString(singleH[:]),
		}
		singleRecs := []archive.Record{
			{
				Key:   "k1",
				Value: (storage.Blob{ID: singleBlobID, Size: int64(len(singleBlobData))}).Record(),
			},
		}
		singleRecsBytes, err := json.Marshal(singleRecs)
		if err != nil {
			t.Fatalf("marshal single recs: %v", err)
		}
		singleRecsH := sha256.Sum256(singleRecsBytes)
		singleManifest := archive.Manifest{
			FormatVersion:    archive.FormatVersion,
			TinyStackVersion: archive.TinyStackVersion,
			Records: archive.RecordsMeta{
				Count:      1,
				TotalBytes: int64(len(singleRecsBytes)),
				SHA256:     hex.EncodeToString(singleRecsH[:]),
			},
			Blobs: []archive.BlobMeta{singleBlobMeta},
			Totals: archive.TotalsMeta{
				BlobCount:      1,
				TotalBlobBytes: int64(len(singleBlobData)),
			},
		}
		singleManifestBytes, err := json.MarshalIndent(singleManifest, "", "  ")
		if err != nil {
			t.Fatalf("marshal single manifest: %v", err)
		}
		singleManifestBytes = append(singleManifestBytes, '\n')

		singleEntries := []safetyTarEntry{
			{Name: archive.ManifestEntryName, Data: singleManifestBytes},
			{Name: archive.RecordsEntryName, Data: singleRecsBytes},
			{Name: archive.BlobsPrefix + singleBlobID, Data: singleBlobData},
		}

		// Use uncompressed tar for deterministic block alignment.
		archivePath := filepath.Join(tempDir, "blob_stream_truncated.tar")
		writeSafetyArchive(t, archivePath, singleEntries, nil)

		// Tar format has two 512-byte zero blocks at EOF (1024 bytes) and pads the 9-byte blob payload
		// to a 512-byte block boundary. Truncate into the blob body payload (4 bytes in) to simulate
		// mid-stream truncation.
		info, statErr := os.Stat(archivePath)
		if statErr != nil {
			t.Fatalf("stat archive: %v", statErr)
		}
		truncatedSize := info.Size() - 1024 - 512 + 4
		if truncErr := os.Truncate(archivePath, truncatedSize); truncErr != nil {
			t.Fatalf("truncate archive: %v", truncErr)
		}

		_, _, err = archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on truncated blob payload stream, got nil")
		}
		if !strings.Contains(err.Error(), "unexpected EOF") && !strings.Contains(err.Error(), "truncated") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

func TestInspectArchive_Safety_UnexpectedOrphanMembers(t *testing.T) {
	_, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	t.Run("orphan_blob_member_in_tar", func(t *testing.T) {
		extraBlobID := "00000000000000000000000000000003"
		extraBlobData := []byte("extra-blob-data")
		mutated := append([]safetyTarEntry(nil), entries...)
		mutated = append(mutated, safetyTarEntry{
			Name: archive.BlobsPrefix + extraBlobID,
			Data: extraBlobData,
		})

		archivePath := filepath.Join(tempDir, "orphan_blob.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on unlisted orphan blob member, got nil")
		}
		if !strings.Contains(err.Error(), "unlisted orphan blob entry in archive") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("unexpected_nonblob_member", func(t *testing.T) {
		mutated := []safetyTarEntry{
			entries[0], // manifest.json
			entries[1], // records.json
			entries[2], // blob 1
			{Name: "unexpected_file.txt", Data: []byte("malicious or extra data")},
		}

		archivePath := filepath.Join(tempDir, "unexpected_member.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on unexpected member, got nil")
		}
		if !strings.Contains(err.Error(), "unexpected unlisted orphan blob") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

func TestInspectArchive_Safety_Ordering(t *testing.T) {
	manifest, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	t.Run("manifest_blobs_order_mutation", func(t *testing.T) {
		// Manifest has blob2 before blob1 (order mutation only)
		badManifest := manifest
		badManifest.Blobs = []archive.BlobMeta{manifest.Blobs[1], manifest.Blobs[0]}
		badManifestBytes, err := json.MarshalIndent(badManifest, "", "  ")
		if err != nil {
			t.Fatalf("marshal bad manifest: %v", err)
		}
		badManifestBytes = append(badManifestBytes, '\n')

		mutated := make([]safetyTarEntry, len(entries))
		copy(mutated, entries)
		mutated[0].Data = badManifestBytes

		archivePath := filepath.Join(tempDir, "manifest_order.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err = archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on unsorted manifest blobs, got nil")
		}
		if !strings.Contains(err.Error(), "manifest blobs not in ascending order") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("tar_blob_members_order_mutation", func(t *testing.T) {
		// Tar entries has blob2 before blob1 (tar member order mutation only)
		mutated := []safetyTarEntry{
			entries[0], // manifest.json
			entries[1], // records.json
			entries[3], // blob 2
			entries[2], // blob 1
		}

		archivePath := filepath.Join(tempDir, "tar_members_order.tar.gz")
		writeSafetyArchive(t, archivePath, mutated, nil)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on unsorted tar blob members, got nil")
		}
		if !strings.Contains(err.Error(), "archive blob entry out of order") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})
}

func TestInspectArchive_Safety_RawTarTrailingData(t *testing.T) {
	_, _, entries := createSafetyControlFixtures(t)
	tempDir := t.TempDir()

	t.Run("uncompressed_tar_trailing_nonzero_bytes", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "trailing_junk.tar")
		trailingJunk := []byte("INVALID_TRAILING_JUNK")
		writeSafetyArchive(t, archivePath, entries, trailingJunk)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err == nil {
			t.Fatal("expected failure on uncompressed tar trailing nonzero bytes, got nil")
		}
		if !strings.Contains(err.Error(), "archive contains non-zero trailing data after tar end marker") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("uncompressed_tar_trailing_valid_zero_padding", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "trailing_zeroes.tar")
		validZeroPadding := make([]byte, 1024)
		writeSafetyArchive(t, archivePath, entries, validZeroPadding)

		_, _, err := archive.InspectArchive(context.Background(), archivePath)
		if err != nil {
			t.Fatalf("expected valid zero padding to succeed, got: %v", err)
		}
	})
}

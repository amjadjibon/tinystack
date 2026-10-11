package archive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// Format version constants.
const (
	FormatVersion    = 1
	MinFormatVersion = 1
	MaxFormatVersion = 1
)

// Archive entry names and prefixes.
const (
	ManifestEntryName = "manifest.json"
	RecordsEntryName  = "records.json"
	BlobsPrefix       = "blobs/"
	TinyStackVersion  = "0.1.0-dev"
)

// Safety bounds and limits for archive metadata and blobs.
const (
	MaxRecords          = 1_000_000
	MaxKeyLength        = 4096
	MaxValueSize        = 64 * 1024 * 1024       // 64 MiB
	DefaultMaxTotalRecs = 1 * 1024 * 1024 * 1024 // 1 GiB
	MaxBlobs            = 1_000_000
	MaxBlobSize         = 5 * 1024 * 1024 * 1024 // 5 GiB
	DefaultMaxManifest  = 32 * 1024 * 1024       // 32 MiB
	StreamBufferSize    = 64 * 1024              // 64 KiB
)

var (
	maxTotalRecordBytes int64 = DefaultMaxTotalRecs
	maxManifestBytes    int64 = DefaultMaxManifest
)

// Manifest defines the top-level contract and integrity metadata stored in manifest.json.
type Manifest struct {
	Blobs            []BlobMeta  `json:"blobs"`
	TinyStackVersion string      `json:"tinystack_version"`
	Records          RecordsMeta `json:"records"`
	Totals           TotalsMeta  `json:"totals"`
	FormatVersion    int         `json:"format_version"`
}

// RecordsMeta describes integrity and count for the records.json payload.
type RecordsMeta struct {
	SHA256     string `json:"sha256"`
	TotalBytes int64  `json:"total_bytes"`
	Count      int    `json:"count"`
}

// BlobMeta describes an individual referenced S3 blob payload.
type BlobMeta struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// TotalsMeta describes aggregate statistics of the archive.
type TotalsMeta struct {
	TotalBlobBytes int64 `json:"total_blob_bytes"`
	BlobCount      int   `json:"blob_count"`
}

// Record aliases storage.Record for binary-safe key/value pair representation.
type Record = storage.Record

// ValidateExportPaths validates the storage directory and output file paths,
// checking for directory existence, parent permissions, and critical hazard overlaps.
func ValidateExportPaths(
	storageDir, outputFile string,
	overwrite bool,
) (absStorageDir, absOutputFile string, err error) {
	if strings.TrimSpace(storageDir) == "" {
		return "", "", errors.New("storage directory is required")
	}

	if strings.TrimSpace(outputFile) == "" {
		return "", "", errors.New("output file is required")
	}

	expandedDir, err := expandHome(storageDir)
	if err != nil {
		return "", "", err
	}

	absDir, err := filepath.Abs(filepath.Clean(expandedDir))
	if err != nil {
		return "", "", fmt.Errorf("resolve storage directory path: %w", err)
	}

	// Storage directory must exist.
	dirStat, err := os.Stat(absDir)
	if err != nil {
		return "", "", fmt.Errorf("storage directory %q does not exist: %w", storageDir, err)
	}

	if !dirStat.IsDir() {
		return "", "", fmt.Errorf("storage directory path %q is not a directory", storageDir)
	}

	dbPath := filepath.Join(absDir, sqlite.File)
	dbStat, err := os.Stat(dbPath)
	if err != nil {
		return "", "", fmt.Errorf("storage database file %q does not exist: %w", dbPath, err)
	}

	if dbStat.IsDir() {
		return "", "", fmt.Errorf("storage database path %q is a directory", dbPath)
	}

	if !dbStat.Mode().IsRegular() {
		return "", "", fmt.Errorf("storage database path %q is not a regular file", dbPath)
	}

	expandedOut, err := expandHome(outputFile)
	if err != nil {
		return "", "", err
	}

	absOut, err := filepath.Abs(filepath.Clean(expandedOut))
	if err != nil {
		return "", "", fmt.Errorf("resolve output file path: %w", err)
	}

	// Resolve symlinks where possible to detect aliased hazard paths.
	evalStorageDir, evalStorageErr := filepath.EvalSymlinks(absDir)
	if evalStorageErr != nil {
		evalStorageDir = absDir
	}

	outDir := filepath.Dir(absOut)
	evalOutDir, evalOutDirErr := filepath.EvalSymlinks(outDir)
	if evalOutDirErr != nil {
		evalOutDir = outDir
	}

	evalOut := filepath.Join(evalOutDir, filepath.Base(absOut))
	if evalOutTarget, evalTargetErr := filepath.EvalSymlinks(absOut); evalTargetErr == nil {
		evalOut = evalOutTarget
	}

	// Hazard 1: Output matches the storage directory itself.
	for _, candidate := range []string{absOut, evalOut} {
		if candidate == absDir || candidate == evalStorageDir {
			return "", "", fmt.Errorf("cannot export: output path matches storage directory (%s)", absOut)
		}
	}

	// Hazard 2: Output matches state.db or SQLite sidecars (state.db-wal, state.db-shm, state.db-journal).
	dbTargets := []string{
		filepath.Join(absDir, sqlite.File),
		filepath.Join(evalStorageDir, sqlite.File),
	}
	if evalDB, evalDBErr := filepath.EvalSymlinks(filepath.Join(absDir, sqlite.File)); evalDBErr == nil {
		dbTargets = append(dbTargets, evalDB)
	}
	for _, dbTarget := range dbTargets {
		for _, candidate := range []string{absOut, evalOut} {
			if candidate == dbTarget {
				return "", "", fmt.Errorf("cannot export: output path overlaps with storage database (%s)", absOut)
			}

			if strings.HasPrefix(candidate, dbTarget+"-") {
				return "", "", fmt.Errorf(
					"cannot export: output path overlaps with storage database sidecar (%s)",
					absOut,
				)
			}
		}
	}

	// Hazard 3: Output matches or resides inside the blobs directory.
	blobsDirs := []string{
		filepath.Join(absDir, "blobs"),
		filepath.Join(evalStorageDir, "blobs"),
	}
	if evalBlobs, evalBlobsErr := filepath.EvalSymlinks(filepath.Join(absDir, "blobs")); evalBlobsErr == nil {
		blobsDirs = append(blobsDirs, evalBlobs)
	}
	for _, bDir := range blobsDirs {
		for _, candidate := range []string{absOut, evalOut} {
			if candidate == bDir {
				return "", "", fmt.Errorf("cannot export: output path matches storage blobs directory (%s)", absOut)
			}

			rel, relErr := filepath.Rel(bDir, candidate)
			if relErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				return "", "", fmt.Errorf(
					"cannot export: output path resides inside storage blobs directory (%s)",
					absOut,
				)
			}
		}
	}

	// Verify destination directory existence.
	outDirStat, err := os.Stat(outDir)
	if err != nil {
		return "", "", fmt.Errorf("output directory %q does not exist: %w", outDir, err)
	}

	if !outDirStat.IsDir() {
		return "", "", fmt.Errorf("output directory path %q is not a directory", outDir)
	}

	// Destination existence check (refuse overwrite by default, including existing symlinks).
	destStat, err := os.Lstat(absOut)
	if err == nil {
		if destStat.IsDir() {
			return "", "", fmt.Errorf("destination path %q is a directory", absOut)
		}

		if !overwrite {
			return "", "", fmt.Errorf("destination file already exists: %s (use --overwrite to replace)", absOut)
		}
	}

	return absDir, absOut, nil
}

func expandHome(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}

		if path == "~" {
			return home, nil
		}

		return filepath.Join(home, path[2:]), nil
	}

	return path, nil
}

// copyBufferContext copies from r to w using buf, checking ctx for cancellation between iterations.
func copyBufferContext(ctx context.Context, w io.Writer, r io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, StreamBufferSize)
	}

	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		nr, rerr := r.Read(buf)
		if nr > 0 {
			nw, werr := w.Write(buf[0:nr])
			written += int64(nw)
			if werr != nil {
				return written, werr
			}

			if nr != nw {
				return written, io.ErrShortWrite
			}
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return written, nil
			}

			return written, rerr
		}
	}
}

type archiveReader struct {
	tr   *tar.Reader
	gz   *gzip.Reader
	br   *bufio.Reader
	file *os.File
}

func (ar *archiveReader) Close() error {
	var err error
	if ar.gz != nil {
		err = errors.Join(err, ar.gz.Close())
	}
	if ar.file != nil {
		err = errors.Join(err, ar.file.Close())
	}
	return err
}

// openArchiveReader opens an archive file, automatically detecting gzip compression.
func openArchiveReader(archivePath string) (*archiveReader, error) {
	// #nosec G304 -- explicit file path validated by caller.
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}

	br := bufio.NewReader(f)
	header, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		return nil, fmt.Errorf("read archive header: %w", err)
	}

	ar := &archiveReader{
		file: f,
		br:   br,
	}

	if len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		gz, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("open gzip reader: %w", gzErr)
		}
		// Configure Multistream(false) immediately before any reads so only the
		// single archive gzip member is consumed and any trailing compressed members
		// or trailing bytes remain in br to be verified.
		gz.Multistream(false)
		ar.gz = gz
		ar.tr = tar.NewReader(gz)
	} else {
		ar.tr = tar.NewReader(br)
	}

	return ar, nil
}

// ValidateArchive inspects and validates an archive without extracting or mutating state.
// It verifies format version, member ordering, canonical names, SHA-256 hashes, and bounds.
func ValidateArchive(ctx context.Context, archivePath string) (*Manifest, error) {
	manifest, _, err := InspectArchive(ctx, archivePath)
	return manifest, err
}

// InspectArchive reads and thoroughly validates an archive, returning the manifest and records.
func InspectArchive(ctx context.Context, archivePath string) (*Manifest, []Record, error) {
	ar, err := openArchiveReader(archivePath)
	if err != nil {
		return nil, nil, fmt.Errorf("open archive %s: %w", archivePath, err)
	}

	defer func() { _ = ar.Close() }()

	var (
		manifest    *Manifest
		records     []Record
		seenBlobs   = make(map[string]bool)
		blobMetaMap = make(map[string]BlobMeta)
		entryIndex  = 0
		seenEntries = make(map[string]bool)
	)

	buf := make([]byte, StreamBufferSize)

	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		hdr, tarErr := ar.tr.Next()
		if errors.Is(tarErr, io.EOF) {
			break
		}

		if tarErr != nil {
			return nil, nil, fmt.Errorf("read archive entry: %w", tarErr)
		}

		if seenEntries[hdr.Name] {
			return nil, nil, fmt.Errorf("duplicate archive entry: %s", hdr.Name)
		}

		seenEntries[hdr.Name] = true

		// Strictly disallow symlinks, hardlinks, directories, or special devices.
		if hdr.Typeflag != tar.TypeReg {
			return nil, nil, fmt.Errorf(
				"invalid archive entry %s: non-regular file type flag %c",
				hdr.Name,
				hdr.Typeflag,
			)
		}

		// Ensure relative canonical path without directory traversal.
		cleanName := filepath.Clean(hdr.Name)
		if filepath.IsAbs(hdr.Name) || cleanName != hdr.Name || strings.HasPrefix(cleanName, "..") {
			return nil, nil, fmt.Errorf("invalid archive entry name %q: not a canonical relative path", hdr.Name)
		}

		switch entryIndex {
		case 0:
			// First entry must be manifest.json.
			if hdr.Name != ManifestEntryName {
				return nil, nil, fmt.Errorf(
					"invalid first archive entry: expected %s, got %s",
					ManifestEntryName,
					hdr.Name,
				)
			}

			if hdr.Size > maxManifestBytes {
				return nil, nil, fmt.Errorf("manifest size %d exceeds limit (%d)", hdr.Size, maxManifestBytes)
			}

			manifestData, readErr := io.ReadAll(io.LimitReader(ar.tr, maxManifestBytes+1))
			if readErr != nil {
				return nil, nil, fmt.Errorf("read manifest: %w", readErr)
			}

			if int64(len(manifestData)) != hdr.Size {
				return nil, nil, fmt.Errorf("manifest size mismatch: read %d, expected %d", len(manifestData), hdr.Size)
			}

			manifest = new(Manifest)
			if unmarshalErr := json.Unmarshal(manifestData, manifest); unmarshalErr != nil {
				return nil, nil, fmt.Errorf("decode manifest: %w", unmarshalErr)
			}

			if manifest.FormatVersion < MinFormatVersion || manifest.FormatVersion > MaxFormatVersion {
				return nil, nil, fmt.Errorf("unsupported format version %d (supported: %d..%d)",
					manifest.FormatVersion, MinFormatVersion, MaxFormatVersion)
			}

			if manifest.Records.Count < 0 || manifest.Records.TotalBytes < 0 ||
				manifest.Totals.BlobCount < 0 || manifest.Totals.TotalBlobBytes < 0 {
				return nil, nil, errors.New("manifest contains negative counts or sizes")
			}

			if err := validateSHA256Hex(manifest.Records.SHA256); err != nil {
				return nil, nil, fmt.Errorf("manifest records SHA-256 is invalid: %w", err)
			}

			if manifest.Records.Count > MaxRecords {
				return nil, nil, fmt.Errorf("manifest record count %d exceeds limit (%d)",
					manifest.Records.Count, MaxRecords)
			}

			if manifest.Totals.BlobCount > MaxBlobs || len(manifest.Blobs) > MaxBlobs {
				return nil, nil, fmt.Errorf("manifest blob count %d exceeds limit (%d)",
					len(manifest.Blobs), MaxBlobs)
			}

			if len(manifest.Blobs) != manifest.Totals.BlobCount {
				return nil, nil, fmt.Errorf("manifest blob list count %d does not match total %d",
					len(manifest.Blobs), manifest.Totals.BlobCount)
			}

			var (
				prevBlobID   string
				sumBlobBytes int64
			)
			for _, bm := range manifest.Blobs {
				if valErr := storage.ValidateBlobID(bm.ID); valErr != nil {
					return nil, nil, fmt.Errorf("manifest contains invalid blob ID %q: %w", bm.ID, valErr)
				}

				if err := validateSHA256Hex(bm.SHA256); err != nil {
					return nil, nil, fmt.Errorf("manifest blob %s SHA-256 is invalid: %w", bm.ID, err)
				}

				if bm.Size < 0 || bm.Size > MaxBlobSize {
					return nil, nil, fmt.Errorf("manifest blob %s size %d is invalid or exceeds limit", bm.ID, bm.Size)
				}

				// Enforce strictly ascending order without duplicates.
				if prevBlobID != "" && bm.ID <= prevBlobID {
					if bm.ID == prevBlobID {
						return nil, nil, fmt.Errorf("manifest contains duplicate blob ID %s", bm.ID)
					}

					return nil, nil, fmt.Errorf("manifest blobs not in ascending order: %s after %s", bm.ID, prevBlobID)
				}
				prevBlobID = bm.ID

				sumBlobBytes += bm.Size
				blobMetaMap[bm.ID] = bm
			}

			if sumBlobBytes != manifest.Totals.TotalBlobBytes {
				return nil, nil, fmt.Errorf("manifest total_blob_bytes mismatch: sum is %d, declared %d",
					sumBlobBytes, manifest.Totals.TotalBlobBytes)
			}

		case 1:
			// Second entry must be records.json.
			if hdr.Name != RecordsEntryName {
				return nil, nil, fmt.Errorf(
					"invalid second archive entry: expected %s, got %s",
					RecordsEntryName,
					hdr.Name,
				)
			}

			if manifest == nil {
				return nil, nil, errors.New("manifest missing before records entry")
			}

			if hdr.Size != manifest.Records.TotalBytes {
				return nil, nil, fmt.Errorf("records entry size %d does not match manifest total bytes %d",
					hdr.Size, manifest.Records.TotalBytes)
			}

			if hdr.Size > maxTotalRecordBytes {
				return nil, nil, fmt.Errorf("records payload size %d exceeds limit (%d)", hdr.Size, maxTotalRecordBytes)
			}

			recordsHasher := sha256.New()
			var recordsBuf bytes.Buffer

			tee := io.TeeReader(ar.tr, recordsHasher)
			n, copyErr := copyBufferContext(ctx, &recordsBuf, io.LimitReader(tee, maxTotalRecordBytes+1), buf)
			if copyErr != nil {
				return nil, nil, fmt.Errorf("read records entry: %w", copyErr)
			}

			if n != hdr.Size {
				return nil, nil, fmt.Errorf("records entry truncated: read %d, expected %d", n, hdr.Size)
			}

			actualSHA256 := hex.EncodeToString(recordsHasher.Sum(nil))
			if actualSHA256 != manifest.Records.SHA256 {
				return nil, nil, fmt.Errorf("records SHA-256 mismatch: expected %s, got %s",
					manifest.Records.SHA256, actualSHA256)
			}

			if unmarshalErr := json.Unmarshal(recordsBuf.Bytes(), &records); unmarshalErr != nil {
				return nil, nil, fmt.Errorf("decode records JSON: %w", unmarshalErr)
			}

			if len(records) != manifest.Records.Count {
				return nil, nil, fmt.Errorf("record count mismatch: decoded %d, expected %d",
					len(records), manifest.Records.Count)
			}

			// Validate record keys are strictly sorted by exact decoded bytes, and unique.
			var prevKey []byte
			for i, r := range records {
				if len(r.Key) > MaxKeyLength {
					return nil, nil, fmt.Errorf("record %d key exceeds maximum length (%d bytes)", i, MaxKeyLength)
				}

				if len(r.Value) > MaxValueSize {
					return nil, nil, fmt.Errorf("record %q value exceeds maximum size (%d bytes)", r.Key, MaxValueSize)
				}

				keyBytes := []byte(r.Key)
				if i > 0 {
					cmp := bytes.Compare(keyBytes, prevKey)
					if cmp == 0 {
						return nil, nil, fmt.Errorf("duplicate record key in records payload: %q", r.Key)
					}

					if cmp < 0 {
						return nil, nil, fmt.Errorf(
							"unsorted record keys in payload: %q after %q",
							r.Key,
							string(prevKey),
						)
					}
				}

				prevKey = keyBytes
			}

			// Reconcile blob references from records with manifest blobs.
			referencedBlobs := make(map[string]int64)
			for _, r := range records {
				blob, isBlob := storage.BlobReference(r.Value)
				if !isBlob {
					continue
				}

				if valErr := storage.ValidateBlobID(blob.ID); valErr != nil {
					return nil, nil, fmt.Errorf("record %q contains invalid blob reference: %w", r.Key, valErr)
				}

				if blob.Size < 0 || blob.Size > MaxBlobSize {
					return nil, nil, fmt.Errorf(
						"record %q blob reference size %d is invalid or exceeds limit",
						r.Key,
						blob.Size,
					)
				}

				if existingSize, seen := referencedBlobs[blob.ID]; seen {
					if existingSize != blob.Size {
						return nil, nil, fmt.Errorf("conflicting sizes for blob %s across records: %d vs %d",
							blob.ID, existingSize, blob.Size)
					}
				} else {
					referencedBlobs[blob.ID] = blob.Size
				}

				bm, exists := blobMetaMap[blob.ID]
				if !exists {
					return nil, nil, fmt.Errorf(
						"record %q references blob %s which is missing from manifest",
						r.Key,
						blob.ID,
					)
				}

				if bm.Size != blob.Size {
					return nil, nil, fmt.Errorf(
						"record %q references blob %s size %d but manifest declares %d",
						r.Key,
						blob.ID,
						blob.Size,
						bm.Size,
					)
				}
			}

			// Exact closure: manifest must not contain blobs not referenced in records.
			if len(manifest.Blobs) != len(referencedBlobs) {
				for _, bm := range manifest.Blobs {
					if _, referenced := referencedBlobs[bm.ID]; !referenced {
						return nil, nil, fmt.Errorf("manifest contains blob %s not referenced by any record", bm.ID)
					}
				}
			}

		default:
			// Subsequent entries must be blobs/<blob_id> in strictly ascending order matching manifest.Blobs.
			blobIdx := entryIndex - 2
			if blobIdx >= len(manifest.Blobs) {
				return nil, nil, fmt.Errorf("unlisted orphan blob entry in archive: %s", hdr.Name)
			}

			expectedBlob := manifest.Blobs[blobIdx]
			expectedName := BlobsPrefix + expectedBlob.ID
			if hdr.Name != expectedName {
				return nil, nil, fmt.Errorf(
					"archive blob entry out of order or unexpected unlisted orphan blob: got %s, expected %s",
					hdr.Name,
					expectedName,
				)
			}

			blobID := expectedBlob.ID
			bm := expectedBlob

			if hdr.Size != bm.Size {
				return nil, nil, fmt.Errorf("blob entry %s size mismatch: tar header %d, manifest %d",
					blobID, hdr.Size, bm.Size)
			}

			blobHasher := sha256.New()
			copied, copyErr := copyBufferContext(ctx, blobHasher, io.LimitReader(ar.tr, MaxBlobSize+1), buf)
			if copyErr != nil {
				return nil, nil, fmt.Errorf("stream blob %s: %w", blobID, copyErr)
			}

			if copied != bm.Size {
				return nil, nil, fmt.Errorf("blob %s truncated: read %d, expected %d", blobID, copied, bm.Size)
			}

			actualBlobSHA := hex.EncodeToString(blobHasher.Sum(nil))
			if actualBlobSHA != bm.SHA256 {
				return nil, nil, fmt.Errorf("blob %s SHA-256 mismatch: expected %s, got %s",
					blobID, bm.SHA256, actualBlobSHA)
			}

			seenBlobs[blobID] = true
		}

		entryIndex++
	}

	if manifest == nil {
		return nil, nil, errors.New("archive is empty or missing manifest")
	}

	if entryIndex < 2 {
		return nil, nil, errors.New("archive is missing records entry")
	}

	if len(seenBlobs) != len(manifest.Blobs) {
		return nil, nil, fmt.Errorf("missing blob entries in archive: read %d, expected %d",
			len(seenBlobs), len(manifest.Blobs))
	}

	// Verify complete stream integrity / trailing bytes (MED-002).
	const maxTrailingTarPadding = 1024 * 1024
	if ar.gz != nil {
		var trailingDecompressed int64
		drainBuf := make([]byte, StreamBufferSize)
		for {
			n, readErr := ar.gz.Read(drainBuf)
			if n > 0 {
				trailingDecompressed += int64(n)
				if trailingDecompressed > maxTrailingTarPadding {
					return nil, nil, fmt.Errorf(
						"archive contains excessive trailing data (>%d bytes)",
						maxTrailingTarPadding,
					)
				}

				for _, b := range drainBuf[:n] {
					if b != 0 {
						return nil, nil, errors.New("archive contains non-zero trailing data after tar end marker")
					}
				}
			}

			if errors.Is(readErr, io.EOF) {
				break
			}

			if readErr != nil {
				return nil, nil, fmt.Errorf("gzip stream integrity error: %w", readErr)
			}
		}

		// Ensure no unexpected bytes after the gzip stream in the underlying file.
		if ar.br.Buffered() > 0 {
			return nil, nil, errors.New("archive contains unexpected trailing data after gzip stream")
		}

		var extra [1]byte
		n, extraErr := ar.br.Read(extra[:])
		if n > 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) {
			return nil, nil, errors.New("archive contains unexpected trailing data after gzip stream")
		}
	} else {
		// For uncompressed tar, ensure remaining bytes are zero padding only.
		var trailingTar int64
		drainBuf := make([]byte, StreamBufferSize)
		for {
			n, readErr := ar.br.Read(drainBuf)
			if n > 0 {
				trailingTar += int64(n)
				if trailingTar > maxTrailingTarPadding {
					return nil, nil, fmt.Errorf(
						"archive contains excessive trailing data (>%d bytes)",
						maxTrailingTarPadding,
					)
				}

				for _, b := range drainBuf[:n] {
					if b != 0 {
						return nil, nil, errors.New("archive contains non-zero trailing data after tar end marker")
					}
				}
			}

			if errors.Is(readErr, io.EOF) {
				break
			}

			if readErr != nil {
				return nil, nil, fmt.Errorf("read uncompressed archive trailing data: %w", readErr)
			}
		}
	}

	return manifest, records, nil
}

func validateSHA256Hex(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("expected 64 hex characters, got length %d", len(s))
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("contains non-hex or uppercase character %q", c)
		}
	}

	return nil
}

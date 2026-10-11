package archive

import (
	"archive/tar"
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
	"sort"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// ExportOptions specifies configuration for an offline persistent state export.
type ExportOptions struct {
	StorageDir string
	OutputFile string
	Overwrite  bool
}

// exportTempFile encapsulates the minimal file operations required for the temporary export artifact.
type exportTempFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

// Production temp file factory using os.CreateTemp.
var createTempFile = func(dir, pattern string) (exportTempFile, error) {
	return os.CreateTemp(dir, pattern)
}

// Internal test hook used exclusively by publication race tests to simulate a concurrent destination creation.
var testBeforePublishHook func(tmpPath string) error

// Export dumps the offline SQLite database records and referenced S3 blobs into a portable archive.
// It acquires an exclusive SQLite connection lock, enforces format integrity and bounds, streams
// blob payloads using bounded buffers, and publishes the archive atomically via a temporary file.
//
//nolint:contextcheck // store.Close does not accept a context parameter.
func Export(ctx context.Context, opts ExportOptions) error {
	absStorageDir, absOutputFile, err := ValidateExportPaths(opts.StorageDir, opts.OutputFile, opts.Overwrite)
	if err != nil {
		return err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// Open the SQLite database to acquire exclusive lock and read records snapshot.
	store, err := sqlite.Open(ctx, absStorageDir)
	if err != nil {
		return fmt.Errorf("open storage database: %w", err)
	}

	defer func() { _ = store.Close() }()

	var (
		records         []Record
		referencedBlobs = make(map[string]int64) // blobID -> size
	)

	err = store.View(ctx, func(tx storage.ReadTx) error {
		keys := tx.Keys("")
		if len(keys) > MaxRecords {
			return fmt.Errorf("record count %d exceeds maximum limit (%d)", len(keys), MaxRecords)
		}

		records = make([]Record, 0, len(keys))
		for _, k := range keys {
			if len(k) > MaxKeyLength {
				return fmt.Errorf("record key exceeds maximum length (%d bytes)", MaxKeyLength)
			}

			val, ok := tx.Get(k)
			if !ok {
				continue
			}

			if len(val) > MaxValueSize {
				return fmt.Errorf("record %q value size %d exceeds maximum limit (%d bytes)", k, len(val), MaxValueSize)
			}

			records = append(records, Record{Key: k, Value: val})

			if blob, isBlob := storage.BlobReference(val); isBlob {
				if valErr := storage.ValidateBlobID(blob.ID); valErr != nil {
					return fmt.Errorf("record %q contains invalid blob reference: %w", k, valErr)
				}

				if blob.Size < 0 || blob.Size > MaxBlobSize {
					return fmt.Errorf("record %q blob reference size %d is invalid or exceeds limit (%d bytes)",
						k, blob.Size, MaxBlobSize)
				}

				if existingSize, seen := referencedBlobs[blob.ID]; seen {
					if existingSize != blob.Size {
						return fmt.Errorf("conflicting sizes recorded for blob %s: %d vs %d",
							blob.ID, existingSize, blob.Size)
					}
				} else {
					referencedBlobs[blob.ID] = blob.Size
				}
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("read state records: %w", err)
	}

	// Deterministic JSON serialization of records with lossless base64 key encoding.
	recordsBytes, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshal records payload: %w", err)
	}

	if int64(len(recordsBytes)) > maxTotalRecordBytes {
		return fmt.Errorf("records payload size %d exceeds maximum limit (%d bytes)",
			len(recordsBytes), maxTotalRecordBytes)
	}

	recordsSHA256 := sha256.Sum256(recordsBytes)
	recordsSHA256Hex := hex.EncodeToString(recordsSHA256[:])

	// Validate and hash referenced blobs.
	blobsDir := filepath.Join(absStorageDir, "blobs")
	if len(referencedBlobs) > 0 {
		bStat, bErr := os.Stat(blobsDir)
		if bErr != nil {
			return fmt.Errorf("referenced blobs directory %q does not exist: %w", blobsDir, bErr)
		}

		if !bStat.IsDir() {
			return fmt.Errorf("referenced blobs path %q is not a directory", blobsDir)
		}
	}

	blobIDs := make([]string, 0, len(referencedBlobs))
	for id := range referencedBlobs {
		blobIDs = append(blobIDs, id)
	}

	sort.Strings(blobIDs)

	blobMetas := make([]BlobMeta, 0, len(blobIDs))
	var totalBlobBytes int64

	for _, id := range blobIDs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		expectedSize := referencedBlobs[id]
		blobPath := filepath.Join(blobsDir, id)

		fi, statErr := os.Lstat(blobPath)
		if statErr != nil {
			return fmt.Errorf("referenced blob %s not found: %w", id, statErr)
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("referenced blob %s is a symlink: symlink blob files are not permitted", id)
		}

		if !fi.Mode().IsRegular() {
			return fmt.Errorf("referenced blob %s is not a regular file (mode: %s)", id, fi.Mode())
		}

		if fi.Size() != expectedSize {
			return fmt.Errorf("referenced blob %s size mismatch: expected %d bytes, got %d bytes",
				id, expectedSize, fi.Size())
		}

		blobHash, hashErr := hashBlobFile(ctx, blobPath, expectedSize)
		if hashErr != nil {
			return fmt.Errorf("hash blob %s: %w", id, hashErr)
		}

		blobMetas = append(blobMetas, BlobMeta{
			ID:     id,
			SHA256: blobHash,
			Size:   expectedSize,
		})
		totalBlobBytes += expectedSize
	}

	// Build manifest.
	manifest := Manifest{
		Blobs:            blobMetas,
		TinyStackVersion: TinyStackVersion,
		Records: RecordsMeta{
			SHA256:     recordsSHA256Hex,
			TotalBytes: int64(len(recordsBytes)),
			Count:      len(records),
		},
		Totals: TotalsMeta{
			TotalBlobBytes: totalBlobBytes,
			BlobCount:      len(blobMetas),
		},
		FormatVersion: FormatVersion,
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	manifestBytes = append(manifestBytes, '\n')

	if int64(len(manifestBytes)) > maxManifestBytes {
		return fmt.Errorf("manifest size %d exceeds maximum limit (%d bytes)",
			len(manifestBytes), maxManifestBytes)
	}

	// Prepare atomic publication via temporary file in the destination's directory.
	outDir := filepath.Dir(absOutputFile)
	tmpFile, err := createTempFile(outDir, ".tinystack-export-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary export file: %w", err)
	}

	// Ensure private file permissions.
	_ = os.Chmod(tmpFile.Name(), 0o600)

	var succeeded bool
	defer func() {
		if !succeeded {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
		}
	}()

	isGzip := !strings.HasSuffix(absOutputFile, ".tar")
	var (
		tw *tar.Writer
		gw *gzip.Writer
	)

	if isGzip {
		gw = gzip.NewWriter(tmpFile)
		gw.ModTime = time.Unix(0, 0)
		gw.OS = 255
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(tmpFile)
	}

	// Entry 1: manifest.json
	manifestHdr := &tar.Header{
		Name:     ManifestEntryName,
		Mode:     0o600,
		Size:     int64(len(manifestBytes)),
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}
	if writeHdrErr := tw.WriteHeader(manifestHdr); writeHdrErr != nil {
		return fmt.Errorf("write manifest header: %w", writeHdrErr)
	}

	if _, writeErr := tw.Write(manifestBytes); writeErr != nil {
		return fmt.Errorf("write manifest content: %w", writeErr)
	}

	// Entry 2: records.json
	recordsHdr := &tar.Header{
		Name:     RecordsEntryName,
		Mode:     0o600,
		Size:     int64(len(recordsBytes)),
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}
	if writeHdrErr := tw.WriteHeader(recordsHdr); writeHdrErr != nil {
		return fmt.Errorf("write records header: %w", writeHdrErr)
	}

	if _, writeErr := tw.Write(recordsBytes); writeErr != nil {
		return fmt.Errorf("write records content: %w", writeErr)
	}

	// Subsequent entries: blobs/<id> in sorted order
	copyBuf := make([]byte, StreamBufferSize)
	for _, bm := range blobMetas {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		blobHdr := &tar.Header{
			Name:     BlobsPrefix + bm.ID,
			Mode:     0o600,
			Size:     bm.Size,
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if writeHdrErr := tw.WriteHeader(blobHdr); writeHdrErr != nil {
			return fmt.Errorf("write blob %s header: %w", bm.ID, writeHdrErr)
		}

		blobPath := filepath.Join(blobsDir, bm.ID)
		f, openErr := openRegularBlobFile(blobPath, bm.Size)
		if openErr != nil {
			return fmt.Errorf("open blob %s for streaming: %w", bm.ID, openErr)
		}

		copied, copyErr := copyBufferContext(ctx, tw, f, copyBuf)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("stream blob %s: %w", bm.ID, copyErr)
		}

		if closeErr != nil {
			return fmt.Errorf("close blob %s: %w", bm.ID, closeErr)
		}

		if copied != bm.Size {
			return fmt.Errorf("blob %s size mismatch during copy: copied %d, expected %d",
				bm.ID, copied, bm.Size)
		}
	}

	// Flush and close archive layers.
	if closeErr := tw.Close(); closeErr != nil {
		return fmt.Errorf("close tar writer: %w", closeErr)
	}

	if gw != nil {
		if closeErr := gw.Close(); closeErr != nil {
			return fmt.Errorf("close gzip writer: %w", closeErr)
		}
	}

	if syncErr := tmpFile.Sync(); syncErr != nil {
		return fmt.Errorf("sync temporary export file: %w", syncErr)
	}

	tmpPath := tmpFile.Name()
	if closeErr := tmpFile.Close(); closeErr != nil {
		return fmt.Errorf("close temporary export file: %w", closeErr)
	}

	// Check cancellation before publication.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// Atomic publication to destination.
	if pubErr := publishArchive(tmpPath, absOutputFile, opts.Overwrite); pubErr != nil {
		return pubErr
	}

	succeeded = true
	return nil
}

func publishArchive(tmpPath, destPath string, overwrite bool) error {
	if testBeforePublishHook != nil {
		if err := testBeforePublishHook(tmpPath); err != nil {
			return err
		}
	}

	if overwrite {
		if err := os.Rename(tmpPath, destPath); err != nil {
			return fmt.Errorf("publish export archive: %w", err)
		}
		return nil
	}

	// Default: atomic no-replace publication using os.Link.
	if err := os.Link(tmpPath, destPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("destination file %q already exists (use --overwrite to replace)", destPath)
		}
		// Fallback for filesystems that do not support hardlinks: check destination with Lstat then rename.
		if _, statErr := os.Lstat(destPath); statErr == nil {
			return fmt.Errorf("destination file %q already exists (use --overwrite to replace)", destPath)
		}
		if renErr := os.Rename(tmpPath, destPath); renErr != nil {
			return fmt.Errorf("publish export archive: %w", renErr)
		}
		return nil
	}

	_ = os.Remove(tmpPath)
	return nil
}

func hashBlobFile(ctx context.Context, path string, expectedSize int64) (string, error) {
	f, err := openRegularBlobFile(path, expectedSize)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	buf := make([]byte, StreamBufferSize)
	copied, err := copyBufferContext(ctx, h, f, buf)
	if err != nil {
		return "", err
	}

	if copied != expectedSize {
		return "", fmt.Errorf("blob file size mismatch during hashing: read %d, expected %d", copied, expectedSize)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// openRegularBlobFile verifies that the target path is a non-symlink regular file matching expected size
// before opening, and verifies that the opened file descriptor matches the lstated file to prevent race attacks.
func openRegularBlobFile(path string, expectedSize int64) (*os.File, error) {
	fi, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, statErr
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symlink blob files are not permitted: %s", path)
	}

	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (mode: %s): %s", fi.Mode(), path)
	}

	if fi.Size() != expectedSize {
		return nil, fmt.Errorf("size mismatch: expected %d bytes, got %d bytes", expectedSize, fi.Size())
	}

	// #nosec G304 -- path is validated as a regular non-symlink file within the storage blobs directory.
	f, openErr := os.Open(filepath.Clean(path))
	if openErr != nil {
		return nil, openErr
	}

	fStat, fStatErr := f.Stat()
	if fStatErr != nil {
		_ = f.Close()
		return nil, fStatErr
	}

	if !os.SameFile(fi, fStat) {
		_ = f.Close()
		return nil, fmt.Errorf("file identity changed after open: %s", path)
	}

	return f, nil
}

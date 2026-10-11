package archive_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func TestParentImportRejectsMalformedRecordWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
	}{
		{"missing key", `[{"value":"dg=="}]`},
		{"missing value", `[{"key":"aw=="}]`},
		{"array encoded bytes", `[{"key":[107],"value":[118]}]`},
		{"contradictory duplicate key fields", `[{"key":"YWJj","key":"aw==","value":"dg=="}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordData := []byte(tc.wire)
			h := sha256.Sum256(recordData)
			m := archive.Manifest{
				FormatVersion:    archive.FormatVersion,
				TinyStackVersion: archive.TinyStackVersion,
				Records: archive.RecordsMeta{
					Count:      1,
					TotalBytes: int64(len(recordData)),
					SHA256:     hex.EncodeToString(h[:]),
				},
			}
			manifestData, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "malformed.tar")
			writeSafetyArchive(t, input, []safetyTarEntry{
				{Name: "manifest.json", Data: manifestData},
				{Name: "records.json", Data: recordData},
			}, nil)
			parent := t.TempDir()
			dest := filepath.Join(parent, "restored")
			importErr := archive.Import(context.Background(), archive.ImportOptions{
				InputFile:  input,
				StorageDir: dest,
			})
			if importErr == nil {
				t.Fatal("malformed record wire was accepted and published")
			}
			if _, statErr := os.Lstat(dest); !os.IsNotExist(statErr) {
				t.Fatalf("malformed archive changed destination: %v", statErr)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".tinystack-import-") {
					t.Fatalf("malformed archive leaked staging directory: %s", entry.Name())
				}
			}
		})
	}
}

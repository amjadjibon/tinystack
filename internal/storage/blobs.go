package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Blob is an immutable file reference.
type Blob struct {
	ID   string
	Size int64
}

const blobMarker = "tinystack:blob:1:"

// BlobReference decodes an opaque file reference.
func BlobReference(data []byte) (Blob, bool) {
	var b Blob
	if !bytes.HasPrefix(data, []byte(blobMarker)) {
		return b, false
	}

	if json.Unmarshal(data[len(blobMarker):], &b) != nil || len(b.ID) != 32 || b.Size < 0 {
		return Blob{}, false
	}

	if _, err := hex.DecodeString(b.ID); err != nil {
		return Blob{}, false
	}

	return b, true
}

func (b Blob) Record() []byte {
	data, _ := json.Marshal(b)
	return append([]byte(blobMarker), data...)
}

// BlobStore owns immutable files independently of any record store. Owners publish
// references in their own transactions and supply live IDs for garbage collection.
// Pins protect files between reading a reference and opening it, and pending writes.
type BlobStore struct {
	pins      map[string]int
	retired   map[string]bool
	dir       string
	fileMu    sync.Mutex
	writeMu   sync.RWMutex
	temporary bool
}

// OpenBlobs opens the file facility without inspecting or changing records.
// An empty directory selects ephemeral storage, removed by Close.
func OpenBlobs(dir string) (*BlobStore, error) {
	temporary := dir == ""
	var err error
	if temporary {
		dir, err = os.MkdirTemp("", "tinystack-blobs-")
	} else {
		dir = filepath.Join(dir, "blobs")
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return nil, err
	}
	return &BlobStore{
		dir: dir, temporary: temporary,
		pins: map[string]int{}, retired: map[string]bool{},
	}, nil
}

// Write publishes a durable immutable file before returning a pinned reference.
// The caller releases the pin after committing the reference (or abandoning it).
func (s *BlobStore) Write(r io.Reader) (Blob, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	f, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return Blob{}, err
	}

	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	buf := make([]byte, 32*1024)
	size, err := io.CopyBuffer(f, r, buf)
	if err != nil {
		return Blob{}, err
	}

	if syncErr := f.Sync(); syncErr != nil {
		return Blob{}, syncErr
	}

	if closeErr := f.Close(); closeErr != nil {
		return Blob{}, closeErr
	}

	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return Blob{}, err
	}

	b := Blob{ID: hex.EncodeToString(id[:]), Size: size}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if renameErr := os.Rename(f.Name(), filepath.Join(s.dir, b.ID)); renameErr != nil {
		return Blob{}, renameErr
	}
	// Sync the directory so a committed reference survives a power loss as well
	// as a process crash. The directory is owned by this store.
	d, err := os.Open(s.dir) // #nosec G304 -- configured storage directory.
	if err != nil {
		return Blob{}, err
	}

	err = errors.Join(d.Sync(), d.Close())
	if err != nil {
		return Blob{}, err
	}

	s.pins[b.ID]++
	return b, nil
}

// Pin is safe inside a record callback: it performs no external I/O.
func (s *BlobStore) Pin(b Blob) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	s.pins[b.ID]++
}

func (s *BlobStore) Release(b Blob) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	s.pins[b.ID]--
	if s.pins[b.ID] == 0 {
		delete(s.pins, b.ID)
		if s.retired[b.ID] {
			if err := os.Remove(filepath.Join(s.dir, b.ID)); err == nil || errors.Is(err, os.ErrNotExist) {
				delete(s.retired, b.ID)
			}
		}
	}
}

// Discard unpins and immediately removes an unpublished file.
func (s *BlobStore) Discard(b Blob) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	s.pins[b.ID]--
	if s.pins[b.ID] <= 0 {
		delete(s.pins, b.ID)
		delete(s.retired, b.ID)
		_ = os.Remove(filepath.Join(s.dir, b.ID))
	}
}

// Open opens a pinned reference. The caller owns the file and releases the pin.
func (s *BlobStore) Open(b Blob) (*os.File, error) {
	return os.Open(filepath.Join(s.dir, b.ID)) // #nosec G304 -- validated opaque blob ID.
}

// Retire reconciles committed reference changes: live IDs cancel pending retirement,
// and candidate IDs absent from live are removed after their last pin is released.
// Owners must order this call with reference publication. Readers pin while their
// record transaction still prevents removal of the reference.
func (s *BlobStore) Retire(candidates, live map[string]bool) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	for id := range live {
		delete(s.retired, id)
	}
	for id := range candidates {
		if live[id] {
			continue
		}
		s.retired[id] = true
		if s.pins[id] == 0 {
			if err := os.Remove(filepath.Join(s.dir, id)); err == nil || errors.Is(err, os.ErrNotExist) {
				delete(s.retired, id)
			}
		}
	}
}

func (s *BlobStore) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if s.temporary {
		return os.RemoveAll(s.dir)
	}

	return nil
}

// Collect removes files absent from the owner's live reference set. Pins protect
// pending writes and readers. The owner must serialize its snapshot and collection
// with reference publication and pin acquisition. Active file writes are serialized
// here so collection cannot remove their temporary files.
func (s *BlobStore) Collect(live map[string]bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := entry.Name()
		if !live[id] {
			if s.pins[id] > 0 {
				continue
			}
			if err := os.Remove(filepath.Join(s.dir, id)); err != nil {
				return err
			}
			delete(s.retired, id)
		}
	}
	return nil
}

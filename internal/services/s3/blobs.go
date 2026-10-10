package s3

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 protocol ETag.
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/amjadjibon/tinystack/internal/storage"
)

type bodyReader interface {
	io.Reader
	io.Seeker
	io.Closer
}

type memoryBody struct{ *bytes.Reader }

func (memoryBody) Close() error { return nil }

type limitedBody struct {
	io.Reader
	io.Closer
}

func (s *Service) releaseBody(data []byte) {
	if blob, ok := storage.BlobReference(data); ok {
		s.blobs.Release(blob)
	}
}

// openBody runs outside transactions. Legacy bytes are retained for direct
// callers that supply a store without app startup migration.
func (s *Service) openBody(data []byte) (bodyReader, error) {
	if blob, ok := storage.BlobReference(data); ok {
		return s.blobs.Open(blob)
	}

	return memoryBody{bytes.NewReader(data)}, nil
}

// partsReader opens one part at a time, keeping completion memory and file
// descriptors bounded even for uploads with thousands of parts.
type partsReader struct {
	service *Service
	current bodyReader
	refs    [][]byte
}

func (r *partsReader) Read(p []byte) (int, error) {
	for {
		if r.current == nil {
			if len(r.refs) == 0 {
				return 0, io.EOF
			}

			var err error
			r.current, err = r.service.openBody(r.refs[0])
			if err != nil {
				return 0, err
			}

			r.refs = r.refs[1:]
		}

		n, err := r.current.Read(p)
		if errors.Is(err, io.EOF) {
			closeErr := r.current.Close()
			r.current = nil
			if closeErr != nil {
				return n, closeErr
			}

			if n > 0 {
				return n, nil
			}

			continue
		}

		return n, err
	}
}

func (r *partsReader) Close() error {
	if r.current != nil {
		return r.current.Close()
	}

	return nil
}

// writeBody computes protocol digests during the copy to the immutable file.
func (s *Service) writeBody(body []byte) (blob storage.Blob, md5Sum []byte, values map[string]string, err error) {
	if s.blobErr != nil {
		return storage.Blob{}, nil, nil, s.blobErr
	}

	md5Digest := md5.New() // #nosec G401 -- S3 protocol ETag.
	writers := []io.Writer{md5Digest}
	digests := map[string]hash.Hash{}
	for algorithm, create := range checksums() {
		digest := create()
		digests[algorithm] = digest
		writers = append(writers, digest)
	}

	blob, err = s.blobs.Write(io.TeeReader(bytes.NewReader(body), io.MultiWriter(writers...)))
	if err != nil {
		return storage.Blob{}, nil, nil, err
	}

	values = map[string]string{}
	for algorithm, digest := range digests {
		values[algorithm] = base64.StdEncoding.EncodeToString(digest.Sum(nil))
	}

	return blob, md5Digest.Sum(nil), values, nil
}

// bodyStore is private to S3. Other services use the unwrapped record store.
// Readers pin under the store read lock, before a delete can commit. updateMu
// orders write commits and retirement, including copies that republish pinned IDs;
// reads remain concurrent and file I/O stays outside record callbacks.
type bodyStore struct {
	storage.Store
	blobs    *storage.BlobStore
	updateMu sync.Mutex
}

// isBodyKey identifies current, historical, and multipart body records.
func isBodyKey(key string) bool {
	_, _, rest, ok := storage.Scoped(key, "s3")
	if !ok {
		return false
	}

	parts := strings.SplitN(rest, "/", 3)
	return len(parts) == 3 && (parts[1] == "db" || parts[1] == "bb" || parts[1] == "pb")
}

// legacyBodyKey maps the old raw-byte namespace to the reference namespace.
// Keeping namespaces distinct avoids mistaking arbitrary legacy object contents
// for a serialized file reference, including during an interrupted migration.
func legacyBodyKey(key string) (string, bool) {
	account, region, rest, ok := storage.Scoped(key, "s3")
	if !ok {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || (parts[1] != "d" && parts[1] != "b" && parts[1] != "p") {
		return "", false
	}
	return account + "/" + region + "/s3/" + parts[0] + "/" + parts[1] + "b/" + parts[2], true
}

func (s *Service) migrate(ctx context.Context) error {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			if _, legacy := legacyBodyKey(key); legacy {
				keys = append(keys, key)
			}
		}

		return nil
	}); err != nil {
		return err
	}

	for _, key := range keys {
		var data []byte
		if err := s.store.View(ctx, func(tx storage.ReadTx) error { data, _ = tx.Get(key); return nil }); err != nil {
			return err
		}

		b, err := s.blobs.Write(bytes.NewReader(data))
		if err != nil {
			return err
		}

		referenceKey, _ := legacyBodyKey(key)
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Put(referenceKey, b.Record())
			tx.Delete(key)
			return nil
		})
		s.blobs.Release(b)
		if err != nil {
			return err
		}
	}

	return nil
}

const blobCountPrefix = "aws/s3/blob/"

type blobTx struct {
	storage.WriteTx
	deltas map[string]int64
}

func (t *blobTx) adjust(key string, data []byte, delta int64) {
	if isBodyKey(key) {
		if b, ok := storage.BlobReference(data); ok {
			t.deltas[b.ID] += delta
		}
	}
}

func (t *blobTx) Put(key string, data []byte) {
	old, _ := t.Get(key)
	t.adjust(key, old, -1)
	t.adjust(key, data, 1)
	t.WriteTx.Put(key, data)
}

func (t *blobTx) Delete(key string) {
	old, _ := t.Get(key)
	t.adjust(key, old, -1)
	t.WriteTx.Delete(key)
}

func (s *bodyStore) Update(ctx context.Context, fn func(storage.WriteTx) error) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	retired, live := map[string]bool{}, map[string]bool{}
	err := s.Store.Update(ctx, func(tx storage.WriteTx) error {
		t := &blobTx{WriteTx: tx, deltas: map[string]int64{}}
		if err := fn(t); err != nil {
			return err
		}

		for id, delta := range t.deltas {
			key := blobCountPrefix + id
			var count int64
			if data, found := tx.Get(key); found {
				var err error
				count, err = strconv.ParseInt(string(data), 10, 64)
				if err != nil || count <= 0 {
					return fmt.Errorf("invalid S3 blob count for %s", id)
				}
			}

			count += delta
			if count < 0 {
				return fmt.Errorf("negative S3 blob count for %s", id)
			}

			if count == 0 {
				tx.Delete(key)
				retired[id] = true
			} else {
				tx.Put(key, []byte(strconv.FormatInt(count, 10)))
				live[id] = true
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.blobs.Retire(retired, live)
	return nil
}

// rebuildCounts runs only before requests are served. The bucket index cannot
// enumerate dangling regional records, so recovery scans all account/region keys.
func (s *Service) rebuildCounts(ctx context.Context) error {
	return s.store.Store.Update(ctx, func(tx storage.WriteTx) error {
		counts := map[string]int64{}
		for _, key := range tx.Keys("") {
			if isBodyKey(key) {
				data, _ := tx.Get(key)
				if b, ok := storage.BlobReference(data); ok {
					counts[b.ID]++
				}
			}
		}

		for _, key := range tx.Keys(blobCountPrefix) {
			tx.Delete(key)
		}

		for id, count := range counts {
			tx.Put(blobCountPrefix+id, []byte(strconv.FormatInt(count, 10)))
		}

		return nil
	})
}

func (s *Service) collect(ctx context.Context) error {
	s.store.updateMu.Lock()
	defer s.store.updateMu.Unlock()
	var live map[string]bool
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		live = map[string]bool{}
		for _, key := range tx.Keys(blobCountPrefix) {
			live[strings.TrimPrefix(key, blobCountPrefix)] = true
		}
		return nil
	}); err != nil {
		return err
	}
	return s.blobs.Collect(live)
}

// Resume migrates legacy body records and collects crash leftovers before serving requests.
func (s *Service) Resume(ctx context.Context) error {
	if s.blobErr != nil {
		return s.blobErr
	}
	if err := s.rebuildCounts(ctx); err != nil {
		return err
	}

	if err := s.migrate(ctx); err != nil {
		return err
	}
	return s.collect(ctx)
}

// Reset retires S3 body references before the app clears the remaining records.
func (s *Service) Reset(ctx context.Context) {
	if s.blobErr != nil {
		return
	}
	if err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("") {
			if isBodyKey(key) {
				tx.Delete(key)
			}
		}
		return nil
	}); err != nil {
		return
	}
	_ = s.collect(ctx)
}

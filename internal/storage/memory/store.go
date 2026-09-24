package memory

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/amjadjibon/tinystack/internal/storage"
)

// spliceLimit is the most keys a commit may add or remove before the sorted
// keys are rebuilt instead of spliced one at a time.
const spliceLimit = 128

// Store owns byte records. Its zero value is ready for use.
type Store struct {
	data    map[string][]byte
	persist Persist
	keys    []string // data's keys, sorted
	mu      sync.RWMutex
}

// Persist durably records one commit's changes: the records it puts and the
// keys it deletes. An error aborts the commit. It must not retain puts.
type Persist func(puts map[string][]byte, deletes []string) error

// New returns a store holding data. Update calls persist, when set, with each
// commit's changes before they become visible, so a failed write changes nothing.
func New(data map[string][]byte, persist Persist) *Store {
	return &Store{data: data, keys: slices.Sorted(maps.Keys(data)), persist: persist}
}

type transaction struct {
	base    map[string][]byte
	writes  map[string][]byte
	deleted map[string]bool
	keys    []string
}

func (t *transaction) Get(k string) ([]byte, bool) {
	if t.deleted[k] {
		return nil, false
	}

	v, ok := t.writes[k]
	if !ok {
		v, ok = t.base[k]
	}

	return append([]byte(nil), v...), ok
}

func (t *transaction) Keys(prefix string) []string {
	i, _ := slices.BinarySearch(t.keys, prefix)
	var out []string
	for _, k := range t.keys[i:] {
		if !strings.HasPrefix(k, prefix) {
			break
		}

		if !t.deleted[k] {
			out = append(out, k)
		}
	}

	added := false
	for k := range t.writes {
		if _, ok := t.base[k]; !ok && strings.HasPrefix(k, prefix) {
			out = append(out, k)
			added = true
		}
	}

	if added {
		slices.Sort(out)
	}

	return out
}

func (t *transaction) Put(k string, v []byte) {
	t.writes[k] = append([]byte(nil), v...)
	delete(t.deleted, k)
}

func (t *transaction) Delete(k string) { t.deleted[k] = true; delete(t.writes, k) }

// View holds a consistent read lock. Callbacks may not retain the transaction.
func (s *Store) View(ctx context.Context, fn func(storage.ReadTx) error) error {
	if guardErr := ctx.Err(); guardErr != nil {
		return guardErr
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if guardErr := ctx.Err(); guardErr != nil {
		return guardErr
	}

	return fn(&transaction{base: s.data, keys: s.keys})
}

// Update commits staged writes atomically on success, or discards all changes.
func (s *Store) Update(ctx context.Context, fn func(storage.WriteTx) error) error {
	if guardErr := ctx.Err(); guardErr != nil {
		return guardErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if guardErr := ctx.Err(); guardErr != nil {
		return guardErr
	}

	tx := &transaction{base: s.data, keys: s.keys, writes: map[string][]byte{}, deleted: map[string]bool{}}
	if guardErr := fn(tx); guardErr != nil {
		return guardErr
	}

	if guardErr := ctx.Err(); guardErr != nil {
		return guardErr
	}

	if s.persist != nil && len(tx.writes)+len(tx.deleted) > 0 {
		if guardErr := s.persist(tx.writes, slices.Collect(maps.Keys(tx.deleted))); guardErr != nil {
			return guardErr
		}
	}

	if s.data == nil {
		s.data = map[string][]byte{}
	}

	var added, removed []string
	for k := range tx.deleted {
		if _, ok := s.data[k]; ok {
			delete(s.data, k)
			removed = append(removed, k)
		}
	}

	for k, v := range tx.writes {
		if _, ok := s.data[k]; !ok {
			added = append(added, k)
		}

		s.data[k] = v
	}

	s.index(added, removed)
	return nil
}

// index brings the sorted keys up to date with a commit that added and removed
// keys. Overwrites leave them untouched.
func (s *Store) index(added, removed []string) {
	if len(added)+len(removed) > spliceLimit {
		s.keys = slices.Sorted(maps.Keys(s.data))
		return
	}

	for _, k := range removed {
		i, _ := slices.BinarySearch(s.keys, k)
		s.keys = slices.Delete(s.keys, i, i+1)
	}

	for _, k := range added {
		i, _ := slices.BinarySearch(s.keys, k)
		s.keys = slices.Insert(s.keys, i, k)
	}
}

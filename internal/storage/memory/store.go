package memory

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/amjadjibon/tinystack/internal/storage"
)

// Store owns byte records. Its zero value is ready for use.
type Store struct {
	data    map[string][]byte
	persist Persist
	mu      sync.RWMutex
}

// Persist durably records one commit's changes: the records it puts and the
// keys it deletes. An error aborts the commit. It must not retain puts.
type Persist func(puts map[string][]byte, deletes []string) error

// New returns a store holding data. Update calls persist, when set, with each
// commit's changes before they become visible, so a failed write changes nothing.
func New(data map[string][]byte, persist Persist) *Store {
	return &Store{data: data, persist: persist}
}

type transaction struct {
	base    map[string][]byte
	writes  map[string][]byte
	deleted map[string]bool
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

func (t *transaction) Keys() []string {
	set := map[string]bool{}
	for k := range t.base {
		if !t.deleted[k] {
			set[k] = true
		}
	}

	for k := range t.writes {
		set[k] = true
	}

	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	return keys
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

	return fn(&transaction{base: s.data})
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

	tx := &transaction{base: s.data, writes: map[string][]byte{}, deleted: map[string]bool{}}
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

	for k := range tx.deleted {
		delete(s.data, k)
	}

	for k, v := range tx.writes {
		s.data[k] = v
	}

	return nil
}

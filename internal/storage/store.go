package storage

import (
	"context"
	"encoding/json"
	"strings"
)

// ReadTx exposes a consistent snapshot; handles must stay inside the callback.
type ReadTx interface {
	Get(string) ([]byte, bool)
	// Keys returns, in order, the keys that start with prefix.
	Keys(prefix string) []string
}

// WriteTx stages changes which are committed together on callback success.
type WriteTx interface {
	ReadTx
	Put(string, []byte)
	Delete(string)
}

// Store runs serializable transactions; callbacks must not perform external I/O.
type Store interface {
	View(context.Context, func(ReadTx) error) error
	Update(context.Context, func(WriteTx) error) error
}

// Record is an opaque key-value pair.
type Record struct {
	Key   string
	Value []byte
}

type recordWire struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// MarshalJSON encodes Record with key and value as lossless base64 byte slices.
func (r Record) MarshalJSON() ([]byte, error) {
	val := r.Value
	if val == nil {
		val = []byte{}
	}

	return json.Marshal(recordWire{
		Key:   []byte(r.Key),
		Value: val,
	})
}

// UnmarshalJSON decodes Record with key and value from base64 byte slices.
func (r *Record) UnmarshalJSON(data []byte) error {
	var w recordWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}

	r.Key = string(w.Key)
	r.Value = w.Value
	if r.Value == nil {
		r.Value = []byte{}
	}

	return nil
}

// Clear deletes every record in one transaction, so concurrent callers see the
// state before or after, never part of it. Services that poll the store find it
// empty and carry on.
func Clear(ctx context.Context, store Store) error {
	return store.Update(ctx, func(tx WriteTx) error {
		for _, key := range tx.Keys("") {
			tx.Delete(key)
		}

		return nil
	})
}

// Scoped splits a key laid out as "<account>/<region>/<service>/<rest>" and
// reports whether it belongs to service.
func Scoped(key, service string) (account, region, rest string, ok bool) {
	parts := strings.SplitN(key, "/", 4)
	if len(parts) != 4 || parts[2] != service {
		return "", "", "", false
	}

	return parts[0], parts[1], parts[3], true
}

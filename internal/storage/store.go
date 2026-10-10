package storage

import (
	"context"
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

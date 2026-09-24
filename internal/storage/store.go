package storage

import (
	"context"
	"strings"
)

// ReadTx exposes a consistent snapshot; handles must stay inside the callback.
type ReadTx interface {
	Get(string) ([]byte, bool)
	Keys() []string
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

// Scoped splits a key laid out as "<account>/<region>/<service>/<rest>" and
// reports whether it belongs to service.
func Scoped(key, service string) (account, region, rest string, ok bool) {
	parts := strings.SplitN(key, "/", 4)
	if len(parts) != 4 || parts[2] != service {
		return "", "", "", false
	}

	return parts[0], parts[1], parts[3], true
}

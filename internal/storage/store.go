package storage

import "context"

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

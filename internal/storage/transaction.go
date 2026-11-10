package storage

import "context"

// Atomic runs service operations against one transaction. The supplied store must not escape fn.
func Atomic(ctx context.Context, store Store, fn func(Store) error) error {
	return store.Update(ctx, func(tx WriteTx) error { return fn(transactionStore{tx}) })
}

type transactionStore struct{ tx WriteTx }

func (s transactionStore) View(_ context.Context, fn func(ReadTx) error) error { return fn(s.tx) }

func (s transactionStore) Update(_ context.Context, fn func(WriteTx) error) error { return fn(s.tx) }

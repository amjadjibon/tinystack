package app

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// openStore returns the store the storage mode selects, keeping persistent
// state in the configured data directory, and the function that releases it.
func openStore(ctx context.Context, cfg *config.Config) (storage.Store, func() error, error) {
	if cfg.Storage.Mode == config.StorageMemory {
		return &memory.Store{}, func() error { return nil }, nil
	}

	dir, err := cfg.DataDir()
	if err != nil {
		return nil, nil, err
	}

	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		return nil, nil, err
	}

	return store, store.Close, nil
}

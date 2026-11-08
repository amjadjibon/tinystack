package dockercli

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// RuntimeOwner returns the persistent identity used to isolate a store's Docker resources.
func RuntimeOwner(store storage.Store, key string) (string, error) {
	var owner string
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		value, found, err := storage.Read[string](tx, key)
		if err != nil {
			return err
		}

		if found {
			owner = *value
			return nil
		}

		owner = common.RandomID(32)
		return storage.Write(tx, key, owner)
	})
	return owner, err
}

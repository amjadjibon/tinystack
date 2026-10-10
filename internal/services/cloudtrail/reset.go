package cloudtrail

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/storage"
)

// Reset forgets event history. The app subsequently clears all remaining records.
func (s *Service) Reset(ctx context.Context) {
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys("") {
			_, _, rest, ok := storage.Scoped(k, serviceName)
			if ok && strings.HasPrefix(rest, "history/") {
				tx.Delete(k)
			}
		}

		return nil
	})
}

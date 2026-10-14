package servicequotas

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicequotas"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists increase requests; the built-in quota table is not mutable state.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, "request/") {
				continue
			}

			r, _, err := storage.Read[api.RequestedServiceQuotaChange](tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: "request", Name: *r.Id,
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r},
			})
		}

		return nil
	})

	return out, err
}

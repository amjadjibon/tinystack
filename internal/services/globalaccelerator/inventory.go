package globalaccelerator

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources returns records across accounts and Regions for the shared inventory.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			r, found, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if !found {
				continue
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    r.Kind,
					Name:    name(r),
					ARN:     r.ARN,
					Tags:    maps.Clone(r.Tags),
					Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r},
				},
			)
		}

		return nil
	})
	return out, err
}

func name(r *record) string {
	if r.Accelerator != nil {
		return common.Value(r.Accelerator.Name)
	}

	return r.ARN
}

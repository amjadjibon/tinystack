package lakeformation

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes records without claiming execution or authorization enforcement.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, "lakeformation")
			if !ok {
				continue
			}

			kind, id, _ := strings.Cut(rest, "/")
			data, _ := tx.Get(k)
			var configuration map[string]any
			if err := json.Unmarshal(data, &configuration); err != nil {
				return err
			}

			name := id
			if v, ok := configuration["TagKey"].(string); ok {
				name = v
			}

			if v, ok := configuration["ResourceArn"].(string); ok {
				name = v
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kind,
					Name:    name,
					Details: map[string]any{
						"RecordsOnly":    true,
						"ExecutionReady": false,
						"Configuration":  configuration,
					},
				},
			)
		}

		return nil
	})
	return out, err
}

package accessanalyzer

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored analyzers for the TinyStack resource catalog.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindAnalyzer {
				continue
			}

			r, _, err := storage.Read[analyzerRecord](tx, k)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kindAnalyzer,
				Name:    r.Name,
				ARN:     r.ARN,
				Tags:    maps.Clone(r.Tags),
				Details: map[string]any{
					"RecordsOnly":    true,
					"ExecutionReady": false,
					"Configuration":  r,
				},
			})
		}

		return nil
	})

	return out, err
}

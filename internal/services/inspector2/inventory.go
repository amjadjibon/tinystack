package inspector2

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored filters for the TinyStack resource catalog.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindFilter {
				continue
			}

			r, _, err := storage.Read[api.Filter](tx, k)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kindFilter,
				Name:    common.Value(r.Name),
				ARN:     common.Value(r.Arn),
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

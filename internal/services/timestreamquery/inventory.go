package timestreamquery

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored scheduled queries.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindScheduledQuery {
				continue
			}

			r, _, e := storage.Read[scheduledQueryRecord](tx, k)
			if e != nil {
				return e
			}

			scope := request.Scope{AccountID: account, Region: region}
			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kindScheduledQuery,
				Name:    r.Name,
				ARN:     scheduledQueryARN(scope, r.Name),
				Tags:    maps.Clone(r.Tags),
				Details: map[string]any{
					"RecordsOnly":    true,
					"ExecutionReady": false,
					"Configuration":  r.description(scope, time.Now().UTC()),
				},
			})
		}

		return nil
	})

	return out, err
}

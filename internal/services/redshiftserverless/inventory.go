package redshiftserverless

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindNamespace && kind != kindWorkgroup && kind != kindSnapshot && kind != kindEndpoint &&
				kind != kindUsageLimit {
				continue
			}

			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kind,
					Name:    r.Name,
					ARN:     r.ARN,
					Tags:    common.CloneTags(r.Tags),
					Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r},
				},
			)
		}

		return nil
	})
	return out, err
}

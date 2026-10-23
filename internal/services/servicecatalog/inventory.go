package servicecatalog

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
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			r := catalog.Resource{Account: account, Region: region, Type: kind}
			var config any
			switch kind {
			case kindPortfolio:
				p, _, e := storage.Read[portfolio](tx, key)
				if e != nil {
					return e
				}

				r.Name = common.Value(p.Detail.DisplayName)
				r.ARN = common.Value(p.Detail.ARN)
				r.Tags = common.CloneTags(p.Tags)
				config = p.Detail
			case kindProduct:
				p, _, e := storage.Read[product](tx, key)
				if e != nil {
					return e
				}

				r.Name = common.Value(p.Detail.ProductViewSummary.Name)
				r.ARN = common.Value(p.Detail.ProductARN)
				r.Tags = common.CloneTags(p.Tags)
				config = p.Detail
			default:
				continue
			}

			r.Details = map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": config}
			out = append(out, r)
		}

		return nil
	})
	return out, err
}

var _ catalog.Inventory = (*Service)(nil)

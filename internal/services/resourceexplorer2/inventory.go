package resourceexplorer2

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != "index" && kind != "view" {
				continue
			}

			r, _, err := storage.Read[record](tx, k)
			if err != nil {
				return err
			}

			item := catalog.Resource{Account: account, Region: region, Type: kind, Tags: common.CloneTags(r.Tags)}
			if r.Index != nil {
				item.ARN = common.Value(r.Index.Arn)
				item.Name = "default"
				item.Details = r.Index
			} else {
				item.ARN = common.Value(r.View.ViewArn)
				item.Name = common.Value(r.View.ViewName)
				item.Details = r.View
			}

			out = append(out, item)
		}

		return nil
	})
	return out, err
}

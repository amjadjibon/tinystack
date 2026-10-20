package opensearchserverless

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
			if !ok || !strings.HasPrefix(rest, "collection/") {
				continue
			}

			r, _, err := storage.Read[collection](tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: collectionKind,
				Name: common.Value(r.Detail.Name), ARN: common.Value(r.Detail.Arn), Tags: common.CloneTags(r.Tags),
				Details: map[string]any{
					"RecordsOnly": true, "ExecutionReady": false, "State": common.Value(r.Detail.Status),
					"Configuration": r.Detail,
				},
			})
		}

		return nil
	})
	return out, err
}

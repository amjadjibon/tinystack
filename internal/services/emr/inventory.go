package emr

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
			if !ok || !strings.HasPrefix(rest, "cluster/") {
				continue
			}

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			tags, e := parseTags(r.Cluster.Tags)
			if e != nil {
				return e
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: "cluster", Name: common.Value(r.Cluster.Name),
				ARN: common.Value(r.Cluster.ClusterArn), Tags: tags, Details: map[string]any{
					"RecordsOnly":    true,
					"ExecutionReady": false,
					"State": common.Value(
						r.Cluster.Status.State,
					),
					"ClusterId":     *r.Cluster.Id,
					"Configuration": r.Cluster,
				},
			})
		}

		return nil
	})
	return out, err
}

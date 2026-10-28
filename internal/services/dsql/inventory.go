package dsql

import (
	"context"
	"fmt"
	"maps"
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
			if !ok || !strings.HasPrefix(rest, kindCluster+"/") {
				continue
			}

			r, _, err := storage.Read[record](tx, k)
			if err != nil {
				return err
			}

			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r.GetClusterOutput}
			if s.runtime != nil {
				details["RecordsOnly"] = false
				details["ExecutionReady"] = common.Value(r.Status) == active
				details["Address"] = fmt.Sprintf("127.0.0.1:%d", r.ProxyPort)
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kindCluster,
				Name:    common.Value(r.Identifier),
				ARN:     common.Value(r.Arn),
				Tags:    maps.Clone(r.Tags),
				Details: details,
			})
		}

		return nil
	})
	return out, err
}

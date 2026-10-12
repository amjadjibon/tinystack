package redshift

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
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
			if kind != clusterKind && kind != subnetKind && kind != parameterKind {
				continue
			}

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			configuration := any(r.Cluster)
			if kind == subnetKind {
				configuration = r.Subnet
			}

			if kind == parameterKind {
				configuration = r.Parameter
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Name: r.Name, Type: kind,
				ARN: arn(request.Scope{AccountID: account, Region: region}, kind, r.Name), Tags: maps.Clone(r.Tags),
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": configuration},
			})
		}

		return nil
	})
	return out, err
}

package elb

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, balancerKind+"/") {
				continue
			}

			b, _, e := storage.Read[balancer](tx, key)
			if e != nil {
				return e
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Name: b.name(), Type: "load-balancer",
				ARN: arn(request.Scope{AccountID: account, Region: region}, b.name()), Tags: maps.Clone(b.Tags),
				Details: map[string]any{
					"RecordsOnly": true, "ExecutionReady": false,
					"Scheme": common.Value(b.Description.Scheme), "DNSName": common.Value(b.Description.DNSName),
				},
			})
		}

		return nil
	})

	return out, err
}

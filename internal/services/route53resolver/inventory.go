package route53resolver

import (
	"context"
	"maps"
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
			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			item := catalog.Resource{
				Account: account, Region: region, Type: kind, Name: r.ID, Tags: maps.Clone(r.Tags),
				Details: details,
			}

			switch kind {
			case endpointKind:
				item.ARN = common.Value(r.Endpoint.Arn)
				details["Configuration"] = r.Endpoint
				details["IpAddresses"] = r.Ips
			case ruleKind:
				item.ARN = common.Value(r.Rule.Arn)
				details["Configuration"] = r.Rule
			case associationKind:
				details["Configuration"] = r.Association
			default:
				continue
			}

			out = append(out, item)
		}

		return nil
	})
	return out, err
}

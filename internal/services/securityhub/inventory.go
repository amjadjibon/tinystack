package securityhub

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
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
			r := catalog.Resource{Account: account, Region: region, Type: kind}
			var configuration any
			switch kind {
			case "hub":
				h, _, err := storage.Read[hubRecord](tx, k)
				if err != nil {
					return err
				}

				r.Name = "default"
				r.ARN = common.Value(h.Hub.HubArn)
				r.Tags = common.CloneTags(h.Tags)
				configuration = h
			case "standard":
				v, _, err := storage.Read[api.StandardsSubscription](tx, k)
				if err != nil {
					return err
				}

				r.Name = common.Value(v.StandardsArn)
				r.ARN = common.Value(v.StandardsSubscriptionArn)
				configuration = v
			case "insight":
				v, _, err := storage.Read[api.Insight](tx, k)
				if err != nil {
					return err
				}

				r.Name = common.Value(v.Name)
				r.ARN = common.Value(v.InsightArn)
				configuration = v
			case "action":
				v, _, err := storage.Read[api.ActionTarget](tx, k)
				if err != nil {
					return err
				}

				r.Name = common.Value(v.Name)
				r.ARN = common.Value(v.ActionTargetArn)
				configuration = v
			default:
				continue
			}

			r.Details = map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": configuration}
			out = append(out, r)
		}

		return nil
	})

	return out, err
}

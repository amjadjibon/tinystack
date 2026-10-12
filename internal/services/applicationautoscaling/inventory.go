package applicationautoscaling

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
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
			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			r := catalog.Resource{Account: account, Region: region, Type: kind, Details: details}
			switch kind {
			case targetKind:
				t, _, e := storage.Read[target](tx, key)
				if e != nil {
					return e
				}

				r.Name = *t.Configuration.ResourceId + "|" + *t.Configuration.ScalableDimension
				r.ARN = *t.Configuration.ScalableTargetARN
				r.Tags = maps.Clone(t.Tags)
				details["Configuration"] = t.Configuration
			case policyKind:
				p, _, e := storage.Read[api.ScalingPolicy](tx, key)
				if e != nil {
					return e
				}

				r.Name = *p.PolicyName
				r.ARN = *p.PolicyARN
				details["Configuration"] = p
			case actionKind:
				a, _, e := storage.Read[api.ScheduledAction](tx, key)
				if e != nil {
					return e
				}

				r.Name = *a.ScheduledActionName
				r.ARN = *a.ScheduledActionARN
				details["Configuration"] = a
			default:
				continue
			}

			out = append(out, r)
		}

		return nil
	})
	return out, err
}

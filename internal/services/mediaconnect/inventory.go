package mediaconnect

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists flow records and their independently taggable children.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, "flow/") {
				continue
			}

			r, _, txErr := storage.Read[record](tx, key)
			if txErr != nil {
				return txErr
			}

			appendResource := func(kind, name, arn string, config any) {
				out = append(out, catalog.Resource{
					Account: account, Region: region, Type: kind, Name: name, ARN: arn,
					Tags: common.CloneTags(r.Tags[arn]), Details: map[string]any{
						"RecordsOnly": true, "ExecutionReady": false, "Configuration": config,
					},
				})
			}

			appendResource("flow", common.Value(r.Flow.Name), common.Value(r.Flow.FlowArn), r.Flow)
			for _, v := range r.Flow.Sources {
				appendResource("flowsource", common.Value(v.Name), common.Value(v.SourceArn), v)
			}

			for i := range r.Flow.Outputs {
				v := &r.Flow.Outputs[i]
				appendResource("flowoutput", common.Value(v.Name), common.Value(v.OutputArn), v)
			}

			for _, v := range r.Flow.Entitlements {
				appendResource("flowentitlement", common.Value(v.Name), common.Value(v.EntitlementArn), v)
			}
		}

		return nil
	})
	return out, err
}

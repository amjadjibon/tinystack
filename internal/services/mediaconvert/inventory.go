package mediaconvert

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes regional MediaConvert records to inventory and tagging.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindQueue && kind != kindPreset && kind != kindTemplate && kind != kindJob {
				continue
			}

			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			typ := map[string]string{kindQueue: "queue", kindPreset: "preset", kindTemplate: "jobtemplate", kindJob: "job"}[kind]
			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    typ,
					Name:    r.Name,
					ARN:     arn(request.Scope{AccountID: account, Region: region}, kind, r.Name),
					Tags:    maps.Clone(r.Tags),
					Details: map[string]any{
						"RecordsOnly":    kind != kindJob,
						"ExecutionReady": kind == kindJob,
						"ProducesMedia":  false,
						"Configuration":  r,
					},
				},
			)
		}

		return nil
	})

	return out, err
}

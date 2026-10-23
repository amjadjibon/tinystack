package fis

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes template records and locally executed experiments to the resource catalog.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindTemplate && kind != kindExperiment {
				continue
			}

			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			resource := catalog.Resource{Account: account, Region: region, Type: kind}
			if r.Template != nil {
				resource.Name = *r.Template.Id
				resource.ARN = *r.Template.Arn
				resource.Tags = r.Template.Tags
				resource.Details = map[string]any{
					"Configuration":  r.Template,
					"ExecutionReady": true,
					"RecordsOnly":    false,
				}
			} else {
				resource.Name = *r.Experiment.Id
				resource.ARN = *r.Experiment.Arn
				resource.Tags = r.Experiment.Tags
				resource.Details = map[string]any{
					"Configuration":  r.Experiment,
					"ExecutionReady": true,
					"RecordsOnly":    false,
				}
			}

			out = append(out, resource)
		}

		return nil
	})
	return out, err
}

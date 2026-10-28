package codepipeline

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
			account, region, _, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.Contains(key, "/codepipeline/pipeline/") {
				continue
			}

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    pipelineKind,
				Name:    *r.Pipeline.Name,
				ARN:     pipelineARN(request.Scope{AccountID: account, Region: region}, *r.Pipeline.Name),
				Tags:    maps.Clone(r.Tags),
				Details: map[string]any{
					"RecordsOnly":    false,
					"ExecutionReady": true,
					"Configuration":  r.Pipeline,
				},
			})
		}

		return nil
	})

	return out, err
}

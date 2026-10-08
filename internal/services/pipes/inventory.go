package pipes

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every pipe, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindPipe+"/") {
				continue
			}

			p, _, err := storage.Read[pipe](tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Type: "pipe", Account: account, Region: region, Name: p.Name,
				ARN: pipeARN(request.Scope{AccountID: account, Region: region}, p.Name), Tags: p.Tags,
				Details: map[string]any{
					"State": p.currentState(), "Source": p.Source, "Target": p.Target,
					"Enrichment": p.Enrichment,
				},
			})
		}

		return nil
	})

	return out, err
}

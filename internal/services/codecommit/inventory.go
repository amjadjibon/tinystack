package codecommit

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
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

			kind, name, ok := strings.Cut(rest, "/")
			if !ok || kind != repositoryKind {
				continue
			}

			rec, found, e := storage.Read[repositoryRecord](tx, key)
			if e != nil {
				return e
			}

			if !found {
				continue
			}

			details := map[string]any{
				"RecordsOnly":    true,
				"ExecutionReady": false,
				"Configuration":  rec.Metadata,
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    repositoryKind,
				Name:    name,
				ARN:     *rec.Metadata.Arn,
				Tags:    maps.Clone(rec.Tags),
				Details: details,
			})
		}

		return nil
	})
	return out, err
}

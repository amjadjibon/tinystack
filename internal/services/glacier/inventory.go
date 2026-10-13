package glacier

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

			kind, _, _ := strings.Cut(rest, "/")
			if kind != vaultKind {
				continue
			}

			rec, found, err := storage.Read[vaultRecord](tx, key)
			if err != nil {
				return err
			}

			if !found {
				continue
			}

			details := map[string]any{
				"RecordsOnly":      true,
				"ExecutionReady":   false,
				"CreationDate":     rec.CreationDate,
				"NumberOfArchives": int64(0),
				"SizeInBytes":      int64(0),
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kind,
				Name:    rec.VaultName,
				ARN:     vaultARN(region, account, rec.VaultName),
				Tags:    maps.Clone(rec.Tags),
				Details: details,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

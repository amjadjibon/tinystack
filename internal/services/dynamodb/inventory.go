package dynamodb

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every table with its description, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys() {
			account, region, name, ok := storage.Scoped(key, "dynamodb")
			if !ok {
				continue
			}

			t, err := load(tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Type:    "table",
				Account: account,
				Region:  region,
				Name:    name,
				ARN:     value(t.Description.TableArn),
				Tags:    t.Tags,
				Details: t.Description,
			})
		}

		return nil
	})

	return out, err
}

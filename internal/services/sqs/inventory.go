package sqs

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every queue with its attributes, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	now := s.now().UnixMilli()
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, name, ok := storage.Scoped(key, "sqs")
			if !ok {
				continue
			}

			q, err := load(tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Type:    "queue",
				Account: account,
				Region:  region,
				Name:    name,
				ARN:     keyARN(key),
				Tags:    q.Tags,
				Details: q.attributes(key, now),
			})
		}

		return nil
	})

	return out, err
}

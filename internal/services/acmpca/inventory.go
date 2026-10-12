package acmpca

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the authorities that are not deleted.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	now := time.Now()
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, id, _ := strings.Cut(rest, "/")
			if kind != authorityKind {
				continue
			}

			rec, _, readErr := storage.Read[authorityRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if common.Value(rec.Authority.Status) == statusDeleted || rec.expired(now) {
				continue
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    authorityKind,
				Name:    id,
				ARN:     *rec.Authority.Arn,
				Tags:    maps.Clone(rec.Tags),
				Details: map[string]any{
					"RecordsOnly":    true,
					"ExecutionReady": false,
					"Configuration":  rec.Authority,
				},
			})
		}

		return nil
	})

	return out, err
}

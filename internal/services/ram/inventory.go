package ram

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
			if kind != shareKind {
				continue
			}

			rec, found, readErr := storage.Read[resourceShareRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if !found || rec == nil || rec.Share == nil ||
				(rec.Share.Status != nil && *rec.Share.Status == statusDeleted) {
				continue
			}

			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			r := catalog.Resource{Account: account, Region: region, Type: kind, Details: details}
			if rec.Share.Name != nil {
				r.Name = *rec.Share.Name
			}

			if rec.Share.ResourceShareArn != nil {
				r.ARN = *rec.Share.ResourceShareArn
			}

			r.Tags = maps.Clone(rec.Tags)
			details["Configuration"] = rec.Share
			out = append(out, r)
		}

		return nil
	})
	return out, err
}

package opensearch

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
			if kind != domainKind {
				continue
			}

			rec, _, readErr := storage.Read[domainRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if rec.Status == nil || rec.Status.DomainName == nil || rec.Status.ARN == nil {
				continue
			}

			details := map[string]any{
				"RecordsOnly":    s.runtime == nil,
				"ExecutionReady": s.runtime != nil && rec.ready(),
				"Configuration":  rec.Status,
			}

			if rec.Failure != "" {
				details["Failure"] = rec.Failure
			}

			r := catalog.Resource{
				Account: account,
				Region:  region,
				Type:    domainKind,
				Name:    *rec.Status.DomainName,
				ARN:     *rec.Status.ARN,
				Tags:    maps.Clone(rec.Tags),
				Details: details,
			}

			out = append(out, r)
		}

		return nil
	})
	return out, err
}

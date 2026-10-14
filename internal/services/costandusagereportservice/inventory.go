package costandusagereportservice

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes definitions as records, without claiming delivery status.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || region != "" || !strings.HasPrefix(rest, "report/") {
				continue
			}

			r, _, e := storage.Read[reportRecord](tx, key)
			if e != nil {
				return e
			}

			name := *r.Definition.ReportName
			out = append(out, catalog.Resource{
				Account: account, Type: "report-definition", Name: name,
				ARN: fmt.Sprintf("arn:aws:cur:us-east-1:%s:definition/%s", account, name), Tags: maps.Clone(r.Tags),
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r.Definition},
			})
		}

		return nil
	})
	return out, err
}

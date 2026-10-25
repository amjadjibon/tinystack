package bedrockagentruntime

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentruntime"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources reports retained standalone sessions across accounts and Regions.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, "session/") {
				continue
			}

			r, _, err := storage.Read[api.GetSessionResponse](tx, key)
			if err != nil {
				return err
			}

			if r.CreatedAt != nil && time.Since(*r.CreatedAt) >= 30*24*time.Hour {
				continue
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kindSession,
					Name:    common.Value(r.SessionId),
					ARN:     common.Value(r.SessionArn),
					Details: map[string]any{"RecordsOnly": true, "Configuration": r},
				},
			)
		}

		return nil
	})
	return out, err
}

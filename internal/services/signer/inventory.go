package signer

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes signing profile metadata and tags to the shared inventory.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.HasPrefix(rest, "profile/") {
				continue
			}

			r, _, e := storage.Read[profile](tx, k)
			if e != nil {
				return e
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    "signingprofile",
					Name:    *r.Detail.ProfileName,
					ARN:     *r.Detail.Arn,
					Tags:    common.CloneTags(r.Detail.Tags),
					Details: map[string]any{
						"RecordsOnly":    true,
						"ExecutionReady": false,
						"State":          *r.Detail.Status,
						"Configuration":  r.Detail,
					},
				},
			)
		}

		return nil
	})
	return out, err
}

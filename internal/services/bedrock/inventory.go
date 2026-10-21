package bedrock

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources exposes local guardrails and application profiles to shared inventory.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			if strings.HasPrefix(rest, "guardrail/") {
				row, _, e := storage.Read[guardrail](tx, key)
				if e != nil {
					return e
				}

				out = append(
					out,
					catalog.Resource{
						Account: account,
						Region:  region,
						Type:    guardrailKind,
						Name:    *row.Detail.Name,
						ARN:     *row.Detail.GuardrailArn,
						Tags:    common.CloneTags(row.Tags),
						Details: row.Detail,
					},
				)
			}

			if strings.HasPrefix(rest, "profile/") {
				row, _, e := storage.Read[inferenceProfile](tx, key)
				if e != nil {
					return e
				}

				out = append(
					out,
					catalog.Resource{
						Account: account,
						Region:  region,
						Type:    applicationProfileKind,
						Name:    *row.Detail.InferenceProfileName,
						ARN:     *row.Detail.InferenceProfileArn,
						Tags:    common.CloneTags(row.Tags),
						Details: row.Detail,
					},
				)
			}
		}

		return nil
	})
	return out, err
}

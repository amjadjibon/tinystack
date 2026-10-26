package s3control

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			if strings.HasPrefix(rest, "mrap/") {
				r, _, txErr := storage.Read[mrapRecord](tx, k)
				if txErr != nil {
					return txErr
				}

				out = append(
					out,
					catalog.Resource{
						Account: account,
						Region:  region,
						Type:    "mrap",
						Name:    common.Value(r.Name),
						ARN:     fmt.Sprintf("arn:aws:s3::%s:accesspoint/%s", account, common.Value(r.Alias)),
						Details: map[string]any{"Configuration": r.MultiRegionAccessPointReport, "RecordsOnly": true},
					},
				)
				continue
			}

			if !strings.HasPrefix(rest, "accesspoint/") {
				continue
			}

			r, _, txErr := storage.Read[accessPointRecord](tx, k)
			if txErr != nil {
				return txErr
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    "accesspoint",
					Name:    common.Value(r.Name),
					ARN:     common.Value(r.AccessPointArn),
					Tags:    common.CloneTags(r.Tags),
					Details: map[string]any{"Configuration": r.GetAccessPointResult},
				},
			)
		}

		return nil
	})
	return out, err
}

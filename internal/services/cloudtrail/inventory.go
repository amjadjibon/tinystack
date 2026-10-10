package cloudtrail

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.HasPrefix(rest, "trail/") {
				continue
			}

			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			out = append(out, catalog.Resource{
				Type: "trail", Name: common.Value(r.Trail.Name), ARN: common.Value(r.Trail.TrailARN),
				Account: account, Region: region, Tags: r.Tags, Details: map[string]any{
					"S3BucketName": common.Value(r.Trail.S3BucketName),
					"IsLogging":    r.Logging,
				},
			})
		}

		return nil
	})
	return out, err
}

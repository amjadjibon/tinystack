package cloudfront

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every CloudFront distribution for the resource browser.
// CloudFront is global, so resources have no Region.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			parts := strings.SplitN(k, "/", 4)
			if len(parts) != 4 || parts[1] != service {
				continue
			}

			account := parts[0]
			if parts[2] == kindDistribution {
				rec, found, readErr := read[distributionRecord](tx, k)
				if readErr != nil {
					return readErr
				}
				if !found || rec.Distribution == nil {
					continue
				}

				id := value(rec.Distribution.Id)
				arn := value(rec.Distribution.ARN)
				domain := value(rec.Distribution.DomainName)
				status := value(rec.Distribution.Status)
				enabled := false
				if rec.Distribution.DistributionConfig != nil && rec.Distribution.DistributionConfig.Enabled != nil {
					enabled = *rec.Distribution.DistributionConfig.Enabled
				}

				out = append(out, catalog.Resource{
					Type:    "distribution",
					Account: account,
					Name:    id,
					ARN:     arn,
					Tags:    rec.Tags,
					Details: map[string]any{
						"DomainName": domain,
						"Status":     status,
						"Enabled":    enabled,
					},
				})
			}
		}
		return nil
	})
	return out, err
}

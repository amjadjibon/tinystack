package cloudfront

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every CloudFront distribution and OAC for the resource browser.
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
			switch parts[2] {
			case kindDistribution:
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
			case kindOAC:
				rec, found, readErr := read[oacRecord](tx, k)
				if readErr != nil {
					return readErr
				}
				if !found || rec.OAC == nil {
					continue
				}

				id := value(rec.OAC.Id)
				name := ""
				originType := ""
				if rec.OAC.OriginAccessControlConfig != nil {
					name = value(rec.OAC.OriginAccessControlConfig.Name)
					originType = value(rec.OAC.OriginAccessControlConfig.OriginAccessControlOriginType)
				}

				out = append(out, catalog.Resource{
					Type:    "origin-access-control",
					Account: account,
					Name:    id,
					ARN:     fmt.Sprintf("arn:aws:cloudfront::%s:origin-access-control/%s", account, id),
					Details: map[string]any{
						"Name":                          name,
						"OriginAccessControlOriginType": originType,
					},
				})
			}
		}
		return nil
	})
	return out, err
}

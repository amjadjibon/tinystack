package cloudfront

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
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
				rec, found, readErr := storage.Read[distributionRecord](tx, k)
				if readErr != nil {
					return readErr
				}
				if !found || rec.Distribution == nil {
					continue
				}

				id := common.Value(rec.Distribution.Id)
				arn := common.Value(rec.Distribution.ARN)
				domain := common.Value(rec.Distribution.DomainName)
				status := common.Value(rec.Distribution.Status)
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
			case kindKVS:
				rec, found, readErr := storage.Read[KVSRecord](tx, k)
				if readErr != nil {
					return readErr
				}
				if !found {
					continue
				}
				out = append(out, catalog.Resource{
					Type: kindKVS, Account: account,
					Name: common.Value(rec.Store.Name), ARN: common.Value(rec.Store.ARN), Tags: rec.Tags,
					Details: map[string]any{"Status": common.Value(rec.Store.Status)},
				})
			case kindOAC:
				rec, found, readErr := storage.Read[oacRecord](tx, k)
				if readErr != nil {
					return readErr
				}
				if !found || rec.OAC == nil {
					continue
				}

				id := common.Value(rec.OAC.Id)
				name := ""
				originType := ""
				if rec.OAC.OriginAccessControlConfig != nil {
					name = common.Value(rec.OAC.OriginAccessControlConfig.Name)
					originType = common.Value(rec.OAC.OriginAccessControlConfig.OriginAccessControlOriginType)
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

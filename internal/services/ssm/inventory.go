package ssm

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every parameter and document, for the resource browser. SecureString
// values are left out.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "ssm")
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			if strings.HasPrefix(rest, "d/") {
				rec, _, err := storage.Read[docRecord](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "document", Account: account, Region: region, Name: rec.Name,
					ARN: documentARN(scope, rec), Tags: rec.Tags,
					Details: map[string]any{
						"DocumentType": rec.Type, "DefaultVersion": rec.Default, "LatestVersion": rec.Latest,
					},
				})

				continue
			}

			if !strings.HasPrefix(rest, "p/") {
				continue
			}

			p, _, err := storage.Read[parameter](tx, key)
			if err != nil {
				return err
			}

			v, _, err := loadVersion(tx, scope, p.Name, p.Version)
			if err != nil {
				return err
			}

			details := map[string]any{
				keyType: v.Type, "Version": v.Version, "Tier": v.Tier, "DataType": v.DataType,
				"Description": v.Description, "Labels": p.Labels,
			}
			if v.Type != typeSecureString {
				details["Value"] = v.Value
			}

			out = append(out, catalog.Resource{
				Type: "parameter", Account: account, Region: region, Name: p.Name,
				ARN: parameterARN(scope, p.Name), Tags: p.Tags, Details: details,
			})
		}

		return nil
	})

	return out, err
}

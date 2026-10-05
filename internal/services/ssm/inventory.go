package ssm

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every parameter, for the resource browser. SecureString
// values are left out.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "ssm")
			if !ok || !strings.HasPrefix(rest, "p/") {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			p, _, err := storage.Read[parameter](tx, key)
			if err != nil {
				return err
			}

			v, _, err := loadVersion(tx, scope, p.Name, p.Version)
			if err != nil {
				return err
			}

			details := map[string]any{
				"Type": v.Type, "Version": v.Version, "Tier": v.Tier, "DataType": v.DataType,
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

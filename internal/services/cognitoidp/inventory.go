package cognitoidp

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every user pool, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok {
				continue
			}

			if strings.HasPrefix(rest, "domain/") {
				d, _, readErr := storage.Read[domain](tx, key)
				if readErr != nil {
					return readErr
				}

				out = append(out, catalog.Resource{
					Type: "user-pool-domain", Account: account, Region: region,
					Name: common.Value(d.Description.Domain), Details: d.Description,
				})
				continue
			}

			if strings.HasPrefix(rest, "provider/") {
				provider, _, readErr := storage.Read[api.IdentityProviderType](tx, key)
				if readErr != nil {
					return readErr
				}

				out = append(out, catalog.Resource{
					Type: "identity-provider", Account: account, Region: region,
					Name: common.Value(provider.ProviderName), Details: map[string]any{
						"UserPoolId": provider.UserPoolId, "ProviderType": provider.ProviderType,
						"AttributeMapping": provider.AttributeMapping, "IdpIdentifiers": provider.IdpIdentifiers,
					},
				})
				continue
			}

			if strings.HasPrefix(rest, "branding/") {
				branding, _, readErr := storage.Read[branding](tx, key)
				if readErr != nil {
					return readErr
				}

				out = append(out, catalog.Resource{
					Type: "managed-login-branding", Account: account, Region: region,
					Name: common.Value(branding.Output.ManagedLoginBrandingId), Details: map[string]any{
						"UserPoolId": branding.Output.UserPoolId, "ClientId": branding.Client,
					},
				})
				continue
			}

			if !strings.HasPrefix(rest, "pool/") {
				continue
			}

			p, _, err := storage.Read[pool](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			out = append(out, catalog.Resource{
				Type: "user-pool", Account: account, Region: region, Name: p.Name, ARN: poolARN(scope, p.ID),
				Tags: p.Tags, Details: map[string]any{
					"Id": p.ID, "Clients": len(tx.Keys(scope.Prefix(service, "client") + p.ID + "/")),
					"Users": len(tx.Keys(userPrefix(scope, p.ID))),
				},
			})
		}

		return nil
	})

	return out, err
}

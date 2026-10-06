package cognitoidentity

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every identity pool, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindPool+"/") {
				continue
			}

			p, _, err := storage.Read[pool](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			identities := len(tx.Keys(identityKey(scope, *p.Pool.IdentityPoolId, "")))
			out = append(out, catalog.Resource{
				Type: "identity-pool", Account: account, Region: region, Name: common.Value(p.Pool.IdentityPoolName),
				ARN: poolARN(scope, *p.Pool.IdentityPoolId), Tags: p.Pool.IdentityPoolTags,
				Details: map[string]any{
					"IdentityPoolId": *p.Pool.IdentityPoolId, "Identities": identities,
					"AllowUnauthenticatedIdentities": common.Value(p.Pool.AllowUnauthenticatedIdentities),
				},
			})
		}

		return nil
	})

	return out, err
}

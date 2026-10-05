package cognitoidp

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every user pool, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, "pool/") {
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
					"Id": p.ID, "Clients": len(tx.Keys(prefix(scope, "client") + p.ID + "/")),
					"Users": len(tx.Keys(userPrefix(scope, p.ID))),
				},
			})
		}

		return nil
	})

	return out, err
}

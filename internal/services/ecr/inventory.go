package ecr

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every repository, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, "repo/") {
				continue
			}

			r, _, err := read[repository](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			out = append(out, catalog.Resource{
				Type: "repository", Account: account, Region: region, Name: r.Name, ARN: repoARN(scope, r.Name),
				Tags: r.Tags, Details: map[string]any{
					"ImageTagMutability": r.Mutability, "Images": len(tx.Keys(owned(scope, "image", r.Name))),
					"Tags": len(tx.Keys(owned(scope, "tag", r.Name))),
				},
			})
		}

		return nil
	})

	return out, err
}

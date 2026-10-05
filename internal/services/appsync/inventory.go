package appsync

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every GraphQL API with its data sources, for the resource
// browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindAPI+"/") {
				continue
			}

			a, _, err := storage.Read[api.GraphqlApi](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			var sources []string
			prefix := owned(scope, kindDataSource, *a.ApiId, "")
			for _, k := range tx.Keys(prefix) {
				sources = append(sources, strings.TrimPrefix(k, prefix))
			}

			out = append(out, catalog.Resource{
				Type: "graphql-api", Account: account, Region: region, Name: common.Value(a.Name),
				ARN: common.Value(a.Arn), Tags: a.Tags,
				Details: map[string]any{
					"ApiId": *a.ApiId, "AuthenticationType": common.Value(a.AuthenticationType),
					"Endpoint": a.Uris["GRAPHQL"], "DataSources": sources,
				},
			})
		}

		return nil
	})

	return out, err
}

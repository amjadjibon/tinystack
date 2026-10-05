package apigatewayv2

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every HTTP API with its routes and stages, for the
// resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindAPI+"/") {
				continue
			}

			a, _, err := storage.Read[api.Api](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			routes, err := storage.Records[api.Route](tx, scope.Prefix(service, kindRoute)+*a.ApiId+"/")
			if err != nil {
				return err
			}

			var keys, stages []string
			for _, r := range routes {
				keys = append(keys, common.Value(r.RouteKey))
			}

			stagePrefix := scope.Prefix(service, kindStage) + *a.ApiId + "/"
			for _, k := range tx.Keys(stagePrefix) {
				stages = append(stages, strings.TrimPrefix(k, stagePrefix))
			}

			out = append(out, catalog.Resource{
				Type:    "http-api",
				Account: account,
				Region:  region,
				Name:    common.Value(a.Name),
				ARN:     apiARN(scope, *a.ApiId),
				Tags:    a.Tags,
				Details: map[string]any{
					"ApiId": *a.ApiId, "Endpoint": common.Value(a.ApiEndpoint), "Routes": keys, "Stages": stages,
				},
			})
		}

		return nil
	})

	return out, err
}

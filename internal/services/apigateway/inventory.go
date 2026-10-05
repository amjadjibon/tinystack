package apigateway

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every REST API with its methods and stages, for the
// resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindAPI+"/") {
				continue
			}

			a, _, err := storage.Read[restAPI](tx, key)
			if err != nil {
				return err
			}

			var methodKeys, stages []string
			for _, r := range a.Resources {
				for _, m := range slices.Sorted(maps.Keys(r.ResourceMethods)) {
					methodKeys = append(methodKeys, m+" "+common.Value(r.Path))
				}
			}

			slices.Sort(methodKeys)
			scope := request.Scope{AccountID: account, Region: region}
			stagePrefix := scope.Prefix(service, kindStage) + *a.API.Id + "/"
			for _, k := range tx.Keys(stagePrefix) {
				stages = append(stages, strings.TrimPrefix(k, stagePrefix))
			}

			out = append(out, catalog.Resource{
				Type: "rest-api", Account: account, Region: region, Name: common.Value(a.API.Name),
				ARN: apiARN(scope, *a.API.Id), Tags: a.API.Tags,
				Details: map[string]any{"ApiId": *a.API.Id, "Methods": methodKeys, "Stages": stages},
			})
		}

		return nil
	})

	return out, err
}

package eks

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every cluster with its node groups, Fargate profiles, and
// add-ons, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, "cluster/") {
				continue
			}

			c, _, err := storage.Read[cluster](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			details := map[string]any{"Version": c.Version, "Endpoint": c.Endpoint}
			for _, kind := range []string{kindNodegroup, kindFargate, kindAddon} {
				var names []string
				keyPrefix := prefix(scope, kind) + c.Name + "/"
				for _, k := range tx.Keys(keyPrefix) {
					names = append(names, strings.TrimPrefix(k, keyPrefix))
				}

				details[kind] = names
			}

			out = append(out, catalog.Resource{
				Type: kindCluster, Account: account, Region: region, Name: c.Name, ARN: clusterARN(scope, c.Name),
				Tags: c.Tags, Details: details,
			})
		}

		return nil
	})

	return out, err
}

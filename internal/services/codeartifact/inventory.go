package codeartifact

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored domains and repositories.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			scope := request.Scope{AccountID: account, Region: region}
			var (
				name, arn     string
				configuration any
				tags          map[string]string
			)

			switch kind {
			case kindDomain:
				r, _, e := storage.Read[domainRecord](tx, k)
				if e != nil {
					return e
				}

				name, arn, tags = r.Name, domainARN(scope, r.Name), r.Tags
				configuration = r.description(scope, repositoryCount(tx, scope, r.Name))
			case kindRepository:
				r, _, e := storage.Read[repositoryRecord](tx, k)
				if e != nil {
					return e
				}

				name, arn, tags = r.Domain+"/"+r.Name, repositoryARN(scope, r.Domain, r.Name), r.Tags
				configuration = r.description(scope)
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: name, ARN: arn,
				Tags: maps.Clone(tags),
				Details: map[string]any{
					"RecordsOnly": true, "ExecutionReady": false, "Configuration": configuration,
				},
			})
		}

		return nil
	})

	return out, err
}

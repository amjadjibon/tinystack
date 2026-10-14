package codeconnections

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored connections and hosts.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			kind, id, _ := strings.Cut(rest, "/")
			scope := request.Scope{AccountID: account, Region: region}
			var (
				name          string
				configuration any
				tags          map[string]string
			)

			switch kind {
			case kindConnection:
				r, _, e := storage.Read[connectionRecord](tx, k)
				if e != nil {
					return e
				}

				c := r.output(scope)
				name, tags, configuration = r.Name, r.Tags, c
			case kindHost:
				r, _, e := storage.Read[hostRecord](tx, k)
				if e != nil {
					return e
				}

				h := r.output(scope)
				name, tags, configuration = r.Name, r.Tags, h
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: name,
				ARN:  arn(scope, kind, id),
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

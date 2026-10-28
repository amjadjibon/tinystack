package timestreamwrite

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored databases and tables.
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
				details       = map[string]any{"RecordsOnly": false, "ExecutionReady": true}
			)

			switch kind {
			case kindDatabase:
				r, _, e := storage.Read[databaseRecord](tx, k)
				if e != nil {
					return e
				}

				name, arn, tags = r.Name, databaseARN(scope, r.Name), r.Tags
				configuration = r.output(scope, tableCount(tx, scope, r.Name))
			case kindTable:
				r, _, e := storage.Read[tableRecord](tx, k)
				if e != nil {
					return e
				}

				name, arn, tags = r.Database+"/"+r.Name, tableARN(scope, r.Database, r.Name), r.Tags
				configuration = r.output(scope)
				details["RecordsIngested"] = r.Ingested
			default:
				continue
			}

			details["Configuration"] = configuration
			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: name, ARN: arn,
				Tags: maps.Clone(tags), Details: details,
			})
		}

		return nil
	})

	return out, err
}

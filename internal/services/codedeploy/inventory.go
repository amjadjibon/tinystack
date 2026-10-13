package codedeploy

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, id, _ := strings.Cut(rest, "/")
			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			scope := request.Scope{AccountID: account, Region: region}
			arnKind := kind
			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			switch kind {
			case applicationKind:
				details["Configuration"] = r.Application
			case groupKind:
				arnKind = "deploymentgroup"
				details["Configuration"] = r.Group
			case configKind:
				arnKind = "deploymentconfig"
				details["Configuration"] = r.Config
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: id,
				ARN: resourceARN(scope, arnKind, id), Tags: maps.Clone(r.Tags), Details: details,
			})
		}

		return nil
	})
	return out, err
}

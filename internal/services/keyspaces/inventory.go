package keyspaces

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindKeyspace && kind != kindTable && kind != kindType {
				continue
			}

			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			res := catalog.Resource{Account: account, Region: region, Type: kind, Name: r.Name, Tags: r.Tags}
			var config any
			switch kind {
			case kindKeyspace:
				res.ARN, config = resourceARN(scope, r.Keyspace, ""), s.keyspaceOutput(scope, r)
			case kindTable:
				res.ARN, config = resourceARN(scope, r.Keyspace, r.Name), s.tableOutput(r)
			case kindType:
				config = s.typeOutput(scope, r)
			}

			details := map[string]any{"Configuration": config, "RecordsOnly": s.runtime == nil, "Status": s.status(r)}
			if s.runtime != nil {
				state, found, e := storage.Read[runtimeState](tx, runtimeIndexKey(scope))
				if e != nil {
					return e
				}

				if found && state.Endpoint != "" {
					details["CqlEndpoint"] = state.Endpoint
				}

				if found && state.Error != "" {
					details["RuntimeError"] = state.Error
				}
			}

			res.Details = details
			out = append(out, res)
		}

		return nil
	})
	return out, err
}

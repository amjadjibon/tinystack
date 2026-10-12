package identitystore

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	"github.com/amjadjibon/tinystack/internal/common"
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
			r := catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kind,
				ARN:     "arn:aws:identitystore:::" + kind + "/" + id,
			}

			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			switch kind {
			case "user":
				u, _, e := storage.Read[api.User](tx, key)
				if e != nil {
					return e
				}

				r.Name = *u.UserName
				details["Configuration"] = u
			case "group":
				g, _, e := storage.Read[api.Group](tx, key)
				if e != nil {
					return e
				}

				r.Name = *g.DisplayName
				details["Configuration"] = g
			case "membership":
				m, _, e := storage.Read[api.GroupMembership](tx, key)
				if e != nil {
					return e
				}

				r.Name = common.Value(m.MembershipId)
				details["Configuration"] = m
				r.Type = "group-membership"
			default:
				continue
			}

			r.Details = details
			out = append(out, r)
		}

		return nil
	})
	return out, err
}

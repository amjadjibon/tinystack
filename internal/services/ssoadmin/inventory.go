package ssoadmin

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
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

			kind, _, _ := strings.Cut(rest, "/")
			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			r := catalog.Resource{Account: account, Region: region, Type: kind, Details: details}
			instance, _ := identitystore.Instance(account)
			details["InstanceArn"] = instance
			switch kind {
			case permissionKind:
				p, _, e := storage.Read[permissionRecord](tx, key)
				if e != nil {
					return e
				}

				r.Name = *p.PermissionSet.Name
				r.ARN = *p.PermissionSet.PermissionSetArn
				r.Tags = maps.Clone(p.Tags)
				details["Configuration"] = p.PermissionSet
				details["InlinePolicy"] = p.InlinePolicy
			case assignmentKind:
				a, _, e := storage.Read[api.AccountAssignment](tx, key)
				if e != nil {
					return e
				}

				r.Name = assignmentName(a)
				details["Configuration"] = a
			default:
				continue
			}

			out = append(out, r)
		}

		return nil
	})
	return out, err
}

package amp

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists the stored workspaces and rule groups namespaces.
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
				name          string
				arn           string
				configuration any
				tags          map[string]string
			)

			switch kind {
			case kindWorkspace:
				r, _, e := storage.Read[workspaceRecord](tx, k)
				if e != nil {
					return e
				}

				name = r.WorkspaceID
				arn = workspaceArn(scope, r.WorkspaceID)
				tags = r.Tags
				configuration = r
			case kindRuleGroupsNamespace:
				r, _, e := storage.Read[ruleGroupsNamespaceRecord](tx, k)
				if e != nil {
					return e
				}

				name = r.Name
				arn = ruleGroupsNamespaceArn(scope, r.WorkspaceID, r.Name)
				tags = r.Tags
				configuration = r
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: kind, Name: name,
				ARN:  arn,
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

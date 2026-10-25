package bedrockagent

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources reports resources across all accounts and Regions.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, id, _ := strings.Cut(rest, "/")
			if kind != kindAgent && kind != kindAlias && kind != kindKnowledgeBase && kind != kindDataSource {
				continue
			}

			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			resource := catalog.Resource{
				Account: account,
				Region:  region,
				Type:    kind,
				Tags:    common.CloneTags(r.Tags),
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r},
			}

			switch kind {
			case kindAgent:
				resource.Name = common.Value(r.Agent.AgentName)
				resource.ARN = common.Value(r.Agent.AgentArn)
			case kindAlias:
				resource.Name = common.Value(r.Alias.AgentAliasName)
				resource.ARN = common.Value(r.Alias.AgentAliasArn)
			case kindKnowledgeBase:
				resource.Name = common.Value(r.KnowledgeBase.Name)
				resource.ARN = common.Value(r.KnowledgeBase.KnowledgeBaseArn)
			case kindDataSource:
				resource.Name = common.Value(r.DataSource.Name)
				resource.ARN = arn(request.Scope{AccountID: account, Region: region}, kindKnowledgeBase, id)
			}

			out = append(out, resource)
		}

		return nil
	})
	return out, err
}

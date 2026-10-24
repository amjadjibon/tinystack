package swf

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// SWF has no CloudFormation resource types, so domains appear only in native inventory.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, "domain/") {
				continue
			}

			r, _, err := storage.Read[domainRecord](tx, key)
			if err != nil {
				return err
			}

			tags := map[string]string{}
			for _, t := range r.Tags {
				tags[common.Value(t.Key)] = common.Value(t.Value)
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    "domain",
					Name:    common.Value(r.Detail.DomainInfo.Name),
					ARN:     common.Value(r.Detail.DomainInfo.Arn),
					Tags:    tags,
					Details: map[string]any{"Configuration": r.Detail, "RecordsOnly": false, "ExecutionReady": true},
				},
			)
		}

		return nil
	})
	return out, err
}

var _ catalog.Inventory = (*Service)(nil)

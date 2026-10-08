package resourcegroups

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every group, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindGroup+"/") {
				continue
			}

			g, _, err := storage.Read[group](tx, key)
			if err != nil {
				return err
			}

			details := map[string]any{"Description": g.Description}
			if g.Query != nil {
				details["QueryType"] = common.Value(g.Query.Type)
			}

			out = append(out, catalog.Resource{
				Type: kindGroup, Account: account, Region: region, Name: g.Name,
				ARN:  groupARN(request.Scope{AccountID: account, Region: region}, g.Name),
				Tags: g.Tags, Details: details,
			})
		}

		return nil
	})

	return out, err
}

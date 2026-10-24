package shield

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
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

			kind, _, _ := strings.Cut(rest, "/")
			if kind != kindProtection && kind != kindGroup {
				continue
			}

			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			name, value := "", ""
			if r.Protection != nil {
				name = common.Value(r.Protection.Name)
				value = common.Value(r.Protection.ProtectionArn)
			} else {
				name = common.Value(r.Group.ProtectionGroupId)
				value = common.Value(r.Group.ProtectionGroupArn)
			}

			out = append(
				out,
				catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kind,
					Name:    name,
					ARN:     value,
					Tags:    maps.Clone(r.Tags),
					Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": r},
				},
			)
		}

		return nil
	})
	return out, err
}

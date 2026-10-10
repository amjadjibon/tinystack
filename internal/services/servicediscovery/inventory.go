package servicediscovery

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || (!strings.HasPrefix(rest, "namespace/") && !strings.HasPrefix(rest, "service/")) {
				continue
			}

			r, _, txErr := storage.Read[record](tx, k)
			if txErr != nil {
				return txErr
			}

			if r.Kind != kindNamespace && r.Kind != kindService {
				continue
			}

			name := ""
			if r.Namespace != nil {
				name = common.Value(r.Namespace.Name)
			}

			if r.Service != nil {
				name = common.Value(r.Service.Name)
			}

			out = append(
				out,
				catalog.Resource{
					Type:    r.Kind,
					Name:    name,
					Account: account,
					Region:  region,
					ARN:     resourceARN(request.Scope{AccountID: account, Region: region}, r.Kind, r.ID),
					Tags:    r.Tags,
					Details: map[string]any{"Id": r.ID},
				},
			)
		}

		return nil
	})
	return out, err
}

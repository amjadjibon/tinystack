package translate

import (
	"context"
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
			if !ok || region == "" {
				continue
			}

			kind, name, _ := strings.Cut(rest, "/")
			resource := catalog.Resource{Account: account, Region: region, Type: kind, Name: name}
			switch kind {
			case kindTerminology:
				r, _, e := storage.Read[terminologyRecord](tx, key)
				if e != nil {
					return e
				}

				resource.ARN = common.Value(r.Properties.Arn)
				resource.Tags = r.Tags
				resource.Details = map[string]any{"RecordsOnly": true, "Configuration": r.Properties}
			case kindParallel:
				r, _, e := storage.Read[parallelRecord](tx, key)
				if e != nil {
					return e
				}

				resource.ARN = common.Value(r.Properties.Arn)
				resource.Tags = r.Tags
				resource.Details = map[string]any{"RecordsOnly": true, "Configuration": r.Properties}
			default:
				continue
			}

			out = append(out, resource)
		}

		return nil
	})
	return out, err
}

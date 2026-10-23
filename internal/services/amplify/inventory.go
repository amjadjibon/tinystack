package amplify

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists configuration records with no execution capability or credential fields.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			var config any
			switch r.Kind {
			case kindApp:
				config = r.appOutput()
			case kindBranch:
				config = r.branchOutput()
			case "domain":
				config = common.Clone(r.Domain)
			case "webhook":
				config = common.Clone(r.Webhook)
			case "backend":
				config = common.Clone(r.Backend)
			default:
				continue
			}

			out = append(out, catalog.Resource{
				Account: account,
				Region:  region,
				Type:    r.Kind,
				Name:    r.Name,
				ARN:     r.ARN,
				Tags: common.CloneTags(
					r.Tags,
				),
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "Configuration": config},
			})
		}

		return nil
	})
	return out, err
}

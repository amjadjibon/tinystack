package sesv2

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every email identity, configuration set, and template, for
// the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			kind, _, _ := strings.Cut(rest, "/")
			if !ok || kind == kindAccount {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			r := catalog.Resource{Type: kind, Account: account, Region: region}
			switch kind {
			case kindIdentity:
				i, _, err := storage.Read[identity](tx, key)
				if err != nil {
					return err
				}

				r.Name, r.Tags, r.Details = i.Name, i.Tags, map[string]any{"IdentityType": i.Type}
			case kindConfigSet:
				c, _, err := storage.Read[configSet](tx, key)
				if err != nil {
					return err
				}

				r.Name, r.Tags = *c.Settings.ConfigurationSetName, c.Tags
				r.Details = map[string]any{"EventDestinations": len(c.Destinations)}
			case kindTemplate:
				t, _, err := storage.Read[template](tx, key)
				if err != nil {
					return err
				}

				r.Name, r.Tags = t.Name, t.Tags
			default:
				continue
			}

			r.ARN = arn(scope, kind, r.Name)
			out = append(out, r)
		}

		return nil
	})

	return out, err
}

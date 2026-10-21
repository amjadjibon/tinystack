package eventbridge

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every custom event bus, every rule with its targets, and
// every archive, replay, connection, and API destination, for the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "eventbridge")
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			if name, isBus := strings.CutPrefix(rest, "bus/"); isBus {
				b, _, err := loadBus(tx, scope, name)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type:    "event-bus",
					Account: account,
					Region:  region,
					Name:    name,
					ARN:     busARN(scope, name),
					Tags:    b.Tags,
					Details: b.describe(scope, policy(tx, scope, name)),
				})
				continue
			}

			if name, isArchive := strings.CutPrefix(rest, "archive/"); isArchive {
				a, _, err := storage.Read[archive](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "archive", Account: account, Region: region, Name: name, ARN: archiveARN(scope, name),
					Details: a.list(scope),
				})
				continue
			}

			if name, isReplay := strings.CutPrefix(rest, "replay/"); isReplay {
				r, _, err := storage.Read[replay](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "replay", Account: account, Region: region, Name: name, ARN: replayARN(scope, name),
					Details: r.list(scope),
				})
				continue
			}

			if name, isConnection := strings.CutPrefix(rest, "connection/"); isConnection {
				c, _, err := storage.Read[connection](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "connection", Account: account, Region: region, Name: name, ARN: c.arn(scope),
					Details: c.list(scope),
				})
				continue
			}

			if name, isDestination := strings.CutPrefix(rest, "destination/"); isDestination {
				d, _, err := storage.Read[destination](tx, key)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "api-destination", Account: account, Region: region, Name: name, ARN: d.arn(scope),
					Details: d.list(scope),
				})
				continue
			}

			if !strings.HasPrefix(rest, "rule/") && !strings.HasPrefix(rest, "bus-rule/") {
				continue
			}

			r, _, err := storage.Read[rule](tx, key)
			if err != nil {
				return err
			}

			out = append(out, catalog.Resource{
				Type:    "rule",
				Account: account,
				Region:  region,
				Name:    r.Name,
				ARN:     ruleARN(scope, r.bus(), r.Name),
				Tags:    r.Tags,
				Details: map[string]any{"Rule": r.describe(scope), "Targets": r.Targets},
			})
		}

		return nil
	})

	return out, err
}

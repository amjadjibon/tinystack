package route53

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every hosted zone and health check, for the resource
// browser. Route 53 is global, so they have no Region.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			parts := strings.SplitN(k, "/", 4)
			if len(parts) != 4 || parts[1] != service {
				continue
			}

			account := parts[0]
			switch parts[2] {
			case kindZone:
				z, _, err := read[zone](tx, k)
				if err != nil {
					return err
				}

				id := bareID(z.Zone.Id)
				out = append(out, catalog.Resource{
					Type: "hosted-zone", Account: account, Name: value(z.Zone.Name),
					ARN: "arn:aws:route53:::hostedzone/" + id, Tags: z.Tags,
					Details: map[string]any{
						"Id": id, "Private": value(z.Zone.Config.PrivateZone),
						"RecordSets": value(z.Zone.ResourceRecordSetCount),
					},
				})
			case kindHealthCheck:
				h, _, err := read[healthCheck](tx, k)
				if err != nil {
					return err
				}

				c := h.Check.HealthCheckConfig
				out = append(out, catalog.Resource{
					Type: "health-check", Account: account, Name: value(h.Check.Id),
					ARN: "arn:aws:route53:::healthcheck/" + value(h.Check.Id), Tags: h.Tags,
					Details: map[string]any{"Type": value(c.Type), "Target": value(c.FullyQualifiedDomainName) +
						value(c.IPAddress)},
				})
			}
		}

		return nil
	})

	return out, err
}

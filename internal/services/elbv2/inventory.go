package elbv2

import (
	"context"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every load balancer and target group, for the resource
// browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, service)
			if !ok {
				continue
			}

			switch {
			case strings.HasPrefix(rest, kindLB+"/"):
				b, _, err := storage.Read[balancer](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "load-balancer", Account: account, Region: region, Name: b.name(), ARN: b.arnValue(),
					Tags: b.Tags, Details: map[string]any{
						"Type": b.kind(), "Scheme": common.Value(b.LB.Scheme), "DNSName": common.Value(b.LB.DNSName),
					},
				})
			case strings.HasPrefix(rest, kindTG+"/"):
				t, _, err := storage.Read[targetGroup](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: "target-group", Account: account, Region: region, Name: t.name(), ARN: t.arnValue(),
					Tags: t.Tags, Details: map[string]any{
						"TargetType": t.kind(), "Targets": strconv.Itoa(len(t.Targets)),
					},
				})
			}
		}

		return nil
	})

	return out, err
}

package wafv2

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || strings.HasPrefix(rest, "association/") {
				continue
			}
			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}
			details := map[string]any{
				"RecordsOnly":  true,
				"Scope":        r.Scope,
				"CreationDate": r.Created,
				"Description":  r.Description,
			}
			if r.Kind == kindIP {
				details["IPAddressVersion"] = r.Family
				details["Addresses"] = r.Addresses
			} else {
				details["Rules"] = r.Rules
				details["VisibilityConfig"] = r.Visibility
				if r.Kind == kindACL {
					details["DefaultAction"] = r.DefaultAction
				} else {
					details["Capacity"] = r.Capacity
				}
			}
			out = append(
				out,
				catalog.Resource{
					Type:    r.Kind,
					Name:    r.Name,
					ARN:     r.ARN,
					Account: account,
					Region:  region,
					Tags:    r.Tags,
					Details: details,
				},
			)
		}
		return nil
	})
	return out, err
}

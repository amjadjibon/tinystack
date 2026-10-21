package waf

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || strings.HasPrefix(rest, "token/") {
				continue
			}

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			out = append(out, catalog.Resource{
				Type: strings.ToLower(r.Kind), Name: r.Name, ARN: r.ARN, Account: account,
				Region: region, Tags: r.Tags, Details: map[string]any{"RecordsOnly": true},
			})
		}

		return nil
	})
	return out, err
}

func (s *Service) GetRateBasedRuleManagedKeys(
	ctx context.Context,
	in *api.GetRateBasedRuleManagedKeysRequest,
) (*api.GetRateBasedRuleManagedKeysResponse, error) {
	if _, err := s.get(ctx, kindRateBasedRule, in.RuleId); err != nil {
		return nil, err
	}

	if in.NextMarker != nil {
		return nil, invalid("Invalid NextMarker")
	}

	return &api.GetRateBasedRuleManagedKeysResponse{ManagedKeys: []string{}}, nil
}

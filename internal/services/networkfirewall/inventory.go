package networkfirewall

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	fieldConfiguration  = "Configuration"
	fieldExecutionReady = "ExecutionReady"
	fieldRecordsOnly    = "RecordsOnly"
)

// Resources lists configuration records with no execution capability.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}

			if fw, ok, e := storage.Read[firewallRecord](tx, k); e == nil && ok && fw.Firewall != nil {
				tags := make(map[string]string)
				for _, t := range fw.Firewall.Tags {
					if t.Key != nil {
						tags[*t.Key] = common.Value(t.Value)
					}
				}

				out = append(out, catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kindFirewall,
					Name:    common.Value(fw.Firewall.FirewallName),
					ARN:     common.Value(fw.Firewall.FirewallArn),
					Tags:    tags,
					Details: map[string]any{
						fieldRecordsOnly:    true,
						fieldExecutionReady: false,
						fieldConfiguration:  common.Clone(fw.Firewall),
					},
				})
				continue
			}

			if pol, ok, e := storage.Read[policyRecord](tx, k); e == nil && ok && pol.PolicyResponse != nil {
				tags := make(map[string]string)
				for _, t := range pol.PolicyResponse.Tags {
					if t.Key != nil {
						tags[*t.Key] = common.Value(t.Value)
					}
				}

				out = append(out, catalog.Resource{
					Account: account,
					Region:  region,
					Type:    "firewallpolicy",
					Name:    common.Value(pol.PolicyResponse.FirewallPolicyName),
					ARN:     common.Value(pol.PolicyResponse.FirewallPolicyArn),
					Tags:    tags,
					Details: map[string]any{
						fieldRecordsOnly:    true,
						fieldExecutionReady: false,
						fieldConfiguration:  common.Clone(pol.Policy),
					},
				})
				continue
			}

			if rg, ok, e := storage.Read[ruleGroupRecord](tx, k); e == nil && ok && rg.RuleGroupResponse != nil {
				tags := make(map[string]string)
				for _, t := range rg.RuleGroupResponse.Tags {
					if t.Key != nil {
						tags[*t.Key] = common.Value(t.Value)
					}
				}

				out = append(out, catalog.Resource{
					Account: account,
					Region:  region,
					Type:    kindRuleGroup,
					Name:    common.Value(rg.RuleGroupResponse.RuleGroupName),
					ARN:     common.Value(rg.RuleGroupResponse.RuleGroupArn),
					Tags:    tags,
					Details: map[string]any{
						fieldRecordsOnly:    true,
						fieldExecutionReady: false,
						fieldConfiguration:  common.Clone(rg.RuleGroup),
					},
				})
				continue
			}
		}

		return nil
	})
	return out, err
}

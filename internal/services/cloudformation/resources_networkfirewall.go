package cloudformation

import (
	"context"
	"fmt"
	"slices"
)

const (
	networkFirewallService = "networkfirewall"

	firewallType             = "AWS::NetworkFirewall::Firewall"
	firewallPolicyType       = "AWS::NetworkFirewall::FirewallPolicy"
	ruleGroupType            = "AWS::NetworkFirewall::RuleGroup"
	loggingConfigurationType = "AWS::NetworkFirewall::LoggingConfiguration"

	memberFirewallName                   = "FirewallName"
	memberFirewallArn                    = "FirewallArn"
	memberFirewallId                     = "FirewallId"
	memberFirewallPolicyArn              = "FirewallPolicyArn"
	memberFirewallPolicyName             = "FirewallPolicyName"
	memberFirewallPolicyId               = "FirewallPolicyId"
	memberFirewallPolicy                 = "FirewallPolicy"
	memberVpcId                          = "VpcId"
	memberSubnetMappings                 = "SubnetMappings"
	memberDeleteProtection               = "DeleteProtection"
	memberFirewallPolicyChangeProtection = "FirewallPolicyChangeProtection"
	memberSubnetChangeProtection         = "SubnetChangeProtection"
	memberEndpointIds                    = "EndpointIds"
	memberRuleGroupName                  = "RuleGroupName"
	memberRuleGroupArn                   = "RuleGroupArn"
	memberRuleGroupId                    = "RuleGroupId"
	memberRuleGroup                      = "RuleGroup"
	memberCapacity                       = "Capacity"
	memberUpdateToken                    = "UpdateToken"
)

func init() {
	register(ruleGroupType, &resourceType{
		name: memberRuleGroupName,
		properties: []string{
			memberRuleGroupName,
			memberType,
			memberCapacity,
			memberRuleGroup,
			memberRules,
			memberDescription,
			memberTags,
		},
		replace: []string{
			memberRuleGroupName,
			memberType,
			memberCapacity,
		},
		attributes: []string{
			memberRuleGroupArn,
			memberRuleGroupId,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p,
				memberRuleGroupName,
				memberType,
				memberCapacity,
				memberRuleGroup,
				memberRules,
				memberDescription,
			)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, networkFirewallService, "CreateRuleGroup", params)
			if err != nil {
				return "", nil, err
			}

			rgr, _ := out["RuleGroupResponse"].(map[string]any)
			arn := outText(rgr, memberRuleGroupArn)
			id := outText(rgr, memberRuleGroupId)

			return arn, map[string]any{
				memberRuleGroupArn: arn,
				memberRuleGroupId:  id,
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			desc, err := c.call(ctx, networkFirewallService, "DescribeRuleGroup", map[string]any{
				memberRuleGroupArn: r.Physical,
			})
			if err != nil {
				return nil, err
			}

			token := outText(desc, memberUpdateToken)

			params := pick(map[string]any{
				memberRuleGroupArn: r.Physical,
				memberUpdateToken:  token,
			}, p,
				memberRuleGroup,
				memberRules,
				memberDescription,
			)

			out, err := c.call(ctx, networkFirewallService, "UpdateRuleGroup", params)
			if err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{
				service: networkFirewallService,
				tag:     opTagResource,
				untag:   opUntagResource,
				target:  map[string]any{memberResourceArn: r.Physical},
				key:     memberKey,
				value:   memberValue,
			}); err != nil {
				return nil, err
			}

			rgr, _ := out["RuleGroupResponse"].(map[string]any)
			arn := outText(rgr, memberRuleGroupArn)
			id := outText(rgr, memberRuleGroupId)

			return map[string]any{
				memberRuleGroupArn: arn,
				memberRuleGroupId:  id,
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, networkFirewallService, "DeleteRuleGroup", map[string]any{
				memberRuleGroupArn: r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register(firewallPolicyType, &resourceType{
		name: memberFirewallPolicyName,
		properties: []string{
			memberFirewallPolicyName,
			memberFirewallPolicy,
			memberDescription,
			memberTags,
		},
		replace: []string{
			memberFirewallPolicyName,
		},
		attributes: []string{
			memberFirewallPolicyArn,
			memberFirewallPolicyId,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p,
				memberFirewallPolicyName,
				memberFirewallPolicy,
				memberDescription,
			)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, networkFirewallService, "CreateFirewallPolicy", params)
			if err != nil {
				return "", nil, err
			}

			fpr, _ := out["FirewallPolicyResponse"].(map[string]any)
			arn := outText(fpr, memberFirewallPolicyArn)
			id := outText(fpr, memberFirewallPolicyId)

			return arn, map[string]any{
				memberFirewallPolicyArn: arn,
				memberFirewallPolicyId:  id,
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			desc, err := c.call(ctx, networkFirewallService, "DescribeFirewallPolicy", map[string]any{
				memberFirewallPolicyArn: r.Physical,
			})
			if err != nil {
				return nil, err
			}

			token := outText(desc, memberUpdateToken)

			params := pick(map[string]any{
				memberFirewallPolicyArn: r.Physical,
				memberUpdateToken:       token,
			}, p,
				memberFirewallPolicy,
				memberDescription,
			)

			out, err := c.call(ctx, networkFirewallService, "UpdateFirewallPolicy", params)
			if err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{
				service: networkFirewallService,
				tag:     opTagResource,
				untag:   opUntagResource,
				target:  map[string]any{memberResourceArn: r.Physical},
				key:     memberKey,
				value:   memberValue,
			}); err != nil {
				return nil, err
			}

			fpr, _ := out["FirewallPolicyResponse"].(map[string]any)
			arn := outText(fpr, memberFirewallPolicyArn)
			id := outText(fpr, memberFirewallPolicyId)

			return map[string]any{
				memberFirewallPolicyArn: arn,
				memberFirewallPolicyId:  id,
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, networkFirewallService, "DeleteFirewallPolicy", map[string]any{
				memberFirewallPolicyArn: r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register(firewallType, &resourceType{
		name: memberFirewallName,
		properties: []string{
			memberFirewallName,
			memberFirewallPolicyArn,
			memberVpcId,
			memberSubnetMappings,
			memberDeleteProtection,
			memberFirewallPolicyChangeProtection,
			memberSubnetChangeProtection,
			memberDescription,
			memberTags,
		},
		replace: []string{
			memberVpcId,
		},
		attributes: []string{
			memberEndpointIds,
			memberFirewallArn,
			memberFirewallId,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p,
				memberFirewallName,
				memberFirewallPolicyArn,
				memberVpcId,
				memberSubnetMappings,
				memberDeleteProtection,
				memberFirewallPolicyChangeProtection,
				memberSubnetChangeProtection,
				memberDescription,
			)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, networkFirewallService, "CreateFirewall", params)
			if err != nil {
				return "", nil, err
			}

			fw, _ := out["Firewall"].(map[string]any)
			arn := outText(fw, memberFirewallArn)
			id := outText(fw, memberFirewallId)
			epIDs := firewallEndpointIDs(out)

			return arn, map[string]any{
				memberEndpointIds: epIDs,
				memberFirewallArn: arn,
				memberFirewallId:  id,
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			desc, err := c.call(ctx, networkFirewallService, "DescribeFirewall", map[string]any{
				memberFirewallArn: r.Physical,
			})
			if err != nil {
				return nil, err
			}

			token := outText(desc, memberUpdateToken)
			old := props(r.Properties)

			var uOut map[string]any
			if p.has(memberDescription) {
				newDesc := p.str(memberDescription)
				if newDesc != old.str(memberDescription) {
					uOut, err = c.call(ctx, networkFirewallService, "UpdateFirewallDescription", map[string]any{
						memberFirewallArn: r.Physical,
						memberUpdateToken: token,
						memberDescription: newDesc,
					})
					if err != nil {
						return nil, err
					}

					token = outText(uOut, memberUpdateToken)
				}
			}

			if p.has(memberDeleteProtection) {
				newDP := p.truth(memberDeleteProtection)
				if newDP != old.truth(memberDeleteProtection) {
					uOut, err = c.call(ctx, networkFirewallService, "UpdateFirewallDeleteProtection", map[string]any{
						memberFirewallArn:      r.Physical,
						memberUpdateToken:      token,
						memberDeleteProtection: newDP,
					})
					if err != nil {
						return nil, err
					}

					token = outText(uOut, memberUpdateToken)
				}
			}

			if p.has(memberFirewallPolicyChangeProtection) {
				newPCP := p.truth(memberFirewallPolicyChangeProtection)
				if newPCP != old.truth(memberFirewallPolicyChangeProtection) {
					uOut, err = c.call(
						ctx,
						networkFirewallService,
						"UpdateFirewallPolicyChangeProtection",
						map[string]any{
							memberFirewallArn:                    r.Physical,
							memberUpdateToken:                    token,
							memberFirewallPolicyChangeProtection: newPCP,
						},
					)
					if err != nil {
						return nil, err
					}

					token = outText(uOut, memberUpdateToken)
				}
			}

			if p.has(memberSubnetChangeProtection) {
				newSCP := p.truth(memberSubnetChangeProtection)
				if newSCP != old.truth(memberSubnetChangeProtection) {
					uOut, err = c.call(ctx, networkFirewallService, "UpdateSubnetChangeProtection", map[string]any{
						memberFirewallArn:            r.Physical,
						memberUpdateToken:            token,
						memberSubnetChangeProtection: newSCP,
					})
					if err != nil {
						return nil, err
					}

					token = outText(uOut, memberUpdateToken)
				}
			}

			if p.has(memberFirewallPolicyArn) {
				newPol := p.str(memberFirewallPolicyArn)
				if newPol != old.str(memberFirewallPolicyArn) {
					_, err = c.call(ctx, networkFirewallService, "AssociateFirewallPolicy", map[string]any{
						memberFirewallArn:       r.Physical,
						memberUpdateToken:       token,
						memberFirewallPolicyArn: newPol,
					})
					if err != nil {
						return nil, err
					}
				}
			}

			if retagErr := retag(ctx, c, r.Properties, p, &tagger{
				service: networkFirewallService,
				tag:     opTagResource,
				untag:   opUntagResource,
				target:  map[string]any{memberResourceArn: r.Physical},
				key:     memberKey,
				value:   memberValue,
			}); retagErr != nil {
				return nil, retagErr
			}

			fresh, err := c.call(ctx, networkFirewallService, "DescribeFirewall", map[string]any{
				memberFirewallArn: r.Physical,
			})
			if err != nil {
				return nil, err
			}

			fw, _ := fresh["Firewall"].(map[string]any)
			arn := outText(fw, memberFirewallArn)
			id := outText(fw, memberFirewallId)
			epIDs := firewallEndpointIDs(fresh)

			return map[string]any{
				memberEndpointIds: epIDs,
				memberFirewallArn: arn,
				memberFirewallId:  id,
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			desc, err := c.call(ctx, networkFirewallService, "DescribeFirewall", map[string]any{
				memberFirewallArn: r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			if err != nil {
				return err
			}

			fw, _ := desc["Firewall"].(map[string]any)
			dp, _ := fw[memberDeleteProtection].(bool)
			token := outText(desc, memberUpdateToken)

			if dp {
				_, err = c.call(ctx, networkFirewallService, "UpdateFirewallDeleteProtection", map[string]any{
					memberFirewallArn:      r.Physical,
					memberUpdateToken:      token,
					memberDeleteProtection: false,
				})
				if err != nil && !gone(err, "ResourceNotFoundException") {
					return err
				}
			}

			_, err = c.call(ctx, networkFirewallService, "DeleteFirewall", map[string]any{
				memberFirewallArn: r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register(loggingConfigurationType, &resourceType{
		name: memberFirewallArn,
		properties: []string{
			memberFirewallArn,
			memberFirewallName,
			"LoggingConfiguration",
		},
		replace: []string{
			memberFirewallArn,
			memberFirewallName,
		},
		attributes: []string{
			memberFirewallArn,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p,
				memberFirewallArn,
				memberFirewallName,
				"LoggingConfiguration",
			)

			out, err := c.call(ctx, networkFirewallService, "UpdateLoggingConfiguration", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, memberFirewallArn)
			return arn, map[string]any{
				memberFirewallArn: arn,
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{}, p,
				memberFirewallArn,
				memberFirewallName,
				"LoggingConfiguration",
			)

			out, err := c.call(ctx, networkFirewallService, "UpdateLoggingConfiguration", params)
			if err != nil {
				return nil, err
			}

			arn := outText(out, memberFirewallArn)
			return map[string]any{
				memberFirewallArn: arn,
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			// Clearing logging configurations
			_, err := c.call(ctx, networkFirewallService, "UpdateLoggingConfiguration", map[string]any{
				memberFirewallArn: r.Physical,
				"LoggingConfiguration": map[string]any{
					"LogDestinationConfigs": []any{},
				},
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

func firewallEndpointIDs(out map[string]any) []string {
	status, _ := out["FirewallStatus"].(map[string]any)
	if status == nil {
		return []string{}
	}

	syncStates, _ := status["SyncStates"].(map[string]any)
	if syncStates == nil {
		return []string{}
	}

	var ids []string
	for az, v := range syncStates {
		state, _ := v.(map[string]any)
		att, _ := state["Attachment"].(map[string]any)
		if att != nil {
			if id := outText(att, "EndpointId"); id != "" {
				ids = append(ids, fmt.Sprintf("%s:%s", az, id))
			}
		}
	}

	slices.Sort(ids)
	return ids
}

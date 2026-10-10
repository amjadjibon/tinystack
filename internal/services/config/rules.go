package config

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutConfigRule(ctx context.Context, in *api.PutConfigRuleRequest) (*api.PutConfigRuleOutput, error) {
	if err := checkRule(in.ConfigRule); err != nil {
		return nil, err
	}
	rule := common.Clone(in.ConfigRule)
	if rule.ConfigRuleName != nil && !validName(*rule.ConfigRuleName, 128) {
		return nil, invalid("ConfigRuleName must be 1-128 letters, digits, hyphens or underscores")
	}
	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name := common.Value(rule.ConfigRuleName)
		var old *record
		if rule.ConfigRuleArn != nil || rule.ConfigRuleId != nil {
			for _, key := range tx.Keys(scope.Prefix(serviceName, kindRule)) {
				r, _, e := storage.Read[record](tx, key)
				if e != nil {
					return e
				}
				if rule.ConfigRuleArn != nil && r.ARN == *rule.ConfigRuleArn ||
					rule.ConfigRuleId != nil && common.Value(r.Rule.ConfigRuleId) == *rule.ConfigRuleId {
					old = r
					break
				}
			}
			if old == nil {
				return missing(kindRule)
			}
			if name != "" && name != old.Name || rule.ConfigRuleArn != nil && *rule.ConfigRuleArn != old.ARN ||
				rule.ConfigRuleId != nil && *rule.ConfigRuleId != common.Value(old.Rule.ConfigRuleId) {
				return invalid("Rule identity fields must identify the same existing rule")
			}
			name = old.Name
		} else {
			if !validName(name, 128) {
				return invalid("ConfigRuleName must be 1-128 letters, digits, hyphens or underscores")
			}
			var e error
			old, _, e = storage.Read[record](tx, scope.Key(serviceName, kindRule, name))
			if e != nil {
				return e
			}
		}
		r := &record{Name: name, Kind: kindRule, Rule: rule, Tags: tags}
		if old != nil {
			r.ARN = old.ARN
			r.Tags = old.Tags
			rule.ConfigRuleId = old.Rule.ConfigRuleId
		} else {
			if len(tx.Keys(scope.Prefix(serviceName, kindRule))) >= 1000 {
				return failure("MaxNumberOfConfigRulesExceededException", "Rule limit reached")
			}
			rule.ConfigRuleId = new("config-rule-" + common.RandomID(7))
			r.ARN = resourceARN(scope, "config-rule/"+*rule.ConfigRuleId)
		}
		rule.ConfigRuleName = new(name)
		rule.ConfigRuleArn = new(r.ARN)
		rule.ConfigRuleState = new("ACTIVE")
		return save(tx, scope, r)
	})
	return &api.PutConfigRuleOutput{}, err
}

func (s *Service) DescribeConfigRules(
	ctx context.Context,
	in *api.DescribeConfigRulesRequest,
) (*api.DescribeConfigRulesResponse, error) {
	records, err := s.list(ctx, kindRule, in.ConfigRuleNames)
	if err != nil {
		return nil, err
	}
	selected, next, err := common.Page(records, func(r *record) string { return r.Name }, in.NextToken, 25,
		failure("InvalidNextTokenException", "Invalid NextToken"))
	if err != nil {
		return nil, err
	}
	out := &api.DescribeConfigRulesResponse{ConfigRules: []api.ConfigRule{}, NextToken: next}
	for _, r := range selected {
		out.ConfigRules = append(out.ConfigRules, *r.Rule)
	}
	return out, nil
}

func (s *Service) DeleteConfigRule(
	ctx context.Context,
	in *api.DeleteConfigRuleRequest,
) (*api.DeleteConfigRuleOutput, error) {
	return &api.DeleteConfigRuleOutput{}, s.remove(ctx, kindRule, common.Value(in.ConfigRuleName))
}

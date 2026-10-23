package networkfirewall

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func mergeTags(existing, newTags []api.Tag) []api.Tag {
	tagMap := make(map[string]string)
	for _, t := range existing {
		if t.Key != nil {
			tagMap[*t.Key] = common.Value(t.Value)
		}
	}

	for _, t := range newTags {
		if t.Key != nil {
			tagMap[*t.Key] = common.Value(t.Value)
		}
	}

	res := make([]api.Tag, 0, len(tagMap))
	for k, v := range tagMap {
		res = append(res, api.Tag{Key: new(k), Value: new(v)})
	}

	return res
}

func removeTags(existing []api.Tag, removeKeys []string) []api.Tag {
	toRemove := make(map[string]bool, len(removeKeys))
	for _, k := range removeKeys {
		toRemove[k] = true
	}

	var res []api.Tag
	for _, t := range existing {
		if t.Key != nil && !toRemove[*t.Key] {
			res = append(res, t)
		}
	}

	return res
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := *in.ResourceArn
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if fwName, rErr := resolveFirewallName(scope, nil, new(arn)); rErr == nil {
			fwKey := scope.Key(serviceName, kindFirewall, fwName)
			if rec, ok, readErr := storage.Read[firewallRecord](tx, fwKey); readErr == nil && ok {
				rec.Firewall.Tags = mergeTags(rec.Firewall.Tags, in.Tags)
				return storage.Write(tx, fwKey, rec)
			}
		}

		if polName, rErr := resolvePolicyName(scope, nil, new(arn)); rErr == nil {
			polKey := scope.Key(serviceName, kindPolicy, polName)
			if rec, ok, readErr := storage.Read[policyRecord](tx, polKey); readErr == nil && ok {
				rec.PolicyResponse.Tags = mergeTags(rec.PolicyResponse.Tags, in.Tags)
				return storage.Write(tx, polKey, rec)
			}
		}

		if rgName, rErr := resolveRuleGroupName(scope, nil, new(arn)); rErr == nil {
			rgKey := scope.Key(serviceName, kindRuleGroup, rgName)
			if rec, ok, readErr := storage.Read[ruleGroupRecord](tx, rgKey); readErr == nil && ok {
				rec.RuleGroupResponse.Tags = mergeTags(rec.RuleGroupResponse.Tags, in.Tags)
				return storage.Write(tx, rgKey, rec)
			}
		}

		return awserror.New("ResourceNotFoundException", fmt.Sprintf("Resource not found: %s", arn))
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := *in.ResourceArn
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if fwName, rErr := resolveFirewallName(scope, nil, new(arn)); rErr == nil {
			fwKey := scope.Key(serviceName, kindFirewall, fwName)
			if rec, ok, readErr := storage.Read[firewallRecord](tx, fwKey); readErr == nil && ok {
				rec.Firewall.Tags = removeTags(rec.Firewall.Tags, in.TagKeys)
				return storage.Write(tx, fwKey, rec)
			}
		}

		if polName, rErr := resolvePolicyName(scope, nil, new(arn)); rErr == nil {
			polKey := scope.Key(serviceName, kindPolicy, polName)
			if rec, ok, readErr := storage.Read[policyRecord](tx, polKey); readErr == nil && ok {
				rec.PolicyResponse.Tags = removeTags(rec.PolicyResponse.Tags, in.TagKeys)
				return storage.Write(tx, polKey, rec)
			}
		}

		if rgName, rErr := resolveRuleGroupName(scope, nil, new(arn)); rErr == nil {
			rgKey := scope.Key(serviceName, kindRuleGroup, rgName)
			if rec, ok, readErr := storage.Read[ruleGroupRecord](tx, rgKey); readErr == nil && ok {
				rec.RuleGroupResponse.Tags = removeTags(rec.RuleGroupResponse.Tags, in.TagKeys)
				return storage.Write(tx, rgKey, rec)
			}
		}

		return awserror.New("ResourceNotFoundException", fmt.Sprintf("Resource not found: %s", arn))
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := *in.ResourceArn
	var tags []api.Tag
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if fwName, rErr := resolveFirewallName(scope, nil, new(arn)); rErr == nil {
			fwKey := scope.Key(serviceName, kindFirewall, fwName)
			if rec, ok, readErr := storage.Read[firewallRecord](tx, fwKey); readErr == nil && ok {
				tags = rec.Firewall.Tags
				return nil
			}
		}

		if polName, rErr := resolvePolicyName(scope, nil, new(arn)); rErr == nil {
			polKey := scope.Key(serviceName, kindPolicy, polName)
			if rec, ok, readErr := storage.Read[policyRecord](tx, polKey); readErr == nil && ok {
				tags = rec.PolicyResponse.Tags
				return nil
			}
		}

		if rgName, rErr := resolveRuleGroupName(scope, nil, new(arn)); rErr == nil {
			rgKey := scope.Key(serviceName, kindRuleGroup, rgName)
			if rec, ok, readErr := storage.Read[ruleGroupRecord](tx, rgKey); readErr == nil && ok {
				tags = rec.RuleGroupResponse.Tags
				return nil
			}
		}

		return awserror.New("ResourceNotFoundException", fmt.Sprintf("Resource not found: %s", arn))
	})
	if err != nil {
		return nil, err
	}

	limit := 100
	if in.MaxResults != nil && *in.MaxResults > 0 {
		limit = int(*in.MaxResults)
	}

	paged, next, err := common.Page(tags, func(t api.Tag) string {
		return common.Value(t.Key)
	}, in.NextToken, limit, awserror.New("InvalidRequestException", "Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags:      paged,
		NextToken: next,
	}, nil
}

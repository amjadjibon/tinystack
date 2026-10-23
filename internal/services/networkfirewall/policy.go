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

func validateFirewallPolicy(policy *api.FirewallPolicy) error {
	if policy == nil {
		return awserror.New("InvalidRequestException", "FirewallPolicy is required")
	}

	if len(policy.StatelessDefaultActions) == 0 {
		return awserror.New("InvalidRequestException", "StatelessDefaultActions cannot be empty")
	}

	if len(policy.StatelessFragmentDefaultActions) == 0 {
		return awserror.New("InvalidRequestException", "StatelessFragmentDefaultActions cannot be empty")
	}

	return nil
}

func checkRuleGroupReferences(tx storage.ReadTx, scope request.Scope, policy *api.FirewallPolicy) error {
	if policy == nil {
		return nil
	}

	checkARN := func(arn string) error {
		// Verify rule group exists
		rgName, err := resolveRuleGroupName(scope, nil, new(arn))
		if err != nil {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Rule group %s not found", arn))
		}

		rgKey := scope.Key(serviceName, kindRuleGroup, rgName)
		if !keyExists(tx, rgKey) {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Rule group %s not found", arn))
		}

		return nil
	}

	for _, ref := range policy.StatelessRuleGroupReferences {
		if ref.ResourceArn != nil && *ref.ResourceArn != "" {
			if err := checkARN(*ref.ResourceArn); err != nil {
				return err
			}
		}
	}

	for _, ref := range policy.StatefulRuleGroupReferences {
		if ref.ResourceArn != nil && *ref.ResourceArn != "" {
			if err := checkARN(*ref.ResourceArn); err != nil {
				return err
			}
		}
	}

	return nil
}

func collectRuleGroupARNs(policy *api.FirewallPolicy) []string {
	if policy == nil {
		return nil
	}

	var arns []string
	for _, ref := range policy.StatelessRuleGroupReferences {
		if ref.ResourceArn != nil && *ref.ResourceArn != "" {
			arns = append(arns, *ref.ResourceArn)
		}
	}

	for _, ref := range policy.StatefulRuleGroupReferences {
		if ref.ResourceArn != nil && *ref.ResourceArn != "" {
			arns = append(arns, *ref.ResourceArn)
		}
	}

	return arns
}

func updateRuleGroupAssociations(tx storage.WriteTx, scope request.Scope, oldARNs, newARNs []string) error {
	delta := make(map[string]int)
	for _, a := range oldARNs {
		delta[a]--
	}

	for _, a := range newARNs {
		delta[a]++
	}

	for arn, diff := range delta {
		if diff == 0 {
			continue
		}

		rgName, err := resolveRuleGroupName(scope, nil, new(arn))
		if err != nil {
			continue
		}

		rgKey := scope.Key(serviceName, kindRuleGroup, rgName)
		rec, ok, rErr := storage.Read[ruleGroupRecord](tx, rgKey)
		if rErr != nil || !ok {
			continue
		}

		rec.NumberOfAssociations += int64(diff)
		if rec.NumberOfAssociations < 0 {
			rec.NumberOfAssociations = 0
		}

		if rec.RuleGroupResponse != nil {
			rec.RuleGroupResponse.NumberOfAssociations = new(rec.NumberOfAssociations)
		}

		if wErr := storage.Write(tx, rgKey, rec); wErr != nil {
			return wErr
		}
	}

	return nil
}

func (s *Service) CreateFirewallPolicy(
	ctx context.Context,
	in *api.CreateFirewallPolicyRequest,
) (*api.CreateFirewallPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if vErr := validateFirewallPolicy(in.FirewallPolicy); vErr != nil {
		return nil, vErr
	}

	name := *in.FirewallPolicyName
	arn := firewallPolicyARN(scope, name)
	key := scope.Key(serviceName, kindPolicy, name)

	policyID := newUUID()
	updateToken := newUUID()
	respMeta := &api.FirewallPolicyResponse{
		FirewallPolicyName:      new(name),
		FirewallPolicyArn:       new(arn),
		FirewallPolicyId:        new(policyID),
		FirewallPolicyStatus:    new(statusReady),
		Description:             in.Description,
		Tags:                    in.Tags,
		EncryptionConfiguration: in.EncryptionConfiguration,
		LastModifiedTime:        now(),
		NumberOfAssociations:    new(int64(0)),
	}

	var resp *api.CreateFirewallPolicyResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if keyExists(tx, key) {
			return awserror.New(
				"InvalidRequestException",
				fmt.Sprintf("A firewall policy with name %s already exists", name),
			)
		}

		if cErr := checkRuleGroupReferences(tx, scope, in.FirewallPolicy); cErr != nil {
			return cErr
		}

		resp = &api.CreateFirewallPolicyResponse{
			FirewallPolicyResponse: common.Clone(respMeta),
			UpdateToken:            new(updateToken),
		}

		if in.DryRun != nil && *in.DryRun {
			return nil
		}

		rec := policyRecord{
			PolicyResponse:       respMeta,
			Policy:               in.FirewallPolicy,
			UpdateToken:          updateToken,
			NumberOfAssociations: 0,
		}

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		newARNs := collectRuleGroupARNs(in.FirewallPolicy)
		return updateRuleGroupAssociations(tx, scope, nil, newARNs)
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DescribeFirewallPolicy(
	ctx context.Context,
	in *api.DescribeFirewallPolicyRequest,
) (*api.DescribeFirewallPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolvePolicyName(scope, in.FirewallPolicyName, in.FirewallPolicyArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindPolicy, name)
	var resp *api.DescribeFirewallPolicyResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, ok, rErr := storage.Read[policyRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall policy %s not found", name))
		}

		resp = &api.DescribeFirewallPolicyResponse{
			FirewallPolicyResponse: common.Clone(rec.PolicyResponse),
			FirewallPolicy:         common.Clone(rec.Policy),
			UpdateToken:            new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) ListFirewallPolicies(
	ctx context.Context,
	in *api.ListFirewallPoliciesRequest,
) (*api.ListFirewallPoliciesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := scope.Key(serviceName, kindPolicy, "")
	var policies []api.FirewallPolicyMetadata
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			rec, ok, rErr := storage.Read[policyRecord](tx, k)
			if rErr != nil || !ok || rec.PolicyResponse == nil {
				continue
			}

			policies = append(policies, api.FirewallPolicyMetadata{
				Name: new(*rec.PolicyResponse.FirewallPolicyName),
				Arn:  new(*rec.PolicyResponse.FirewallPolicyArn),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	limit := 100
	if in.MaxResults != nil && *in.MaxResults > 0 {
		limit = int(*in.MaxResults)
	}

	paged, next, err := common.Page(policies, func(p api.FirewallPolicyMetadata) string {
		return common.Value(p.Name)
	}, in.NextToken, limit, awserror.New("InvalidRequestException", "Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	return &api.ListFirewallPoliciesResponse{
		FirewallPolicies: paged,
		NextToken:        next,
	}, nil
}

func (s *Service) UpdateFirewallPolicy(
	ctx context.Context,
	in *api.UpdateFirewallPolicyRequest,
) (*api.UpdateFirewallPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolvePolicyName(scope, in.FirewallPolicyName, in.FirewallPolicyArn)
	if err != nil {
		return nil, err
	}

	if vErr := validateFirewallPolicy(in.FirewallPolicy); vErr != nil {
		return nil, vErr
	}

	key := scope.Key(serviceName, kindPolicy, name)
	var resp *api.UpdateFirewallPolicyResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[policyRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall policy %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		if cErr := checkRuleGroupReferences(tx, scope, in.FirewallPolicy); cErr != nil {
			return cErr
		}

		nextToken := newUUID()
		updatedResp := common.Clone(rec.PolicyResponse)
		if in.Description != nil {
			updatedResp.Description = in.Description
		}

		if in.EncryptionConfiguration != nil {
			updatedResp.EncryptionConfiguration = in.EncryptionConfiguration
		}

		updatedResp.LastModifiedTime = now()

		resp = &api.UpdateFirewallPolicyResponse{
			FirewallPolicyResponse: common.Clone(updatedResp),
			UpdateToken:            new(nextToken),
		}

		if in.DryRun != nil && *in.DryRun {
			return nil
		}

		oldARNs := collectRuleGroupARNs(rec.Policy)
		newARNs := collectRuleGroupARNs(in.FirewallPolicy)

		rec.Policy = in.FirewallPolicy
		rec.PolicyResponse = updatedResp
		rec.UpdateToken = nextToken

		if wErr := storage.Write(tx, key, rec); wErr != nil {
			return wErr
		}

		return updateRuleGroupAssociations(tx, scope, oldARNs, newARNs)
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DeleteFirewallPolicy(
	ctx context.Context,
	in *api.DeleteFirewallPolicyRequest,
) (*api.DeleteFirewallPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolvePolicyName(scope, in.FirewallPolicyName, in.FirewallPolicyArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindPolicy, name)
	var resp *api.DeleteFirewallPolicyResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[policyRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall policy %s not found", name))
		}

		if rec.NumberOfAssociations > 0 {
			return awserror.New(
				"InvalidOperationException",
				"Cannot delete firewall policy that is associated with a firewall",
			)
		}

		oldARNs := collectRuleGroupARNs(rec.Policy)
		if uErr := updateRuleGroupAssociations(tx, scope, oldARNs, nil); uErr != nil {
			return uErr
		}

		// Delete any attached resource policy
		arn := firewallPolicyARN(scope, name)
		resPolicyKey := scope.Key(serviceName, kindResourcePolicy, arn)
		tx.Delete(resPolicyKey)

		tx.Delete(key)

		resp = &api.DeleteFirewallPolicyResponse{
			FirewallPolicyResponse: common.Clone(rec.PolicyResponse),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

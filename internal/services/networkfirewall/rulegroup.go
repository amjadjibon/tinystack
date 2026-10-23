package networkfirewall

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func validateRulesSource(source *api.RulesSource) error {
	if source == nil {
		return awserror.New("InvalidRequestException", "RulesSource is required")
	}

	count := 0
	if source.RulesString != nil && *source.RulesString != "" {
		count++
	}

	if source.RulesSourceList != nil {
		count++
	}

	if len(source.StatefulRules) > 0 {
		count++
	}

	if source.StatelessRulesAndCustomActions != nil {
		count++
	}

	if count != 1 {
		return awserror.New(
			"InvalidRequestException",
			"RulesSource must specify exactly one of RulesString, RulesSourceList, "+
				"StatefulRules, or StatelessRulesAndCustomActions",
		)
	}

	return nil
}

func (s *Service) CreateRuleGroup(
	ctx context.Context,
	in *api.CreateRuleGroupRequest,
) (*api.CreateRuleGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	ruleType := strings.ToUpper(*in.Type)
	var rulesSourceString *string
	rg := in.RuleGroup
	switch {
	case in.Rules != nil && *in.Rules != "":
		rulesSourceString = in.Rules
		if rg == nil {
			rg = &api.RuleGroup{
				RulesSource: &api.RulesSource{
					RulesString: in.Rules,
				},
			}
		}
	case rg != nil && rg.RulesSource != nil:
		if vErr := validateRulesSource(rg.RulesSource); vErr != nil {
			return nil, vErr
		}

		rulesSourceString = rg.RulesSource.RulesString
	default:
		return nil, awserror.New("InvalidRequestException", "Either RuleGroup or Rules must be specified")
	}

	name := *in.RuleGroupName
	arn := ruleGroupARN(scope, ruleType, name)
	key := scope.Key(serviceName, kindRuleGroup, name)

	ruleGroupID := newUUID()
	updateToken := newUUID()
	respMeta := &api.RuleGroupResponse{
		RuleGroupName:           new(name),
		RuleGroupArn:            new(arn),
		RuleGroupId:             new(ruleGroupID),
		RuleGroupStatus:         new(statusReady),
		Type:                    new(ruleType),
		Capacity:                in.Capacity,
		Description:             in.Description,
		Tags:                    in.Tags,
		EncryptionConfiguration: in.EncryptionConfiguration,
		LastModifiedTime:        now(),
		NumberOfAssociations:    new(int64(0)),
	}

	var resp *api.CreateRuleGroupResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if keyExists(tx, key) {
			return awserror.New(
				"InvalidRequestException",
				fmt.Sprintf("A rule group with name %s already exists", name),
			)
		}

		resp = &api.CreateRuleGroupResponse{
			RuleGroupResponse: common.Clone(respMeta),
			UpdateToken:       new(updateToken),
		}

		if in.DryRun != nil && *in.DryRun {
			return nil
		}

		rec := ruleGroupRecord{
			RuleGroupResponse:    respMeta,
			RuleGroup:            rg,
			RulesString:          rulesSourceString,
			UpdateToken:          updateToken,
			NumberOfAssociations: 0,
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DescribeRuleGroup(
	ctx context.Context,
	in *api.DescribeRuleGroupRequest,
) (*api.DescribeRuleGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveRuleGroupName(scope, in.RuleGroupName, in.RuleGroupArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindRuleGroup, name)
	var resp *api.DescribeRuleGroupResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, ok, rErr := storage.Read[ruleGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Rule group %s not found", name))
		}

		resp = &api.DescribeRuleGroupResponse{
			RuleGroupResponse: common.Clone(rec.RuleGroupResponse),
			RuleGroup:         common.Clone(rec.RuleGroup),
			UpdateToken:       new(rec.UpdateToken),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DescribeRuleGroupMetadata(
	ctx context.Context,
	in *api.DescribeRuleGroupMetadataRequest,
) (*api.DescribeRuleGroupMetadataResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveRuleGroupName(scope, in.RuleGroupName, in.RuleGroupArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindRuleGroup, name)
	var resp *api.DescribeRuleGroupMetadataResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, ok, rErr := storage.Read[ruleGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Rule group %s not found", name))
		}

		meta := rec.RuleGroupResponse
		resp = &api.DescribeRuleGroupMetadataResponse{
			RuleGroupName:       meta.RuleGroupName,
			RuleGroupArn:        meta.RuleGroupArn,
			Description:         meta.Description,
			Type:                meta.Type,
			Capacity:            meta.Capacity,
			StatefulRuleOptions: nil,
			LastModifiedTime:    meta.LastModifiedTime,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) ListRuleGroups(
	ctx context.Context,
	in *api.ListRuleGroupsRequest,
) (*api.ListRuleGroupsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := scope.Key(serviceName, kindRuleGroup, "")
	var groups []api.RuleGroupMetadata
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			rec, ok, rErr := storage.Read[ruleGroupRecord](tx, k)
			if rErr != nil || !ok || rec.RuleGroupResponse == nil {
				continue
			}

			if in.Type != nil && *in.Type != "" {
				if rec.RuleGroupResponse.Type == nil || !strings.EqualFold(*in.Type, *rec.RuleGroupResponse.Type) {
					continue
				}
			}

			groups = append(groups, api.RuleGroupMetadata{
				Name: new(*rec.RuleGroupResponse.RuleGroupName),
				Arn:  new(*rec.RuleGroupResponse.RuleGroupArn),
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

	paged, next, err := common.Page(groups, func(g api.RuleGroupMetadata) string {
		return common.Value(g.Name)
	}, in.NextToken, limit, awserror.New("InvalidRequestException", "Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	return &api.ListRuleGroupsResponse{
		RuleGroups: paged,
		NextToken:  next,
	}, nil
}

func (s *Service) UpdateRuleGroup(
	ctx context.Context,
	in *api.UpdateRuleGroupRequest,
) (*api.UpdateRuleGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveRuleGroupName(scope, in.RuleGroupName, in.RuleGroupArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindRuleGroup, name)
	var resp *api.UpdateRuleGroupResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[ruleGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Rule group %s not found", name))
		}

		if tErr := checkUpdateToken(in.UpdateToken, rec.UpdateToken); tErr != nil {
			return tErr
		}

		if in.Type != nil && *in.Type != "" && rec.RuleGroupResponse.Type != nil {
			if !strings.EqualFold(*in.Type, *rec.RuleGroupResponse.Type) {
				return awserror.New("InvalidRequestException", "Rule group Type is immutable")
			}
		}

		rg := in.RuleGroup
		var rulesSourceString *string
		if in.Rules != nil && *in.Rules != "" {
			rulesSourceString = in.Rules
			if rg == nil {
				rg = &api.RuleGroup{
					RulesSource: &api.RulesSource{
						RulesString: in.Rules,
					},
				}
			}
		} else if rg != nil && rg.RulesSource != nil {
			if vErr := validateRulesSource(rg.RulesSource); vErr != nil {
				return vErr
			}

			rulesSourceString = rg.RulesSource.RulesString
		}

		nextToken := newUUID()
		updatedResp := common.Clone(rec.RuleGroupResponse)
		if in.Description != nil {
			updatedResp.Description = in.Description
		}

		if in.EncryptionConfiguration != nil {
			updatedResp.EncryptionConfiguration = in.EncryptionConfiguration
		}

		updatedResp.LastModifiedTime = now()

		resp = &api.UpdateRuleGroupResponse{
			RuleGroupResponse: common.Clone(updatedResp),
			UpdateToken:       new(nextToken),
		}

		if in.DryRun != nil && *in.DryRun {
			return nil
		}

		if rg != nil {
			rec.RuleGroup = rg
		}

		if rulesSourceString != nil {
			rec.RulesString = rulesSourceString
		}

		rec.RuleGroupResponse = updatedResp
		rec.UpdateToken = nextToken

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DeleteRuleGroup(
	ctx context.Context,
	in *api.DeleteRuleGroupRequest,
) (*api.DeleteRuleGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveRuleGroupName(scope, in.RuleGroupName, in.RuleGroupArn)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindRuleGroup, name)
	var resp *api.DeleteRuleGroupResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, ok, rErr := storage.Read[ruleGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Rule group %s not found", name))
		}

		if rec.NumberOfAssociations > 0 {
			return awserror.New(
				"InvalidOperationException",
				"Cannot delete rule group that is referenced by a firewall policy",
			)
		}

		// Delete any attached resource policy
		ruleType := common.Value(rec.RuleGroupResponse.Type)
		arn := ruleGroupARN(scope, ruleType, name)
		resPolicyKey := scope.Key(serviceName, kindResourcePolicy, arn)
		tx.Delete(resPolicyKey)

		tx.Delete(key)

		resp = &api.DeleteRuleGroupResponse{
			RuleGroupResponse: common.Clone(rec.RuleGroupResponse),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

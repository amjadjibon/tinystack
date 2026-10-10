package wafv2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) CreateWebACL(ctx context.Context, in *api.CreateWebACLRequest) (*api.CreateWebACLResponse, error) {
	r, err := s.create(
		ctx,
		&record{
			Kind:          kindACL,
			Name:          common.Value(in.Name),
			Scope:         common.Value(in.Scope),
			Description:   in.Description,
			Rules:         in.Rules,
			Visibility:    in.VisibilityConfig,
			DefaultAction: in.DefaultAction,
		},
		in.Tags,
	)
	if err != nil {
		return nil, err
	}
	return &api.CreateWebACLResponse{Summary: summaryWebACL(r)}, nil
}

func summaryWebACL(r *record) *api.WebACLSummary {
	return &api.WebACLSummary{
		Name:        new(r.Name),
		Id:          new(r.ID),
		ARN:         new(r.ARN),
		Description: r.Description,
		LockToken:   new(r.Token),
	}
}

func (s *Service) GetWebACL(ctx context.Context, in *api.GetWebACLRequest) (*api.GetWebACLResponse, error) {
	r, err := s.get(ctx, kindACL, in.Name, in.Id, in.Scope)
	if err != nil {
		return nil, err
	}
	return &api.GetWebACLResponse{WebACL: valueWebACL(r), LockToken: new(r.Token)}, nil
}

func valueWebACL(r *record) *api.WebACL {
	return &api.WebACL{
		Name:             new(r.Name),
		Id:               new(r.ID),
		ARN:              new(r.ARN),
		Description:      r.Description,
		Rules:            r.Rules,
		VisibilityConfig: r.Visibility,
		DefaultAction:    r.DefaultAction,
	}
}

func (s *Service) UpdateWebACL(ctx context.Context, in *api.UpdateWebACLRequest) (*api.UpdateWebACLResponse, error) {
	r, err := s.update(
		ctx,
		kindACL,
		in.Name,
		in.Id,
		in.Scope,
		in.LockToken,
		&record{
			Description:   in.Description,
			Rules:         in.Rules,
			Visibility:    in.VisibilityConfig,
			DefaultAction: in.DefaultAction,
		},
	)
	if err != nil {
		return nil, err
	}
	return &api.UpdateWebACLResponse{NextLockToken: new(r.Token)}, nil
}

func (s *Service) DeleteWebACL(ctx context.Context, in *api.DeleteWebACLRequest) (*api.DeleteWebACLResponse, error) {
	err := s.remove(ctx, kindACL, in.Name, in.Id, in.Scope, in.LockToken)
	return &api.DeleteWebACLResponse{}, err
}

func (s *Service) ListWebACLs(ctx context.Context, in *api.ListWebACLsRequest) (*api.ListWebACLsResponse, error) {
	rows, next, err := s.list(ctx, kindACL, in.Scope, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}
	values := make([]api.WebACLSummary, 0, len(rows))
	for _, r := range rows {
		values = append(values, *summaryWebACL(r))
	}
	return &api.ListWebACLsResponse{WebACLs: values, NextMarker: next}, nil
}

func (s *Service) CreateIPSet(ctx context.Context, in *api.CreateIPSetRequest) (*api.CreateIPSetResponse, error) {
	r, err := s.create(
		ctx,
		&record{
			Kind:        kindIP,
			Name:        common.Value(in.Name),
			Scope:       common.Value(in.Scope),
			Description: in.Description,
			Family:      common.Value(in.IPAddressVersion),
			Addresses:   in.Addresses,
		},
		in.Tags,
	)
	if err != nil {
		return nil, err
	}
	return &api.CreateIPSetResponse{Summary: summaryIPSet(r)}, nil
}

func summaryIPSet(r *record) *api.IPSetSummary {
	return &api.IPSetSummary{
		Name:        new(r.Name),
		Id:          new(r.ID),
		ARN:         new(r.ARN),
		Description: r.Description,
		LockToken:   new(r.Token),
	}
}

func (s *Service) GetIPSet(ctx context.Context, in *api.GetIPSetRequest) (*api.GetIPSetResponse, error) {
	r, err := s.get(ctx, kindIP, in.Name, in.Id, in.Scope)
	if err != nil {
		return nil, err
	}
	return &api.GetIPSetResponse{IPSet: valueIPSet(r), LockToken: new(r.Token)}, nil
}

func valueIPSet(r *record) *api.IPSet {
	return &api.IPSet{
		Name:             new(r.Name),
		Id:               new(r.ID),
		ARN:              new(r.ARN),
		Description:      r.Description,
		IPAddressVersion: new(r.Family),
		Addresses:        r.Addresses,
	}
}

func (s *Service) UpdateIPSet(ctx context.Context, in *api.UpdateIPSetRequest) (*api.UpdateIPSetResponse, error) {
	r, err := s.update(
		ctx,
		kindIP,
		in.Name,
		in.Id,
		in.Scope,
		in.LockToken,
		&record{Description: in.Description, Addresses: in.Addresses},
	)
	if err != nil {
		return nil, err
	}
	return &api.UpdateIPSetResponse{NextLockToken: new(r.Token)}, nil
}

func (s *Service) DeleteIPSet(ctx context.Context, in *api.DeleteIPSetRequest) (*api.DeleteIPSetResponse, error) {
	err := s.remove(ctx, kindIP, in.Name, in.Id, in.Scope, in.LockToken)
	return &api.DeleteIPSetResponse{}, err
}

func (s *Service) ListIPSets(ctx context.Context, in *api.ListIPSetsRequest) (*api.ListIPSetsResponse, error) {
	rows, next, err := s.list(ctx, kindIP, in.Scope, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}
	values := make([]api.IPSetSummary, 0, len(rows))
	for _, r := range rows {
		values = append(values, *summaryIPSet(r))
	}
	return &api.ListIPSetsResponse{IPSets: values, NextMarker: next}, nil
}

func (s *Service) CreateRuleGroup(
	ctx context.Context,
	in *api.CreateRuleGroupRequest,
) (*api.CreateRuleGroupResponse, error) {
	r, err := s.create(
		ctx,
		&record{
			Kind:        kindGroup,
			Name:        common.Value(in.Name),
			Scope:       common.Value(in.Scope),
			Description: in.Description,
			Rules:       in.Rules,
			Visibility:  in.VisibilityConfig,
			Capacity:    common.Value(in.Capacity),
		},
		in.Tags,
	)
	if err != nil {
		return nil, err
	}
	return &api.CreateRuleGroupResponse{Summary: summaryRuleGroup(r)}, nil
}

func summaryRuleGroup(r *record) *api.RuleGroupSummary {
	return &api.RuleGroupSummary{
		Name:        new(r.Name),
		Id:          new(r.ID),
		ARN:         new(r.ARN),
		Description: r.Description,
		LockToken:   new(r.Token),
	}
}

func (s *Service) GetRuleGroup(ctx context.Context, in *api.GetRuleGroupRequest) (*api.GetRuleGroupResponse, error) {
	r, err := s.get(ctx, kindGroup, in.Name, in.Id, in.Scope)
	if err != nil {
		return nil, err
	}
	return &api.GetRuleGroupResponse{RuleGroup: valueRuleGroup(r), LockToken: new(r.Token)}, nil
}

func valueRuleGroup(r *record) *api.RuleGroup {
	return &api.RuleGroup{
		Name:             new(r.Name),
		Id:               new(r.ID),
		ARN:              new(r.ARN),
		Description:      r.Description,
		Rules:            r.Rules,
		VisibilityConfig: r.Visibility,
		Capacity:         new(r.Capacity),
	}
}

func (s *Service) UpdateRuleGroup(
	ctx context.Context,
	in *api.UpdateRuleGroupRequest,
) (*api.UpdateRuleGroupResponse, error) {
	r, err := s.update(
		ctx,
		kindGroup,
		in.Name,
		in.Id,
		in.Scope,
		in.LockToken,
		&record{Description: in.Description, Rules: in.Rules, Visibility: in.VisibilityConfig},
	)
	if err != nil {
		return nil, err
	}
	return &api.UpdateRuleGroupResponse{NextLockToken: new(r.Token)}, nil
}

func (s *Service) DeleteRuleGroup(
	ctx context.Context,
	in *api.DeleteRuleGroupRequest,
) (*api.DeleteRuleGroupResponse, error) {
	err := s.remove(ctx, kindGroup, in.Name, in.Id, in.Scope, in.LockToken)
	return &api.DeleteRuleGroupResponse{}, err
}

func (s *Service) ListRuleGroups(
	ctx context.Context,
	in *api.ListRuleGroupsRequest,
) (*api.ListRuleGroupsResponse, error) {
	rows, next, err := s.list(ctx, kindGroup, in.Scope, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}
	values := make([]api.RuleGroupSummary, 0, len(rows))
	for _, r := range rows {
		values = append(values, *summaryRuleGroup(r))
	}
	return &api.ListRuleGroupsResponse{RuleGroups: values, NextMarker: next}, nil
}

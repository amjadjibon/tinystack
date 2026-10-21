package waf

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/wafcommon"
)

func (s *Service) CreateIPSet(ctx context.Context, in *api.CreateIPSetRequest) (*api.CreateIPSetResponse, error) {
	r := &record{Kind: kindIPSet, Name: common.Value(in.Name), IPSet: &api.IPSet{Name: in.Name}}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.IPSet.IPSetId = new(r.ID)

	return &api.CreateIPSetResponse{IPSet: r.IPSet, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetIPSet(ctx context.Context, in *api.GetIPSetRequest) (*api.GetIPSetResponse, error) {
	r, err := s.get(ctx, kindIPSet, in.IPSetId)
	if err != nil {
		return nil, err
	}

	r.IPSet.IPSetId = new(r.ID)

	return &api.GetIPSetResponse{IPSet: r.IPSet}, nil
}

func (s *Service) DeleteIPSet(ctx context.Context, in *api.DeleteIPSetRequest) (*api.DeleteIPSetResponse, error) {
	err := s.remove(ctx, kindIPSet, in.IPSetId, in.ChangeToken)
	return &api.DeleteIPSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateIPSet(ctx context.Context, in *api.UpdateIPSetRequest) (*api.UpdateIPSetResponse, error) {
	err := s.change(ctx, kindIPSet, in.IPSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.IPSetDescriptor == nil {
				return invalid("Update member is required")
			}

			var e error
			r.IPSet.IPSetDescriptors, e = apply(r.IPSet.IPSetDescriptors, u.Action, *u.IPSetDescriptor)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateIPSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListIPSets(ctx context.Context, in *api.ListIPSetsRequest) (*api.ListIPSetsResponse, error) {
	rows, next, err := s.list(ctx, kindIPSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.IPSetSummary{}
	for _, r := range rows {
		out = append(out, api.IPSetSummary{IPSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListIPSetsResponse{IPSets: out, NextMarker: next}, nil
}

func (s *Service) CreateByteMatchSet(
	ctx context.Context,
	in *api.CreateByteMatchSetRequest,
) (*api.CreateByteMatchSetResponse, error) {
	r := &record{Kind: kindByteMatchSet, Name: common.Value(in.Name), ByteMatchSet: &api.ByteMatchSet{Name: in.Name}}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.ByteMatchSet.ByteMatchSetId = new(r.ID)

	return &api.CreateByteMatchSetResponse{ByteMatchSet: r.ByteMatchSet, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetByteMatchSet(
	ctx context.Context,
	in *api.GetByteMatchSetRequest,
) (*api.GetByteMatchSetResponse, error) {
	r, err := s.get(ctx, kindByteMatchSet, in.ByteMatchSetId)
	if err != nil {
		return nil, err
	}

	r.ByteMatchSet.ByteMatchSetId = new(r.ID)

	return &api.GetByteMatchSetResponse{ByteMatchSet: r.ByteMatchSet}, nil
}

func (s *Service) DeleteByteMatchSet(
	ctx context.Context,
	in *api.DeleteByteMatchSetRequest,
) (*api.DeleteByteMatchSetResponse, error) {
	err := s.remove(ctx, kindByteMatchSet, in.ByteMatchSetId, in.ChangeToken)
	return &api.DeleteByteMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateByteMatchSet(
	ctx context.Context,
	in *api.UpdateByteMatchSetRequest,
) (*api.UpdateByteMatchSetResponse, error) {
	err := s.change(ctx, kindByteMatchSet, in.ByteMatchSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.ByteMatchTuple == nil {
				return invalid("Update member is required")
			}

			var e error
			r.ByteMatchSet.ByteMatchTuples, e = apply(r.ByteMatchSet.ByteMatchTuples, u.Action, *u.ByteMatchTuple)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateByteMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListByteMatchSets(
	ctx context.Context,
	in *api.ListByteMatchSetsRequest,
) (*api.ListByteMatchSetsResponse, error) {
	rows, next, err := s.list(ctx, kindByteMatchSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.ByteMatchSetSummary{}
	for _, r := range rows {
		out = append(out, api.ByteMatchSetSummary{ByteMatchSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListByteMatchSetsResponse{ByteMatchSets: out, NextMarker: next}, nil
}

func (s *Service) CreateSizeConstraintSet(
	ctx context.Context,
	in *api.CreateSizeConstraintSetRequest,
) (*api.CreateSizeConstraintSetResponse, error) {
	r := &record{
		Kind:              kindSizeConstraintSet,
		Name:              common.Value(in.Name),
		SizeConstraintSet: &api.SizeConstraintSet{Name: in.Name},
	}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.SizeConstraintSet.SizeConstraintSetId = new(r.ID)

	return &api.CreateSizeConstraintSetResponse{
		SizeConstraintSet: r.SizeConstraintSet,
		ChangeToken:       in.ChangeToken,
	}, nil
}

func (s *Service) GetSizeConstraintSet(
	ctx context.Context,
	in *api.GetSizeConstraintSetRequest,
) (*api.GetSizeConstraintSetResponse, error) {
	r, err := s.get(ctx, kindSizeConstraintSet, in.SizeConstraintSetId)
	if err != nil {
		return nil, err
	}

	r.SizeConstraintSet.SizeConstraintSetId = new(r.ID)

	return &api.GetSizeConstraintSetResponse{SizeConstraintSet: r.SizeConstraintSet}, nil
}

func (s *Service) DeleteSizeConstraintSet(
	ctx context.Context,
	in *api.DeleteSizeConstraintSetRequest,
) (*api.DeleteSizeConstraintSetResponse, error) {
	err := s.remove(ctx, kindSizeConstraintSet, in.SizeConstraintSetId, in.ChangeToken)
	return &api.DeleteSizeConstraintSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateSizeConstraintSet(
	ctx context.Context,
	in *api.UpdateSizeConstraintSetRequest,
) (*api.UpdateSizeConstraintSetResponse, error) {
	err := s.change(ctx, kindSizeConstraintSet, in.SizeConstraintSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.SizeConstraint == nil {
				return invalid("Update member is required")
			}

			var e error
			r.SizeConstraintSet.SizeConstraints, e = apply(
				r.SizeConstraintSet.SizeConstraints,
				u.Action,
				*u.SizeConstraint,
			)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateSizeConstraintSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListSizeConstraintSets(
	ctx context.Context,
	in *api.ListSizeConstraintSetsRequest,
) (*api.ListSizeConstraintSetsResponse, error) {
	rows, next, err := s.list(ctx, kindSizeConstraintSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.SizeConstraintSetSummary{}
	for _, r := range rows {
		out = append(out, api.SizeConstraintSetSummary{SizeConstraintSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListSizeConstraintSetsResponse{SizeConstraintSets: out, NextMarker: next}, nil
}

func (s *Service) CreateSqlInjectionMatchSet(
	ctx context.Context,
	in *api.CreateSqlInjectionMatchSetRequest,
) (*api.CreateSqlInjectionMatchSetResponse, error) {
	r := &record{
		Kind:                 kindSqlInjectionMatchSet,
		Name:                 common.Value(in.Name),
		SqlInjectionMatchSet: &api.SqlInjectionMatchSet{Name: in.Name},
	}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.SqlInjectionMatchSet.SqlInjectionMatchSetId = new(r.ID)

	return &api.CreateSqlInjectionMatchSetResponse{
		SqlInjectionMatchSet: r.SqlInjectionMatchSet,
		ChangeToken:          in.ChangeToken,
	}, nil
}

func (s *Service) GetSqlInjectionMatchSet(
	ctx context.Context,
	in *api.GetSqlInjectionMatchSetRequest,
) (*api.GetSqlInjectionMatchSetResponse, error) {
	r, err := s.get(ctx, kindSqlInjectionMatchSet, in.SqlInjectionMatchSetId)
	if err != nil {
		return nil, err
	}

	r.SqlInjectionMatchSet.SqlInjectionMatchSetId = new(r.ID)

	return &api.GetSqlInjectionMatchSetResponse{SqlInjectionMatchSet: r.SqlInjectionMatchSet}, nil
}

func (s *Service) DeleteSqlInjectionMatchSet(
	ctx context.Context,
	in *api.DeleteSqlInjectionMatchSetRequest,
) (*api.DeleteSqlInjectionMatchSetResponse, error) {
	err := s.remove(ctx, kindSqlInjectionMatchSet, in.SqlInjectionMatchSetId, in.ChangeToken)
	return &api.DeleteSqlInjectionMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateSqlInjectionMatchSet(
	ctx context.Context,
	in *api.UpdateSqlInjectionMatchSetRequest,
) (*api.UpdateSqlInjectionMatchSetResponse, error) {
	err := s.change(ctx, kindSqlInjectionMatchSet, in.SqlInjectionMatchSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.SqlInjectionMatchTuple == nil {
				return invalid("Update member is required")
			}

			var e error
			r.SqlInjectionMatchSet.SqlInjectionMatchTuples, e = apply(
				r.SqlInjectionMatchSet.SqlInjectionMatchTuples,
				u.Action,
				*u.SqlInjectionMatchTuple,
			)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateSqlInjectionMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListSqlInjectionMatchSets(
	ctx context.Context,
	in *api.ListSqlInjectionMatchSetsRequest,
) (*api.ListSqlInjectionMatchSetsResponse, error) {
	rows, next, err := s.list(ctx, kindSqlInjectionMatchSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.SqlInjectionMatchSetSummary{}
	for _, r := range rows {
		out = append(out, api.SqlInjectionMatchSetSummary{SqlInjectionMatchSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListSqlInjectionMatchSetsResponse{SqlInjectionMatchSets: out, NextMarker: next}, nil
}

func (s *Service) CreateXssMatchSet(
	ctx context.Context,
	in *api.CreateXssMatchSetRequest,
) (*api.CreateXssMatchSetResponse, error) {
	r := &record{Kind: kindXssMatchSet, Name: common.Value(in.Name), XssMatchSet: &api.XssMatchSet{Name: in.Name}}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.XssMatchSet.XssMatchSetId = new(r.ID)

	return &api.CreateXssMatchSetResponse{XssMatchSet: r.XssMatchSet, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetXssMatchSet(
	ctx context.Context,
	in *api.GetXssMatchSetRequest,
) (*api.GetXssMatchSetResponse, error) {
	r, err := s.get(ctx, kindXssMatchSet, in.XssMatchSetId)
	if err != nil {
		return nil, err
	}

	r.XssMatchSet.XssMatchSetId = new(r.ID)

	return &api.GetXssMatchSetResponse{XssMatchSet: r.XssMatchSet}, nil
}

func (s *Service) DeleteXssMatchSet(
	ctx context.Context,
	in *api.DeleteXssMatchSetRequest,
) (*api.DeleteXssMatchSetResponse, error) {
	err := s.remove(ctx, kindXssMatchSet, in.XssMatchSetId, in.ChangeToken)
	return &api.DeleteXssMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateXssMatchSet(
	ctx context.Context,
	in *api.UpdateXssMatchSetRequest,
) (*api.UpdateXssMatchSetResponse, error) {
	err := s.change(ctx, kindXssMatchSet, in.XssMatchSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.XssMatchTuple == nil {
				return invalid("Update member is required")
			}

			var e error
			r.XssMatchSet.XssMatchTuples, e = apply(r.XssMatchSet.XssMatchTuples, u.Action, *u.XssMatchTuple)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateXssMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListXssMatchSets(
	ctx context.Context,
	in *api.ListXssMatchSetsRequest,
) (*api.ListXssMatchSetsResponse, error) {
	rows, next, err := s.list(ctx, kindXssMatchSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.XssMatchSetSummary{}
	for _, r := range rows {
		out = append(out, api.XssMatchSetSummary{XssMatchSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListXssMatchSetsResponse{XssMatchSets: out, NextMarker: next}, nil
}

func (s *Service) CreateGeoMatchSet(
	ctx context.Context,
	in *api.CreateGeoMatchSetRequest,
) (*api.CreateGeoMatchSetResponse, error) {
	r := &record{Kind: kindGeoMatchSet, Name: common.Value(in.Name), GeoMatchSet: &api.GeoMatchSet{Name: in.Name}}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.GeoMatchSet.GeoMatchSetId = new(r.ID)

	return &api.CreateGeoMatchSetResponse{GeoMatchSet: r.GeoMatchSet, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetGeoMatchSet(
	ctx context.Context,
	in *api.GetGeoMatchSetRequest,
) (*api.GetGeoMatchSetResponse, error) {
	r, err := s.get(ctx, kindGeoMatchSet, in.GeoMatchSetId)
	if err != nil {
		return nil, err
	}

	r.GeoMatchSet.GeoMatchSetId = new(r.ID)

	return &api.GetGeoMatchSetResponse{GeoMatchSet: r.GeoMatchSet}, nil
}

func (s *Service) DeleteGeoMatchSet(
	ctx context.Context,
	in *api.DeleteGeoMatchSetRequest,
) (*api.DeleteGeoMatchSetResponse, error) {
	err := s.remove(ctx, kindGeoMatchSet, in.GeoMatchSetId, in.ChangeToken)
	return &api.DeleteGeoMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateGeoMatchSet(
	ctx context.Context,
	in *api.UpdateGeoMatchSetRequest,
) (*api.UpdateGeoMatchSetResponse, error) {
	err := s.change(ctx, kindGeoMatchSet, in.GeoMatchSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.GeoMatchConstraint == nil {
				return invalid("Update member is required")
			}

			var e error
			r.GeoMatchSet.GeoMatchConstraints, e = apply(
				r.GeoMatchSet.GeoMatchConstraints,
				u.Action,
				*u.GeoMatchConstraint,
			)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateGeoMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListGeoMatchSets(
	ctx context.Context,
	in *api.ListGeoMatchSetsRequest,
) (*api.ListGeoMatchSetsResponse, error) {
	rows, next, err := s.list(ctx, kindGeoMatchSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.GeoMatchSetSummary{}
	for _, r := range rows {
		out = append(out, api.GeoMatchSetSummary{GeoMatchSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListGeoMatchSetsResponse{GeoMatchSets: out, NextMarker: next}, nil
}

func (s *Service) CreateRegexPatternSet(
	ctx context.Context,
	in *api.CreateRegexPatternSetRequest,
) (*api.CreateRegexPatternSetResponse, error) {
	r := &record{
		Kind:            kindRegexPatternSet,
		Name:            common.Value(in.Name),
		RegexPatternSet: &api.RegexPatternSet{Name: in.Name},
	}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.RegexPatternSet.RegexPatternSetId = new(r.ID)

	return &api.CreateRegexPatternSetResponse{RegexPatternSet: r.RegexPatternSet, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetRegexPatternSet(
	ctx context.Context,
	in *api.GetRegexPatternSetRequest,
) (*api.GetRegexPatternSetResponse, error) {
	r, err := s.get(ctx, kindRegexPatternSet, in.RegexPatternSetId)
	if err != nil {
		return nil, err
	}

	r.RegexPatternSet.RegexPatternSetId = new(r.ID)

	return &api.GetRegexPatternSetResponse{RegexPatternSet: r.RegexPatternSet}, nil
}

func (s *Service) DeleteRegexPatternSet(
	ctx context.Context,
	in *api.DeleteRegexPatternSetRequest,
) (*api.DeleteRegexPatternSetResponse, error) {
	err := s.remove(ctx, kindRegexPatternSet, in.RegexPatternSetId, in.ChangeToken)
	return &api.DeleteRegexPatternSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateRegexPatternSet(
	ctx context.Context,
	in *api.UpdateRegexPatternSetRequest,
) (*api.UpdateRegexPatternSetResponse, error) {
	err := s.change(ctx, kindRegexPatternSet, in.RegexPatternSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.RegexPatternString == nil {
				return invalid("Pattern is required")
			}

			var e error
			r.RegexPatternSet.RegexPatternStrings, e = apply(
				r.RegexPatternSet.RegexPatternStrings,
				u.Action,
				common.Value(u.RegexPatternString),
			)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateRegexPatternSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListRegexPatternSets(
	ctx context.Context,
	in *api.ListRegexPatternSetsRequest,
) (*api.ListRegexPatternSetsResponse, error) {
	rows, next, err := s.list(ctx, kindRegexPatternSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.RegexPatternSetSummary{}
	for _, r := range rows {
		out = append(out, api.RegexPatternSetSummary{RegexPatternSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListRegexPatternSetsResponse{RegexPatternSets: out, NextMarker: next}, nil
}

func (s *Service) CreateRegexMatchSet(
	ctx context.Context,
	in *api.CreateRegexMatchSetRequest,
) (*api.CreateRegexMatchSetResponse, error) {
	r := &record{Kind: kindRegexMatchSet, Name: common.Value(in.Name), RegexMatchSet: &api.RegexMatchSet{Name: in.Name}}

	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.RegexMatchSet.RegexMatchSetId = new(r.ID)

	return &api.CreateRegexMatchSetResponse{RegexMatchSet: r.RegexMatchSet, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetRegexMatchSet(
	ctx context.Context,
	in *api.GetRegexMatchSetRequest,
) (*api.GetRegexMatchSetResponse, error) {
	r, err := s.get(ctx, kindRegexMatchSet, in.RegexMatchSetId)
	if err != nil {
		return nil, err
	}

	r.RegexMatchSet.RegexMatchSetId = new(r.ID)

	return &api.GetRegexMatchSetResponse{RegexMatchSet: r.RegexMatchSet}, nil
}

func (s *Service) DeleteRegexMatchSet(
	ctx context.Context,
	in *api.DeleteRegexMatchSetRequest,
) (*api.DeleteRegexMatchSetResponse, error) {
	err := s.remove(ctx, kindRegexMatchSet, in.RegexMatchSetId, in.ChangeToken)
	return &api.DeleteRegexMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateRegexMatchSet(
	ctx context.Context,
	in *api.UpdateRegexMatchSetRequest,
) (*api.UpdateRegexMatchSetResponse, error) {
	err := s.change(ctx, kindRegexMatchSet, in.RegexMatchSetId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.RegexMatchTuple == nil {
				return invalid("Update member is required")
			}

			var e error
			r.RegexMatchSet.RegexMatchTuples, e = apply(r.RegexMatchSet.RegexMatchTuples, u.Action, *u.RegexMatchTuple)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateRegexMatchSetResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListRegexMatchSets(
	ctx context.Context,
	in *api.ListRegexMatchSetsRequest,
) (*api.ListRegexMatchSetsResponse, error) {
	rows, next, err := s.list(ctx, kindRegexMatchSet, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.RegexMatchSetSummary{}
	for _, r := range rows {
		out = append(out, api.RegexMatchSetSummary{RegexMatchSetId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListRegexMatchSetsResponse{RegexMatchSets: out, NextMarker: next}, nil
}

func (s *Service) CreateRule(ctx context.Context, in *api.CreateRuleRequest) (*api.CreateRuleResponse, error) {
	r := &record{Kind: kindRule, Name: common.Value(in.Name), Rule: &api.Rule{Name: in.Name, MetricName: in.MetricName}}
	tags, err := wafcommon.Tags(in.Tags, false, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
	if err != nil {
		return nil, err
	}

	r.Tags = tags
	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.Rule.RuleId = new(r.ID)

	return &api.CreateRuleResponse{Rule: r.Rule, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetRule(ctx context.Context, in *api.GetRuleRequest) (*api.GetRuleResponse, error) {
	r, err := s.get(ctx, kindRule, in.RuleId)
	if err != nil {
		return nil, err
	}

	r.Rule.RuleId = new(r.ID)

	return &api.GetRuleResponse{Rule: r.Rule}, nil
}

func (s *Service) DeleteRule(ctx context.Context, in *api.DeleteRuleRequest) (*api.DeleteRuleResponse, error) {
	err := s.remove(ctx, kindRule, in.RuleId, in.ChangeToken)
	return &api.DeleteRuleResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateRule(ctx context.Context, in *api.UpdateRuleRequest) (*api.UpdateRuleResponse, error) {
	err := s.change(ctx, kindRule, in.RuleId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.Predicate == nil {
				return invalid("Update member is required")
			}

			var e error
			r.Rule.Predicates, e = apply(r.Rule.Predicates, u.Action, *u.Predicate)
			if e != nil {
				return e
			}
		}

		return nil
	})
	return &api.UpdateRuleResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListRules(ctx context.Context, in *api.ListRulesRequest) (*api.ListRulesResponse, error) {
	rows, next, err := s.list(ctx, kindRule, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.RuleSummary{}
	for _, r := range rows {
		out = append(out, api.RuleSummary{RuleId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListRulesResponse{Rules: out, NextMarker: next}, nil
}

func (s *Service) CreateRateBasedRule(
	ctx context.Context,
	in *api.CreateRateBasedRuleRequest,
) (*api.CreateRateBasedRuleResponse, error) {
	r := &record{
		Kind: kindRateBasedRule,
		Name: common.Value(in.Name),
		RateBasedRule: &api.RateBasedRule{
			Name:       in.Name,
			MetricName: in.MetricName,
			RateKey:    in.RateKey,
			RateLimit:  in.RateLimit,
		},
	}

	tags, err := wafcommon.Tags(in.Tags, false, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
	if err != nil {
		return nil, err
	}

	r.Tags = tags
	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.RateBasedRule.RuleId = new(r.ID)

	return &api.CreateRateBasedRuleResponse{Rule: r.RateBasedRule, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetRateBasedRule(
	ctx context.Context,
	in *api.GetRateBasedRuleRequest,
) (*api.GetRateBasedRuleResponse, error) {
	r, err := s.get(ctx, kindRateBasedRule, in.RuleId)
	if err != nil {
		return nil, err
	}

	r.RateBasedRule.RuleId = new(r.ID)

	return &api.GetRateBasedRuleResponse{Rule: r.RateBasedRule}, nil
}

func (s *Service) DeleteRateBasedRule(
	ctx context.Context,
	in *api.DeleteRateBasedRuleRequest,
) (*api.DeleteRateBasedRuleResponse, error) {
	err := s.remove(ctx, kindRateBasedRule, in.RuleId, in.ChangeToken)
	return &api.DeleteRateBasedRuleResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateRateBasedRule(
	ctx context.Context,
	in *api.UpdateRateBasedRuleRequest,
) (*api.UpdateRateBasedRuleResponse, error) {
	err := s.change(ctx, kindRateBasedRule, in.RuleId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.Predicate == nil {
				return invalid("Update member is required")
			}

			var e error
			r.RateBasedRule.MatchPredicates, e = apply(r.RateBasedRule.MatchPredicates, u.Action, *u.Predicate)
			if e != nil {
				return e
			}
		}

		r.RateBasedRule.RateLimit = in.RateLimit
		return nil
	})
	return &api.UpdateRateBasedRuleResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListRateBasedRules(
	ctx context.Context,
	in *api.ListRateBasedRulesRequest,
) (*api.ListRateBasedRulesResponse, error) {
	rows, next, err := s.list(ctx, kindRateBasedRule, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.RuleSummary{}
	for _, r := range rows {
		out = append(out, api.RuleSummary{RuleId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListRateBasedRulesResponse{Rules: out, NextMarker: next}, nil
}

func (s *Service) CreateWebACL(ctx context.Context, in *api.CreateWebACLRequest) (*api.CreateWebACLResponse, error) {
	r := &record{
		Kind:   kindWebACL,
		Name:   common.Value(in.Name),
		WebACL: &api.WebACL{Name: in.Name, MetricName: in.MetricName, DefaultAction: in.DefaultAction},
	}

	tags, err := wafcommon.Tags(in.Tags, false, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
	if err != nil {
		return nil, err
	}

	r.Tags = tags
	if err := s.create(ctx, r, in.ChangeToken); err != nil {
		return nil, err
	}

	r.WebACL.WebACLId = new(r.ID)

	r.WebACL.WebACLArn = new(r.ARN)
	return &api.CreateWebACLResponse{WebACL: r.WebACL, ChangeToken: in.ChangeToken}, nil
}

func (s *Service) GetWebACL(ctx context.Context, in *api.GetWebACLRequest) (*api.GetWebACLResponse, error) {
	r, err := s.get(ctx, kindWebACL, in.WebACLId)
	if err != nil {
		return nil, err
	}

	r.WebACL.WebACLId = new(r.ID)

	r.WebACL.WebACLArn = new(r.ARN)
	return &api.GetWebACLResponse{WebACL: r.WebACL}, nil
}

func (s *Service) DeleteWebACL(ctx context.Context, in *api.DeleteWebACLRequest) (*api.DeleteWebACLResponse, error) {
	err := s.remove(ctx, kindWebACL, in.WebACLId, in.ChangeToken)
	return &api.DeleteWebACLResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) UpdateWebACL(ctx context.Context, in *api.UpdateWebACLRequest) (*api.UpdateWebACLResponse, error) {
	err := s.change(ctx, kindWebACL, in.WebACLId, in.ChangeToken, func(r *record) error {
		for _, u := range in.Updates {
			if u.ActivatedRule == nil {
				return invalid("Update member is required")
			}

			var e error
			r.WebACL.Rules, e = apply(r.WebACL.Rules, u.Action, *u.ActivatedRule)
			if e != nil {
				return e
			}
		}

		if in.DefaultAction != nil {
			r.WebACL.DefaultAction = in.DefaultAction
		}

		return nil
	})
	return &api.UpdateWebACLResponse{ChangeToken: in.ChangeToken}, err
}

func (s *Service) ListWebACLs(ctx context.Context, in *api.ListWebACLsRequest) (*api.ListWebACLsResponse, error) {
	rows, next, err := s.list(ctx, kindWebACL, in.NextMarker, in.Limit)
	if err != nil {
		return nil, err
	}

	out := []api.WebACLSummary{}
	for _, r := range rows {
		out = append(out, api.WebACLSummary{WebACLId: new(r.ID), Name: new(r.Name)})
	}

	return &api.ListWebACLsResponse{WebACLs: out, NextMarker: next}, nil
}

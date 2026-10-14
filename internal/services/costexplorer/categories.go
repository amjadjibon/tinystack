package costexplorer

import (
	"context"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var zonedDatePattern = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(([+-]\d\d:\d\d)|Z)$`)

func effectiveStart(value *string) (*string, error) {
	if value == nil {
		return new(time.Now().UTC().Format("2006-01") + "-01T00:00:00Z"), nil
	}

	t, err := time.Parse(time.RFC3339, *value)
	if err != nil || !zonedDatePattern.MatchString(*value) || t.UTC().Day() != 1 || t.UTC().Hour() != 0 ||
		t.UTC().Minute() != 0 ||
		t.UTC().Second() != 0 {
		return nil, awserror.Validation("EffectiveStart must be the start of a month in RFC3339 format.")
	}

	current := time.Now().UTC()
	month := time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, time.UTC)
	if t.Before(month.AddDate(-1, 0, 0)) || t.After(current) {
		return nil, awserror.Validation("EffectiveStart cannot precede the previous twelve months or be in the future.")
	}

	return new(t.UTC().Format(time.RFC3339)), nil
}

func (s *Service) CreateCostCategoryDefinition(
	ctx context.Context,
	in *api.CreateCostCategoryDefinitionRequest,
) (*api.CreateCostCategoryDefinitionResponse, error) {
	c := &api.CostCategory{
		Name:             in.Name,
		RuleVersion:      in.RuleVersion,
		Rules:            in.Rules,
		DefaultValue:     in.DefaultValue,
		SplitChargeRules: in.SplitChargeRules,
	}

	if err := validateCategory(c); err != nil {
		return nil, err
	}

	var err error
	c.EffectiveStart, err = effectiveStart(in.EffectiveStart)
	if err != nil {
		return nil, err
	}

	r := &record{Category: c}
	if err := s.create(ctx, categoryKind, r, in.ResourceTags); err != nil {
		return nil, err
	}

	return &api.CreateCostCategoryDefinitionResponse{CostCategoryArn: new(r.ARN), EffectiveStart: c.EffectiveStart}, nil
}

func (s *Service) DescribeCostCategoryDefinition(
	ctx context.Context,
	in *api.DescribeCostCategoryDefinitionRequest,
) (*api.DescribeCostCategoryDefinitionResponse, error) {
	r, err := s.read(ctx, in.CostCategoryArn, categoryKind)
	if err != nil {
		return nil, err
	}

	if err := currentDefinition(r.Category, in.EffectiveOn); err != nil {
		return nil, err
	}

	return &api.DescribeCostCategoryDefinitionResponse{CostCategory: r.Category}, nil
}

// Only the current definition is stored. Historical reads fail explicitly.
func currentDefinition(c *api.CostCategory, effectiveOn *string) error {
	if effectiveOn == nil {
		return nil
	}

	t, err := time.Parse(time.RFC3339, *effectiveOn)
	if err != nil || !zonedDatePattern.MatchString(*effectiveOn) {
		return awserror.Validation("EffectiveOn must use RFC3339 format.")
	}

	start, err := time.Parse(time.RFC3339, common.Value(c.EffectiveStart))
	if err != nil {
		return err
	}

	if t.Before(start) {
		return awserror.New("NotImplemented", "Historical cost category definitions are not stored.")
	}

	return nil
}

func (s *Service) UpdateCostCategoryDefinition(
	ctx context.Context,
	in *api.UpdateCostCategoryDefinitionRequest,
) (*api.UpdateCostCategoryDefinitionResponse, error) {
	start, err := effectiveStart(in.EffectiveStart)
	if err != nil {
		return nil, err
	}

	err = s.edit(ctx, in.CostCategoryArn, categoryKind, func(r *record, _ storage.WriteTx) error {
		c := &api.CostCategory{
			CostCategoryArn:  in.CostCategoryArn,
			Name:             r.Category.Name,
			EffectiveStart:   start,
			RuleVersion:      in.RuleVersion,
			Rules:            in.Rules,
			DefaultValue:     common.OrElse(in.DefaultValue, r.Category.DefaultValue),
			SplitChargeRules: in.SplitChargeRules,
		}

		if in.SplitChargeRules == nil {
			c.SplitChargeRules = r.Category.SplitChargeRules
		}

		if e := validateCategory(c); e != nil {
			return e
		}

		r.Category = c
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateCostCategoryDefinitionResponse{CostCategoryArn: in.CostCategoryArn, EffectiveStart: start}, nil
}

func (s *Service) DeleteCostCategoryDefinition(
	ctx context.Context,
	in *api.DeleteCostCategoryDefinitionRequest,
) (*api.DeleteCostCategoryDefinitionResponse, error) {
	if err := s.remove(ctx, in.CostCategoryArn, categoryKind); err != nil {
		return nil, err
	}

	return &api.DeleteCostCategoryDefinitionResponse{
		CostCategoryArn: in.CostCategoryArn,
		EffectiveEnd:    new(time.Now().UTC().Format(time.RFC3339)),
	}, nil
}

func (s *Service) ListCostCategoryDefinitions(
	ctx context.Context,
	in *api.ListCostCategoryDefinitionsRequest,
) (*api.ListCostCategoryDefinitionsResponse, error) {
	if in.EffectiveOn != nil {
		if _, err := time.Parse(
			time.RFC3339,
			*in.EffectiveOn,
		); err != nil ||
			!zonedDatePattern.MatchString(*in.EffectiveOn) {
			return nil, awserror.Validation("EffectiveOn must use RFC3339 format.")
		}
	}

	rs, next, err := s.list(ctx, categoryKind, in.NextToken, in.MaxResults, func(_ *record) bool { return true }, nil)
	if err != nil {
		return nil, err
	}

	out := &api.ListCostCategoryDefinitionsResponse{
		CostCategoryReferences: []api.CostCategoryReference{},
		NextToken:              next,
	}

	for _, r := range rs {
		c := r.Category
		if err := currentDefinition(c, in.EffectiveOn); err != nil {
			return nil, err
		}

		values := []string{}
		for _, rule := range c.Rules {
			if rule.Value != nil {
				values = append(values, *rule.Value)
			}
		}

		out.CostCategoryReferences = append(out.CostCategoryReferences, api.CostCategoryReference{
			CostCategoryArn: c.CostCategoryArn,
			Name:            c.Name,
			EffectiveStart:  c.EffectiveStart,
			DefaultValue:    c.DefaultValue,
			NumberOfRules:   new(int64(len(c.Rules))),
			Values:          values,
		})
	}

	return out, nil
}

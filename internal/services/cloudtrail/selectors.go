package cloudtrail

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudtrail"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Selectors describe trail configuration; event history does not use trail selectors.
func (s *Service) PutEventSelectors(ctx context.Context, in *api.PutEventSelectorsRequest) (
	*api.PutEventSelectorsResponse, error,
) {
	if (len(in.EventSelectors) > 0) == (len(in.AdvancedEventSelectors) > 0) {
		return nil, invalid("Specify either EventSelectors or AdvancedEventSelectors")
	}

	if len(in.EventSelectors) > 5 || len(in.AdvancedEventSelectors) > 5 {
		return nil, invalid("At most five selectors")
	}

	for _, v := range in.EventSelectors {
		typ := common.Value(v.ReadWriteType)
		if typ != "" && typ != "All" && typ != attributeReadOnly && typ != "WriteOnly" {
			return nil, invalid("Invalid ReadWriteType")
		}
	}

	for _, v := range in.AdvancedEventSelectors {
		if len(v.FieldSelectors) == 0 {
			return nil, invalid("FieldSelectors must not be empty")
		}

		for i := range v.FieldSelectors {
			f := &v.FieldSelectors[i]
			if common.Value(f.Field) == "" {
				return nil, invalid("Field is required")
			}

			if len(
				f.Equals,
			)+len(
				f.NotEquals,
			)+len(
				f.StartsWith,
			)+len(
				f.NotStartsWith,
			)+len(
				f.EndsWith,
			)+len(
				f.NotEndsWith,
			) == 0 {
				return nil, invalid("A field selector requires values")
			}
		}
	}

	r, err := s.edit(ctx, common.Value(in.TrailName), func(r *record) error {
		r.Selectors = in.EventSelectors
		r.Advanced = in.AdvancedEventSelectors
		r.Trail.HasCustomEventSelectors = new(true)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutEventSelectorsResponse{
		TrailARN: r.Trail.TrailARN, EventSelectors: r.Selectors,
		AdvancedEventSelectors: r.Advanced,
	}, nil
}

func (s *Service) GetEventSelectors(ctx context.Context, in *api.GetEventSelectorsRequest) (
	*api.GetEventSelectorsResponse, error,
) {
	r, err := s.get(ctx, common.Value(in.TrailName))
	if err != nil {
		return nil, err
	}

	return &api.GetEventSelectorsResponse{
		TrailARN: r.Trail.TrailARN, EventSelectors: r.Selectors,
		AdvancedEventSelectors: r.Advanced,
	}, nil
}

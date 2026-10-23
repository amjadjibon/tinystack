package apprunner

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateAutoScalingConfiguration(
	ctx context.Context,
	in *api.CreateAutoScalingConfigurationRequest,
) (*api.CreateAutoScalingConfigurationResponse, error) {
	if err := validateCreateAutoScalingConfigurationRequest(in); err != nil {
		return nil, err
	}

	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{
		Kind: kindAutoScaling,
		Name: common.Value(in.AutoScalingConfigurationName),
		Tags: tags,
		AutoScalingConfiguration: &api.AutoScalingConfiguration{
			AutoScalingConfigurationName: in.AutoScalingConfigurationName,
			MaxConcurrency:               common.OrElse(in.MaxConcurrency, new(int64(100))),
			MinSize:                      common.OrElse(in.MinSize, new(int64(1))),
			MaxSize:                      common.OrElse(in.MaxSize, new(int64(25))),
			Latest:                       new(true),
			Status:                       new(statusActive),
			CreatedAt:                    common.SecondsMilli(time.Now()),
			IsDefault:                    new(false),
			HasAssociatedService:         new(false),
		},
	}

	if *r.AutoScalingConfiguration.MinSize > *r.AutoScalingConfiguration.MaxSize {
		return nil, invalid("MinSize exceeds MaxSize")
	}

	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}

	return &api.CreateAutoScalingConfigurationResponse{AutoScalingConfiguration: r.AutoScalingConfiguration}, nil
}

func (s *Service) DescribeAutoScalingConfiguration(
	ctx context.Context,
	in *api.DescribeAutoScalingConfigurationRequest,
) (*api.DescribeAutoScalingConfigurationResponse, error) {
	if err := validateDescribeAutoScalingConfigurationRequest(in); err != nil {
		return nil, err
	}

	if err := s.seedDefault(ctx); err != nil {
		return nil, err
	}

	r, err := s.get(ctx, in.AutoScalingConfigurationArn, kindAutoScaling)
	if err != nil {
		return nil, err
	}

	return &api.DescribeAutoScalingConfigurationResponse{AutoScalingConfiguration: r.AutoScalingConfiguration}, nil
}

func (s *Service) DeleteAutoScalingConfiguration(
	ctx context.Context,
	in *api.DeleteAutoScalingConfigurationRequest,
) (*api.DeleteAutoScalingConfigurationResponse, error) {
	if err := validateDeleteAutoScalingConfigurationRequest(in); err != nil {
		return nil, err
	}

	if common.Value(in.DeleteAllRevisions) &&
		len(strings.Split(common.Value(in.AutoScalingConfigurationArn), "/")) != 2 {
		return nil, invalid("DeleteAllRevisions requires a name-only ARN")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.AutoScalingConfiguration
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, common.Value(in.AutoScalingConfigurationArn), kindAutoScaling)
		if e != nil {
			return e
		}

		if common.Value(r.AutoScalingConfiguration.HasAssociatedService) {
			return invalid("Configuration has associated services")
		}

		out = r.AutoScalingConfiguration
		r.AutoScalingConfiguration.Status = new(statusInactive)
		r.AutoScalingConfiguration.DeletedAt = common.SecondsMilli(time.Now())
		if common.Value(r.AutoScalingConfiguration.IsDefault) {
			return invalid("Cannot delete the default auto scaling configuration")
		}

		if common.Value(in.DeleteAllRevisions) {
			all, e := storage.Records[record](tx, scope.Prefix(serviceName, kindAutoScaling))
			if e != nil {
				return e
			}

			for _, other := range all {
				if other.Name != r.Name {
					continue
				}

				if common.Value(other.AutoScalingConfiguration.HasAssociatedService) {
					return invalid("Configuration has associated services")
				}

				if common.Value(other.AutoScalingConfiguration.IsDefault) {
					return invalid("Cannot delete default configuration")
				}

				other.AutoScalingConfiguration.Status = new(statusInactive)
				other.AutoScalingConfiguration.DeletedAt = common.SecondsMilli(time.Now())
				if e := save(tx, scope, other); e != nil {
					return e
				}
			}
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAutoScalingConfigurationResponse{AutoScalingConfiguration: out}, nil
}

func (s *Service) ListAutoScalingConfigurations(
	ctx context.Context,
	in *api.ListAutoScalingConfigurationsRequest,
) (*api.ListAutoScalingConfigurationsResponse, error) {
	if err := validateListAutoScalingConfigurationsRequest(in); err != nil {
		return nil, err
	}

	if err := s.seedDefault(ctx); err != nil {
		return nil, err
	}

	items, next, err := s.list(
		ctx,
		kindAutoScaling,
		in.AutoScalingConfigurationName,
		common.OrElse(in.LatestOnly, new(true)),
		in.MaxResults,
		in.NextToken,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListAutoScalingConfigurationsResponse{
		NextToken:                           next,
		AutoScalingConfigurationSummaryList: []api.AutoScalingConfigurationSummary{},
	}

	for _, r := range items {
		out.AutoScalingConfigurationSummaryList = append(
			out.AutoScalingConfigurationSummaryList,
			api.AutoScalingConfigurationSummary{
				AutoScalingConfigurationArn:      r.AutoScalingConfiguration.AutoScalingConfigurationArn,
				AutoScalingConfigurationName:     r.AutoScalingConfiguration.AutoScalingConfigurationName,
				AutoScalingConfigurationRevision: r.AutoScalingConfiguration.AutoScalingConfigurationRevision,
				Status:                           r.AutoScalingConfiguration.Status,
				CreatedAt:                        r.AutoScalingConfiguration.CreatedAt,
				HasAssociatedService:             r.AutoScalingConfiguration.HasAssociatedService,
				IsDefault:                        r.AutoScalingConfiguration.IsDefault,
			},
		)
	}

	return out, nil
}

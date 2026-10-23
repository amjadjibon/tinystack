package apprunner

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateObservabilityConfiguration(
	ctx context.Context,
	in *api.CreateObservabilityConfigurationRequest,
) (*api.CreateObservabilityConfigurationResponse, error) {
	if err := validateCreateObservabilityConfigurationRequest(in); err != nil {
		return nil, err
	}

	if common.Value(in.ObservabilityConfigurationName) == defaultConfigurationName {
		return nil, invalid("DefaultConfiguration is reserved")
	}

	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{
		Kind: kindObservability,
		Name: common.Value(in.ObservabilityConfigurationName),
		Tags: tags,
		ObservabilityConfiguration: &api.ObservabilityConfiguration{
			ObservabilityConfigurationName: in.ObservabilityConfigurationName,
			TraceConfiguration:             in.TraceConfiguration,
			Latest:                         new(true),
			Status:                         new(statusActive),
			CreatedAt:                      common.SecondsMilli(time.Now()),
		},
	}

	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}

	return &api.CreateObservabilityConfigurationResponse{ObservabilityConfiguration: r.ObservabilityConfiguration}, nil
}

func (s *Service) DescribeObservabilityConfiguration(
	ctx context.Context,
	in *api.DescribeObservabilityConfigurationRequest,
) (*api.DescribeObservabilityConfigurationResponse, error) {
	if err := validateDescribeObservabilityConfigurationRequest(in); err != nil {
		return nil, err
	}

	r, err := s.get(ctx, in.ObservabilityConfigurationArn, kindObservability)
	if err != nil {
		return nil, err
	}

	return &api.DescribeObservabilityConfigurationResponse{
		ObservabilityConfiguration: r.ObservabilityConfiguration,
	}, nil
}

func (s *Service) DeleteObservabilityConfiguration(
	ctx context.Context,
	in *api.DeleteObservabilityConfigurationRequest,
) (*api.DeleteObservabilityConfigurationResponse, error) {
	if err := validateDeleteObservabilityConfigurationRequest(in); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ObservabilityConfiguration
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, common.Value(in.ObservabilityConfigurationArn), kindObservability)
		if e != nil {
			return e
		}

		out = r.ObservabilityConfiguration
		r.ObservabilityConfiguration.Status = new(statusInactive)
		r.ObservabilityConfiguration.DeletedAt = common.SecondsMilli(time.Now())
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteObservabilityConfigurationResponse{ObservabilityConfiguration: out}, nil
}

func (s *Service) ListObservabilityConfigurations(
	ctx context.Context,
	in *api.ListObservabilityConfigurationsRequest,
) (*api.ListObservabilityConfigurationsResponse, error) {
	if err := validateListObservabilityConfigurationsRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(
		ctx,
		kindObservability,
		in.ObservabilityConfigurationName,
		common.OrElse(in.LatestOnly, new(true)),
		in.MaxResults,
		in.NextToken,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListObservabilityConfigurationsResponse{
		NextToken:                             next,
		ObservabilityConfigurationSummaryList: []api.ObservabilityConfigurationSummary{},
	}

	for _, r := range items {
		out.ObservabilityConfigurationSummaryList = append(
			out.ObservabilityConfigurationSummaryList,
			api.ObservabilityConfigurationSummary{
				ObservabilityConfigurationArn:      r.ObservabilityConfiguration.ObservabilityConfigurationArn,
				ObservabilityConfigurationName:     r.ObservabilityConfiguration.ObservabilityConfigurationName,
				ObservabilityConfigurationRevision: r.ObservabilityConfiguration.ObservabilityConfigurationRevision,
			},
		)
	}

	return out, nil
}

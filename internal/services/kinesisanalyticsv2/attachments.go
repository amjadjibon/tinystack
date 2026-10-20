package kinesisanalyticsv2

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) addLog(tx storage.ReadTx, scope request.Scope, r *record, o *api.CloudWatchLoggingOption) error {
	if o == nil {
		return invalid("CloudWatchLoggingOption is required.")
	}

	if len(r.Detail.CloudWatchLoggingOptionDescriptions) >= 1 {
		return invalid("Only one logging option is allowed.")
	}

	if err := s.logs.CheckStreamARN(tx, scope, common.Value(o.LogStreamARN)); err != nil {
		return invalid("Log stream must exist: %s", err)
	}

	r.Detail.CloudWatchLoggingOptionDescriptions = append(
		r.Detail.CloudWatchLoggingOptionDescriptions,
		api.CloudWatchLoggingOptionDescription{
			CloudWatchLoggingOptionId: new("log-" + common.RandomID(16)),
			LogStreamARN:              o.LogStreamARN,
		},
	)
	return nil
}

// AddApplicationCloudWatchLoggingOption adds a reference to an existing local log stream.
func (s *Service) AddApplicationCloudWatchLoggingOption(
	ctx context.Context,
	in *api.AddApplicationCloudWatchLoggingOptionRequest,
) (*api.AddApplicationCloudWatchLoggingOptionResponse, error) {
	r, err := s.change(ctx, in.ApplicationName, nil, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if e := checkVersion(r, in.CurrentApplicationVersionId); e != nil {
			return e
		}

		if e := s.addLog(tx, scope, r, in.CloudWatchLoggingOption); e != nil {
			return e
		}

		revise(r)
		transition(r, "UPDATING", *r.Detail.ApplicationStatus)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddApplicationCloudWatchLoggingOptionResponse{
		ApplicationARN:                      r.Detail.ApplicationARN,
		ApplicationVersionId:                r.Detail.ApplicationVersionId,
		CloudWatchLoggingOptionDescriptions: r.Detail.CloudWatchLoggingOptionDescriptions,
	}, nil
}

// DeleteApplicationCloudWatchLoggingOption removes a logging option and increments the application version.
func (s *Service) DeleteApplicationCloudWatchLoggingOption(
	ctx context.Context,
	in *api.DeleteApplicationCloudWatchLoggingOptionRequest,
) (*api.DeleteApplicationCloudWatchLoggingOptionResponse, error) {
	r, err := s.change(ctx, in.ApplicationName, nil, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		if e := checkVersion(r, in.CurrentApplicationVersionId); e != nil {
			return e
		}

		old := r.Detail.CloudWatchLoggingOptionDescriptions
		filtered := slices.DeleteFunc(old, func(o api.CloudWatchLoggingOptionDescription) bool {
			return common.Value(o.CloudWatchLoggingOptionId) == common.Value(in.CloudWatchLoggingOptionId)
		})
		if len(filtered) == len(old) {
			return missing()
		}

		r.Detail.CloudWatchLoggingOptionDescriptions = filtered
		revise(r)
		transition(r, "UPDATING", *r.Detail.ApplicationStatus)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApplicationCloudWatchLoggingOptionResponse{
		ApplicationARN:                      r.Detail.ApplicationARN,
		ApplicationVersionId:                r.Detail.ApplicationVersionId,
		CloudWatchLoggingOptionDescriptions: r.Detail.CloudWatchLoggingOptionDescriptions,
	}, nil
}

// AddApplicationVpcConfiguration adds validated local EC2 references to an application.
func (s *Service) AddApplicationVpcConfiguration(
	ctx context.Context,
	in *api.AddApplicationVpcConfigurationRequest,
) (*api.AddApplicationVpcConfigurationResponse, error) {
	var v *api.VpcConfigurationDescription
	r, err := s.change(ctx, in.ApplicationName, nil, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if e := checkVersion(r, in.CurrentApplicationVersionId); e != nil {
			return e
		}

		d := r.Detail.ApplicationConfigurationDescription
		if len(d.VpcConfigurationDescriptions) > 0 {
			return invalid("Only one VPC configuration is allowed.")
		}

		var e error
		v, e = s.vpc(tx, scope, in.VpcConfiguration)
		if e != nil {
			return e
		}

		d.VpcConfigurationDescriptions = []api.VpcConfigurationDescription{*v}
		revise(r)
		transition(r, "UPDATING", *r.Detail.ApplicationStatus)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddApplicationVpcConfigurationResponse{
		ApplicationARN:              r.Detail.ApplicationARN,
		ApplicationVersionId:        r.Detail.ApplicationVersionId,
		VpcConfigurationDescription: v,
	}, nil
}

// DeleteApplicationVpcConfiguration removes a VPC configuration and increments the application version.
func (s *Service) DeleteApplicationVpcConfiguration(
	ctx context.Context,
	in *api.DeleteApplicationVpcConfigurationRequest,
) (*api.DeleteApplicationVpcConfigurationResponse, error) {
	r, err := s.change(ctx, in.ApplicationName, nil, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		if e := checkVersion(r, in.CurrentApplicationVersionId); e != nil {
			return e
		}

		d := r.Detail.ApplicationConfigurationDescription
		old := d.VpcConfigurationDescriptions
		filtered := slices.DeleteFunc(old, func(v api.VpcConfigurationDescription) bool {
			return common.Value(v.VpcConfigurationId) == common.Value(in.VpcConfigurationId)
		})
		if len(filtered) == len(old) {
			return missing()
		}

		d.VpcConfigurationDescriptions = filtered
		revise(r)
		transition(r, "UPDATING", *r.Detail.ApplicationStatus)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApplicationVpcConfigurationResponse{
		ApplicationARN:       r.Detail.ApplicationARN,
		ApplicationVersionId: r.Detail.ApplicationVersionId,
	}, nil
}

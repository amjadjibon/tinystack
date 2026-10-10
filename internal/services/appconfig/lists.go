package appconfig

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) ListApplications(ctx context.Context, in *api.ListApplicationsRequest) (*api.Applications, error) {
	records, next, err := s.list(ctx, kindApplication, "", "", in.NextToken, in.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	items := make([]api.Application, 0, len(records))
	for _, r := range records {
		items = append(items, *r.Application)
	}

	return &api.Applications{Items: items, NextToken: next}, nil
}

func (s *Service) ListEnvironments(ctx context.Context, in *api.ListEnvironmentsRequest) (*api.Environments, error) {
	records, next, err := s.list(
		ctx,
		kindEnvironment,
		common.Value(in.ApplicationId),
		"",
		in.NextToken,
		in.MaxResults,
		nil,
	)
	if err != nil {
		return nil, err
	}

	items := make([]api.Environment, 0, len(records))
	for _, r := range records {
		items = append(items, *r.Environment)
	}

	return &api.Environments{Items: items, NextToken: next}, nil
}

func (s *Service) ListConfigurationProfiles(
	ctx context.Context,
	in *api.ListConfigurationProfilesRequest,
) (*api.ConfigurationProfiles, error) {
	records, next, err := s.list(
		ctx,
		kindProfile,
		common.Value(in.ApplicationId),
		"",
		in.NextToken,
		in.MaxResults,
		func(r *record) bool { return in.Type == nil || common.Value(r.Profile.Type) == *in.Type },
	)
	if err != nil {
		return nil, err
	}

	items := make([]api.ConfigurationProfileSummary, 0, len(records))
	for _, r := range records {
		p := r.Profile
		types := []string{}
		for _, v := range p.Validators {
			types = append(types, common.Value(v.Type))
		}

		items = append(
			items,
			api.ConfigurationProfileSummary{
				Id:             p.Id,
				ApplicationId:  p.ApplicationId,
				Name:           p.Name,
				LocationUri:    p.LocationUri,
				Type:           p.Type,
				ValidatorTypes: types,
			},
		)
	}

	return &api.ConfigurationProfiles{Items: items, NextToken: next}, nil
}

func (s *Service) ListDeploymentStrategies(
	ctx context.Context,
	in *api.ListDeploymentStrategiesRequest,
) (*api.DeploymentStrategies, error) {
	records, next, err := s.list(ctx, kindStrategy, "", "", in.NextToken, in.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	items := make([]api.DeploymentStrategy, 0, len(records))
	for _, r := range records {
		items = append(items, *r.Strategy)
	}

	return &api.DeploymentStrategies{Items: items, NextToken: next}, nil
}

func (s *Service) ListHostedConfigurationVersions(
	ctx context.Context,
	in *api.ListHostedConfigurationVersionsRequest,
) (*api.HostedConfigurationVersions, error) {
	records, next, err := s.list(
		ctx,
		kindVersion,
		common.Value(in.ApplicationId),
		common.Value(in.ConfigurationProfileId),
		in.NextToken,
		in.MaxResults,
		func(r *record) bool { return in.VersionLabel == nil || r.Hosted.Label == *in.VersionLabel },
	)
	if err != nil {
		return nil, err
	}

	items := make([]api.HostedConfigurationVersionSummary, 0, len(records))
	for _, r := range records {
		h := r.Hosted
		items = append(
			items,
			api.HostedConfigurationVersionSummary{
				ApplicationId:          new(h.ApplicationID),
				ConfigurationProfileId: new(h.ProfileID),
				VersionNumber:          new(h.Number),
				ContentType:            new(h.ContentType),
				Description:            new(h.Description),
				VersionLabel:           new(h.Label),
			},
		)
	}

	return &api.HostedConfigurationVersions{Items: items, NextToken: next}, nil
}

func (s *Service) ListDeployments(ctx context.Context, in *api.ListDeploymentsRequest) (*api.Deployments, error) {
	records, next, err := s.list(
		ctx,
		kindDeployment,
		common.Value(in.ApplicationId),
		common.Value(in.EnvironmentId),
		in.NextToken,
		in.MaxResults,
		nil,
	)
	if err != nil {
		return nil, err
	}

	items := make([]api.DeploymentSummary, 0, len(records))
	for _, r := range records {
		d := r.Deployment
		items = append(
			items,
			api.DeploymentSummary{
				DeploymentNumber:            d.DeploymentNumber,
				ConfigurationProfileId:      d.ConfigurationProfileId,
				ConfigurationName:           d.ConfigurationName,
				ConfigurationVersion:        d.ConfigurationVersion,
				DeploymentDurationInMinutes: d.DeploymentDurationInMinutes,
				GrowthType:                  d.GrowthType,
				GrowthFactor:                d.GrowthFactor,
				FinalBakeTimeInMinutes:      d.FinalBakeTimeInMinutes,
				State:                       d.State,
				PercentageComplete:          d.PercentageComplete,
				StartedAt:                   d.StartedAt,
				CompletedAt:                 d.CompletedAt,
				VersionLabel:                d.VersionLabel,
			},
		)
	}

	return &api.Deployments{Items: items, NextToken: next}, nil
}

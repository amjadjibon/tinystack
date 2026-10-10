package appconfig

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) CreateApplication(ctx context.Context, in *api.CreateApplicationRequest) (*api.Application, error) {
	id := common.RandomID(7, "abcdefghijklmnopqrstuvwxyz0123456789")
	out := &api.Application{Id: new(id), Name: in.Name, Description: in.Description}
	r := &record{
		ID:            id,
		Name:          common.Value(in.Name),
		Kind:          kindApplication,
		ApplicationID: "",
		Tags:          in.Tags,
		Application:   out,
	}

	err := s.create(ctx, r)
	return out, err
}

func (s *Service) GetApplication(ctx context.Context, in *api.GetApplicationRequest) (*api.Application, error) {
	r, err := s.get(ctx, kindApplication, "", "", common.Value(in.ApplicationId))
	if err != nil {
		return nil, err
	}

	return r.Application, nil
}

func (s *Service) UpdateApplication(ctx context.Context, in *api.UpdateApplicationRequest) (*api.Application, error) {
	r, err := s.edit(ctx, kindApplication, "", common.Value(in.ApplicationId), func(r *record) error {
		if in.Name != nil {
			r.Application.Name = in.Name
			r.Name = *in.Name
		}

		if in.Description != nil {
			r.Application.Description = in.Description
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return r.Application, nil
}

func (s *Service) DeleteApplication(
	ctx context.Context,
	in *api.DeleteApplicationRequest,
) (*api.DeleteApplicationOutput, error) {
	err := s.remove(ctx, kindApplication, "", common.Value(in.ApplicationId))
	return &api.DeleteApplicationOutput{}, err
}

func (s *Service) CreateEnvironment(ctx context.Context, in *api.CreateEnvironmentRequest) (*api.Environment, error) {
	id := common.RandomID(7, "abcdefghijklmnopqrstuvwxyz0123456789")
	out := &api.Environment{
		Id:            new(id),
		Name:          in.Name,
		Description:   in.Description,
		ApplicationId: in.ApplicationId,
		State:         new(stateReady),
	}

	r := &record{
		ID:            id,
		Name:          common.Value(in.Name),
		Kind:          kindEnvironment,
		ApplicationID: common.Value(in.ApplicationId),
		Tags:          in.Tags,
		Environment:   out,
	}

	err := s.create(ctx, r)
	return out, err
}

func (s *Service) GetEnvironment(ctx context.Context, in *api.GetEnvironmentRequest) (*api.Environment, error) {
	r, err := s.get(ctx, kindEnvironment, common.Value(in.ApplicationId), "", common.Value(in.EnvironmentId))
	if err != nil {
		return nil, err
	}

	return r.Environment, nil
}

func (s *Service) UpdateEnvironment(ctx context.Context, in *api.UpdateEnvironmentRequest) (*api.Environment, error) {
	r, err := s.edit(
		ctx,
		kindEnvironment,
		common.Value(in.ApplicationId),
		common.Value(in.EnvironmentId),
		func(r *record) error {
			if in.Name != nil {
				r.Environment.Name = in.Name
				r.Name = *in.Name
			}

			if in.Description != nil {
				r.Environment.Description = in.Description
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return r.Environment, nil
}

func (s *Service) DeleteEnvironment(
	ctx context.Context,
	in *api.DeleteEnvironmentRequest,
) (*api.DeleteEnvironmentOutput, error) {
	err := s.remove(ctx, kindEnvironment, common.Value(in.ApplicationId), common.Value(in.EnvironmentId))
	return &api.DeleteEnvironmentOutput{}, err
}

func (s *Service) CreateConfigurationProfile(
	ctx context.Context,
	in *api.CreateConfigurationProfileRequest,
) (*api.ConfigurationProfile, error) {
	if common.Value(in.LocationUri) != "hosted" {
		return nil, bad("Only hosted LocationUri is supported")
	}

	if in.Type != nil && *in.Type != "AWS.Freeform" && *in.Type != "AWS.AppConfig.FeatureFlags" {
		return nil, bad("Unsupported configuration type")
	}

	id := common.RandomID(7, "abcdefghijklmnopqrstuvwxyz0123456789")
	out := &api.ConfigurationProfile{
		Id:            new(id),
		Name:          in.Name,
		Description:   in.Description,
		ApplicationId: in.ApplicationId,
		LocationUri:   in.LocationUri,
		Validators:    in.Validators,
		Type:          common.OrElse(in.Type, new("AWS.Freeform")),
	}

	r := &record{
		ID:            id,
		Name:          common.Value(in.Name),
		Kind:          kindProfile,
		ApplicationID: common.Value(in.ApplicationId),
		Tags:          in.Tags,
		Profile:       out,
	}

	err := s.create(ctx, r)
	return out, err
}

func (s *Service) GetConfigurationProfile(
	ctx context.Context,
	in *api.GetConfigurationProfileRequest,
) (*api.ConfigurationProfile, error) {
	r, err := s.get(ctx, kindProfile, common.Value(in.ApplicationId), "", common.Value(in.ConfigurationProfileId))
	if err != nil {
		return nil, err
	}

	return r.Profile, nil
}

func (s *Service) UpdateConfigurationProfile(
	ctx context.Context,
	in *api.UpdateConfigurationProfileRequest,
) (*api.ConfigurationProfile, error) {
	r, err := s.edit(
		ctx,
		kindProfile,
		common.Value(in.ApplicationId),
		common.Value(in.ConfigurationProfileId),
		func(r *record) error {
			if in.Name != nil {
				r.Profile.Name = in.Name
				r.Name = *in.Name
			}

			if in.Description != nil {
				r.Profile.Description = in.Description
			}

			if in.Validators != nil {
				r.Profile.Validators = in.Validators
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return r.Profile, nil
}

func (s *Service) DeleteConfigurationProfile(
	ctx context.Context,
	in *api.DeleteConfigurationProfileRequest,
) (*api.DeleteConfigurationProfileOutput, error) {
	err := s.remove(ctx, kindProfile, common.Value(in.ApplicationId), common.Value(in.ConfigurationProfileId))
	return &api.DeleteConfigurationProfileOutput{}, err
}

func (s *Service) CreateDeploymentStrategy(
	ctx context.Context,
	in *api.CreateDeploymentStrategyRequest,
) (*api.DeploymentStrategy, error) {
	if strings.HasPrefix(common.Value(in.Name), "AppConfig.") {
		return nil, bad("Reserved strategy name")
	}

	id := common.RandomID(7, "abcdefghijklmnopqrstuvwxyz0123456789")
	out := &api.DeploymentStrategy{
		Id:                          new(id),
		Name:                        in.Name,
		Description:                 in.Description,
		DeploymentDurationInMinutes: in.DeploymentDurationInMinutes,
		FinalBakeTimeInMinutes:      common.OrElse(in.FinalBakeTimeInMinutes, new(int64(0))),
		GrowthFactor:                in.GrowthFactor,
		GrowthType:                  common.OrElse(in.GrowthType, new(growthLinear)),
		ReplicateTo:                 in.ReplicateTo,
	}

	if err := validateStrategy(out); err != nil {
		return nil, err
	}

	r := &record{
		ID:            id,
		Name:          common.Value(in.Name),
		Kind:          kindStrategy,
		ApplicationID: "",
		Tags:          in.Tags,
		Strategy:      out,
	}
	err := s.create(ctx, r)
	return out, err
}

func (s *Service) GetDeploymentStrategy(
	ctx context.Context,
	in *api.GetDeploymentStrategyRequest,
) (*api.DeploymentStrategy, error) {
	r, err := s.get(ctx, kindStrategy, "", "", common.Value(in.DeploymentStrategyId))
	if err != nil {
		return nil, err
	}

	return r.Strategy, nil
}

func (s *Service) UpdateDeploymentStrategy(
	ctx context.Context,
	in *api.UpdateDeploymentStrategyRequest,
) (*api.DeploymentStrategy, error) {
	r, err := s.edit(ctx, kindStrategy, "", common.Value(in.DeploymentStrategyId), func(r *record) error {
		if in.Description != nil {
			r.Strategy.Description = in.Description
		}

		if in.DeploymentDurationInMinutes != nil {
			r.Strategy.DeploymentDurationInMinutes = in.DeploymentDurationInMinutes
		}

		if in.FinalBakeTimeInMinutes != nil {
			r.Strategy.FinalBakeTimeInMinutes = in.FinalBakeTimeInMinutes
		}

		if in.GrowthFactor != nil {
			r.Strategy.GrowthFactor = in.GrowthFactor
		}

		if in.GrowthType != nil {
			r.Strategy.GrowthType = in.GrowthType
		}

		return validateStrategy(r.Strategy)
	})
	if err != nil {
		return nil, err
	}

	return r.Strategy, nil
}

func (s *Service) DeleteDeploymentStrategy(
	ctx context.Context,
	in *api.DeleteDeploymentStrategyRequest,
) (*api.DeleteDeploymentStrategyOutput, error) {
	err := s.remove(ctx, kindStrategy, "", common.Value(in.DeploymentStrategyId))
	return &api.DeleteDeploymentStrategyOutput{}, err
}

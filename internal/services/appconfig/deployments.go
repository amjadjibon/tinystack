package appconfig

import (
	"context"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) StartDeployment(ctx context.Context, in *api.StartDeploymentRequest) (*api.Deployment, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(in.Tags); err != nil {
		return nil, bad(err.Error())
	}

	var out *api.Deployment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		app, env, profile := common.Value(
			in.ApplicationId,
		), common.Value(
			in.EnvironmentId,
		), common.Value(
			in.ConfigurationProfileId,
		)
		e, stepErr := read(tx, scope, kindEnvironment, app, "", env)
		if stepErr != nil {
			return stepErr
		}

		p, stepErr := read(tx, scope, kindProfile, app, "", profile)
		if stepErr != nil {
			return stepErr
		}

		strategy, stepErr := read(tx, scope, kindStrategy, "", "", common.Value(in.DeploymentStrategyId))
		if stepErr != nil {
			return stepErr
		}

		var version *record
		for _, k := range tx.Keys(scope.Key(serviceName, kindVersion, app+"/"+profile+"/")) {
			v, _, versionErr := storage.Read[record](tx, k)
			if versionErr != nil {
				return versionErr
			}

			if strconv.FormatInt(v.Hosted.Number, 10) == common.Value(in.ConfigurationVersion) ||
				(v.Hosted.Label != "" && v.Hosted.Label == common.Value(in.ConfigurationVersion)) {
				version = v
				break
			}
		}

		if version == nil {
			return missing()
		}

		e.Counter++
		now := time.Now().UTC()
		d := strategy.Strategy
		out = &api.Deployment{
			ApplicationId:          in.ApplicationId,
			EnvironmentId:          in.EnvironmentId,
			DeploymentStrategyId:   in.DeploymentStrategyId,
			ConfigurationProfileId: in.ConfigurationProfileId,
			DeploymentNumber: new(
				e.Counter,
			),
			ConfigurationName:           p.Profile.Name,
			ConfigurationLocationUri:    new("hosted"),
			ConfigurationVersion:        new(strconv.FormatInt(version.Hosted.Number, 10)),
			Description:                 in.Description,
			DeploymentDurationInMinutes: d.DeploymentDurationInMinutes,
			GrowthType:                  d.GrowthType,
			GrowthFactor:                d.GrowthFactor,
			FinalBakeTimeInMinutes:      d.FinalBakeTimeInMinutes,
			State:                       new(stateComplete),
			PercentageComplete:          new(float64(100)),
			StartedAt:                   new(now),
			CompletedAt:                 new(now),
			VersionLabel:                new(version.Hosted.Label),
		}

		if e.Active == nil {
			e.Active = map[string]string{}
		}

		r := &record{
			ID: versionID(e.Counter), Name: strconv.FormatInt(e.Counter, 10), Kind: kindDeployment,
			ApplicationID: app, Parent: env, Deployment: out, Previous: e.Active[profile], Tags: in.Tags,
		}

		e.Active[profile] = version.ID
		e.Environment.State = new(stateReady)
		if saveErr := save(tx, scope, e); saveErr != nil {
			return saveErr
		}

		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) GetDeployment(ctx context.Context, in *api.GetDeploymentRequest) (*api.Deployment, error) {
	r, err := s.get(ctx, kindDeployment, common.Value(in.ApplicationId), common.Value(in.EnvironmentId),
		versionID(common.Value(in.DeploymentNumber)))
	if err != nil {
		return nil, err
	}

	return r.Deployment, nil
}

func (s *Service) StopDeployment(ctx context.Context, in *api.StopDeploymentRequest) (*api.Deployment, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Deployment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		app, env := common.Value(in.ApplicationId), common.Value(in.EnvironmentId)
		r, e := read(tx, scope, kindDeployment, app, env, versionID(common.Value(in.DeploymentNumber)))
		if e != nil {
			return e
		}

		if !common.Value(in.AllowRevert) {
			return bad("Completed deployments require AllowRevert")
		}

		environment, e := read(tx, scope, kindEnvironment, app, "", env)
		if e != nil {
			return e
		}

		if environment.Counter != common.Value(in.DeploymentNumber) ||
			common.Value(r.Deployment.State) != stateComplete {
			return conflict("Only the latest completed deployment can be reverted")
		}

		profile := common.Value(r.Deployment.ConfigurationProfileId)
		if r.Previous != "" {
			if _, e = read(tx, scope, kindVersion, app, profile, r.Previous); e != nil {
				return e
			}

			environment.Active[profile] = r.Previous
		} else {
			delete(environment.Active, profile)
		}

		environment.Environment.State = new("REVERTED")
		r.Deployment.State = new("REVERTED")
		out = r.Deployment
		if saveErr := save(tx, scope, environment); saveErr != nil {
			return saveErr
		}

		return save(tx, scope, r)
	})
	return out, err
}

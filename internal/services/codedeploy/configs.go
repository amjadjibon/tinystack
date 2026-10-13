package codedeploy

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Names and traffic policies: AWS CodeDeploy User Guide, deployment-configurations.html.
// OneAtATime's special output type: APIReference/API_MinimumHealthyHosts.html.
// Built-ins are immutable metadata; IDs/timestamps are omitted rather than invented.
func builtins() map[string]*api.DeploymentConfigInfo {
	out := map[string]*api.DeploymentConfigInfo{}
	for _, c := range []struct {
		name, kind string
		value      int64
	}{
		{allAtOnce, "HOST_COUNT", 0}, {"HalfAtATime", fleetPercent, 50}, {"OneAtATime", "MOST_CONCURRENCY", 1},
	} {
		name := "CodeDeployDefault." + c.name
		out[name] = &api.DeploymentConfigInfo{
			DeploymentConfigName: new(name), ComputePlatform: new(server),
			MinimumHealthyHosts: &api.MinimumHealthyHosts{Type: new(c.kind), Value: new(c.value)},
		}
	}

	for _, c := range []struct {
		platform, suffix, kind string
		interval               int64
	}{
		{lambda, "Canary10Percent5Minutes", timeBasedCanary, 5},
		{lambda, "Canary10Percent10Minutes", timeBasedCanary, 10},
		{lambda, "Canary10Percent15Minutes", timeBasedCanary, 15},
		{lambda, "Canary10Percent30Minutes", timeBasedCanary, 30},
		{lambda, "Linear10PercentEvery1Minute", timeBasedLinear, 1},
		{lambda, "Linear10PercentEvery2Minutes", timeBasedLinear, 2},
		{lambda, "Linear10PercentEvery3Minutes", timeBasedLinear, 3},
		{lambda, "Linear10PercentEvery10Minutes", timeBasedLinear, 10},
		{lambda, allAtOnce, allAtOnce, 0},
		{ecs, "Linear10PercentEvery1Minutes", timeBasedLinear, 1},
		{ecs, "Linear10PercentEvery3Minutes", timeBasedLinear, 3},
		{ecs, "Canary10Percent5Minutes", timeBasedCanary, 5},
		{ecs, "Canary10Percent15Minutes", timeBasedCanary, 15},
		{ecs, allAtOnce, allAtOnce, 0},
	} {
		traffic := &api.TrafficRoutingConfig{Type: new(c.kind)}
		switch c.kind {
		case timeBasedCanary:
			traffic.TimeBasedCanary = &api.TimeBasedCanary{
				CanaryPercentage: new(int64(10)),
				CanaryInterval:   new(c.interval),
			}

		case timeBasedLinear:
			traffic.TimeBasedLinear = &api.TimeBasedLinear{
				LinearPercentage: new(int64(10)),
				LinearInterval:   new(c.interval),
			}
		}

		name := "CodeDeployDefault." + c.platform + c.suffix
		out[name] = &api.DeploymentConfigInfo{
			DeploymentConfigName: new(name),
			ComputePlatform:      new(c.platform),
			TrafficRoutingConfig: traffic,
		}
	}

	return out
}

func readConfig(tx storage.ReadTx, scope request.Scope, name *string) (*api.DeploymentConfigInfo, error) {
	if validationErr := checkName(name, "DeploymentConfig"); validationErr != nil {
		return nil, validationErr
	}

	if c, ok := builtins()[*name]; ok {
		return c, nil
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, configKind, *name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, awserror.New("DeploymentConfigDoesNotExistException", "Deployment configuration does not exist")
	}

	return r.Config, nil
}

func (s *Service) CreateDeploymentConfig(
	ctx context.Context,
	in *api.CreateDeploymentConfigInput,
) (*api.CreateDeploymentConfigOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.CreateDeploymentConfigInput{}
	}

	in = common.Clone(in)
	c := &api.DeploymentConfigInfo{
		DeploymentConfigName: in.DeploymentConfigName,
		ComputePlatform: common.OrElse(
			in.ComputePlatform,
			new(server),
		),
		MinimumHealthyHosts:  in.MinimumHealthyHosts,
		TrafficRoutingConfig: in.TrafficRoutingConfig,
		DeploymentConfigId:   new(common.RandomID(32)),
		CreateTime:           common.SecondsMilli(time.Now()),
	}

	if validationErr := checkConfig(c); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, configKind, *c.DeploymentConfigName)
		if _, ok := tx.Get(key); ok {
			return awserror.New("DeploymentConfigAlreadyExistsException", "Deployment configuration already exists")
		}

		return storage.Write(tx, key, &record{Config: c})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDeploymentConfigOutput{DeploymentConfigId: c.DeploymentConfigId}, nil
}

func (s *Service) GetDeploymentConfig(
	ctx context.Context,
	in *api.GetDeploymentConfigInput,
) (*api.GetDeploymentConfigOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.GetDeploymentConfigInput{}
	}

	var c *api.DeploymentConfigInfo
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		c, e = readConfig(tx, scope, in.DeploymentConfigName)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetDeploymentConfigOutput{DeploymentConfigInfo: c}, nil
}

func (s *Service) ListDeploymentConfigs(
	ctx context.Context,
	in *api.ListDeploymentConfigsInput,
) (*api.ListDeploymentConfigsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.ListDeploymentConfigsInput{}
	}

	names := []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := scope.Prefix(serviceName, configKind)
		for _, key := range tx.Keys(prefix) {
			names = append(names, strings.TrimPrefix(key, prefix))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Merge virtual built-ins into the already ordered custom-name list for cursor pagination.
	for name := range builtins() {
		i, _ := slices.BinarySearch(names, name)
		names = slices.Insert(names, i, name)
	}

	page, next, err := common.Page(names, func(n string) string { return n }, in.NextToken, 100,
		awserror.New("InvalidNextTokenException", "Invalid nextToken"))
	return &api.ListDeploymentConfigsOutput{DeploymentConfigsList: page, NextToken: next}, err
}

func (s *Service) DeleteDeploymentConfig(
	ctx context.Context,
	in *api.DeleteDeploymentConfigInput,
) (*api.DeleteDeploymentConfigOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.DeleteDeploymentConfigInput{}
	}

	if validationErr := checkName(in.DeploymentConfigName, "DeploymentConfig"); validationErr != nil {
		return nil, validationErr
	}

	if _, ok := builtins()[*in.DeploymentConfigName]; ok {
		return nil, awserror.New("InvalidOperationException", "Predefined configurations cannot be deleted")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		groups, e := storage.Records[record](tx, scope.Prefix(serviceName, groupKind))
		if e != nil {
			return e
		}

		for _, g := range groups {
			if *g.Group.DeploymentConfigName == *in.DeploymentConfigName {
				return awserror.New("DeploymentConfigInUseException", "Deployment configuration is in use")
			}
		}

		tx.Delete(scope.Key(serviceName, configKind, *in.DeploymentConfigName))
		return nil
	})
	return &api.DeleteDeploymentConfigOutput{}, err
}

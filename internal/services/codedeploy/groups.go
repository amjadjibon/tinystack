package codedeploy

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// mergeGroup is shared by create (over defaults) and update (over the current record).
func mergeGroup(g *api.DeploymentGroupInfo, in *api.UpdateDeploymentGroupInput) {
	if in.NewDeploymentGroupName != nil {
		g.DeploymentGroupName = new(*in.NewDeploymentGroupName)
	}

	if in.DeploymentConfigName != nil {
		g.DeploymentConfigName = new(*in.DeploymentConfigName)
	}

	if in.ServiceRoleArn != nil {
		g.ServiceRoleArn = new(*in.ServiceRoleArn)
	}

	if in.Ec2TagFilters != nil {
		g.Ec2TagFilters = *common.Clone(&in.Ec2TagFilters)
	}

	if in.OnPremisesInstanceTagFilters != nil {
		g.OnPremisesInstanceTagFilters = *common.Clone(&in.OnPremisesInstanceTagFilters)
	}

	if in.EcsServices != nil {
		g.EcsServices = *common.Clone(&in.EcsServices)
	}

	if in.AutoScalingGroups != nil {
		g.AutoScalingGroups = []api.AutoScalingGroup{}
		for _, name := range in.AutoScalingGroups {
			g.AutoScalingGroups = append(g.AutoScalingGroups, api.AutoScalingGroup{Name: new(name)})
		}
	}

	if in.AlarmConfiguration != nil {
		g.AlarmConfiguration = common.Clone(in.AlarmConfiguration)
	}

	if in.AutoRollbackConfiguration != nil {
		g.AutoRollbackConfiguration = common.Clone(in.AutoRollbackConfiguration)
	}

	if in.BlueGreenDeploymentConfiguration != nil {
		g.BlueGreenDeploymentConfiguration = common.Clone(in.BlueGreenDeploymentConfiguration)
	}

	if in.LoadBalancerInfo != nil {
		g.LoadBalancerInfo = common.Clone(in.LoadBalancerInfo)
	}

	if in.DeploymentStyle != nil {
		if in.DeploymentStyle.DeploymentType != nil {
			g.DeploymentStyle.DeploymentType = new(*in.DeploymentStyle.DeploymentType)
		}

		if in.DeploymentStyle.DeploymentOption != nil {
			g.DeploymentStyle.DeploymentOption = new(*in.DeploymentStyle.DeploymentOption)
		}
	}
}

func validateGroup(tx storage.ReadTx, scope request.Scope, g *api.DeploymentGroupInfo) error {
	if validationErr := checkGroup(g); validationErr != nil {
		return validationErr
	}

	c, err := readConfig(tx, scope, g.DeploymentConfigName)
	if err != nil {
		return err
	}

	if *c.ComputePlatform != *g.ComputePlatform {
		return awserror.New(
			"InvalidDeploymentConfigNameException",
			"Deployment configuration must match the application's compute platform",
		)
	}

	return nil
}

func (s *Service) CreateDeploymentGroup(
	ctx context.Context,
	in *api.CreateDeploymentGroupInput,
) (*api.CreateDeploymentGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.CreateDeploymentGroupInput{}
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if validationErr := checkGroupTags(tags); validationErr != nil {
		return nil, validationErr
	}

	var out *api.CreateDeploymentGroupOutput
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, e := readApplication(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		g := &api.DeploymentGroupInfo{
			ApplicationName: new(*in.ApplicationName), DeploymentGroupName: in.DeploymentGroupName,
			DeploymentGroupId: new(common.RandomID(32)), ComputePlatform: a.Application.ComputePlatform,
			DeploymentConfigName: new(oneAtATime), DeploymentStyle: &api.DeploymentStyle{
				DeploymentType: new("IN_PLACE"), DeploymentOption: new("WITHOUT_TRAFFIC_CONTROL"),
			},
		}

		if *g.ComputePlatform != server {
			g.DeploymentStyle = &api.DeploymentStyle{
				DeploymentType:   new("BLUE_GREEN"),
				DeploymentOption: new("WITH_TRAFFIC_CONTROL"),
			}
		}

		mergeGroup(g, &api.UpdateDeploymentGroupInput{
			DeploymentConfigName:             in.DeploymentConfigName,
			ServiceRoleArn:                   in.ServiceRoleArn,
			Ec2TagFilters:                    in.Ec2TagFilters,
			OnPremisesInstanceTagFilters:     in.OnPremisesInstanceTagFilters,
			AutoScalingGroups:                in.AutoScalingGroups,
			AlarmConfiguration:               in.AlarmConfiguration,
			AutoRollbackConfiguration:        in.AutoRollbackConfiguration,
			BlueGreenDeploymentConfiguration: in.BlueGreenDeploymentConfiguration,
			LoadBalancerInfo:                 in.LoadBalancerInfo,
			EcsServices:                      in.EcsServices,
			DeploymentStyle:                  in.DeploymentStyle,
		})
		if e := validateGroup(tx, scope, g); e != nil {
			return e
		}

		key := scope.Key(serviceName, groupKind, *g.ApplicationName+"/"+*g.DeploymentGroupName)
		if _, ok := tx.Get(key); ok {
			return awserror.New("DeploymentGroupAlreadyExistsException", "Deployment group already exists")
		}

		out = &api.CreateDeploymentGroupOutput{DeploymentGroupId: g.DeploymentGroupId}
		return storage.Write(tx, key, &record{Group: g, Tags: tags})
	})
	return out, err
}

func (s *Service) GetDeploymentGroup(
	ctx context.Context,
	in *api.GetDeploymentGroupInput,
) (*api.GetDeploymentGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.GetDeploymentGroupInput{}
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readGroup(tx, scope, in.ApplicationName, in.DeploymentGroupName)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetDeploymentGroupOutput{DeploymentGroupInfo: r.Group}, nil
}

func (s *Service) BatchGetDeploymentGroups(
	ctx context.Context,
	in *api.BatchGetDeploymentGroupsInput,
) (*api.BatchGetDeploymentGroupsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.BatchGetDeploymentGroupsInput{}
	}

	if validationErr := checkBatch(in.DeploymentGroupNames, "DeploymentGroup"); validationErr != nil {
		return nil, validationErr
	}

	out := &api.BatchGetDeploymentGroupsOutput{DeploymentGroupsInfo: []api.DeploymentGroupInfo{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, e := readApplication(tx, scope, in.ApplicationName); e != nil {
			return e
		}

		for _, name := range in.DeploymentGroupNames {
			r, ok, e := storage.Read[record](tx, scope.Key(serviceName, groupKind, *in.ApplicationName+"/"+name))
			if e != nil {
				return e
			}

			if !ok {
				out.ErrorMessage = new("One or more deployment groups do not exist")
				continue
			}

			out.DeploymentGroupsInfo = append(out.DeploymentGroupsInfo, *r.Group)
		}

		return nil
	})
	return out, err
}

func (s *Service) ListDeploymentGroups(
	ctx context.Context,
	in *api.ListDeploymentGroupsInput,
) (*api.ListDeploymentGroupsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.ListDeploymentGroupsInput{}
	}

	names := []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, e := readApplication(tx, scope, in.ApplicationName); e != nil {
			return e
		}

		prefix := scope.Key(serviceName, groupKind, *in.ApplicationName) + "/"
		for _, key := range tx.Keys(prefix) {
			names = append(names, strings.TrimPrefix(key, prefix))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(names, func(n string) string { return n }, in.NextToken, 100,
		awserror.New("InvalidNextTokenException", "Invalid nextToken"))
	return &api.ListDeploymentGroupsOutput{
		ApplicationName:  in.ApplicationName,
		DeploymentGroups: page,
		NextToken:        next,
	}, err
}

func (s *Service) UpdateDeploymentGroup(
	ctx context.Context,
	in *api.UpdateDeploymentGroupInput,
) (*api.UpdateDeploymentGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.UpdateDeploymentGroupInput{}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readGroup(tx, scope, in.ApplicationName, in.CurrentDeploymentGroupName)
		if e != nil {
			return e
		}

		mergeGroup(r.Group, in)
		if e := validateGroup(tx, scope, r.Group); e != nil {
			return e
		}

		old := scope.Key(serviceName, groupKind, *in.ApplicationName+"/"+*in.CurrentDeploymentGroupName)
		key := scope.Key(serviceName, groupKind, *in.ApplicationName+"/"+*r.Group.DeploymentGroupName)
		if key != old {
			if _, ok := tx.Get(key); ok {
				return awserror.New("DeploymentGroupAlreadyExistsException", "Deployment group already exists")
			}

			tx.Delete(old)
		}

		return storage.Write(tx, key, r)
	})
	return &api.UpdateDeploymentGroupOutput{HooksNotCleanedUp: []api.AutoScalingGroup{}}, err
}

func (s *Service) DeleteDeploymentGroup(
	ctx context.Context,
	in *api.DeleteDeploymentGroupInput,
) (*api.DeleteDeploymentGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.DeleteDeploymentGroupInput{}
	}

	if validationErr := checkName(in.ApplicationName, "Application"); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkName(in.DeploymentGroupName, "DeploymentGroup"); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, groupKind, *in.ApplicationName+"/"+*in.DeploymentGroupName))
		return nil
	})
	return &api.DeleteDeploymentGroupOutput{HooksNotCleanedUp: []api.AutoScalingGroup{}}, err
}

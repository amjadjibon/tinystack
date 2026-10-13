package codedeploy_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/codedeploy"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestApplicationGroupLifecycleAtomicityAndScope(t *testing.T) {
	s := codedeploy.New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")
	in := &api.CreateApplicationInput{ApplicationName: new("app"), Tags: []api.Tag{{Key: new("env")}}}
	created, err := s.CreateApplication(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateApplication(ctx, in)
	wantCode(t, err, "ApplicationAlreadyExistsException")
	*in.ApplicationName = "caller-mutated"
	got, err := s.GetApplication(ctx, &api.GetApplicationInput{ApplicationName: new("app")})
	if err != nil || *got.Application.ApplicationId != *created.ApplicationId ||
		*got.Application.ComputePlatform != "Server" {
		t.Fatal(got, err)
	}

	for _, other := range []context.Context{
		testContext("123456789013", "us-east-1"), testContext("123456789012", "us-west-2"),
	} {
		_, err = s.GetApplication(other, &api.GetApplicationInput{ApplicationName: new("app")})
		wantCode(t, err, "ApplicationDoesNotExistException")
	}

	group := &api.CreateDeploymentGroupInput{
		ApplicationName: new(
			"app",
		),
		DeploymentGroupName: new("group"),
		ServiceRoleArn:      new("arn:aws:iam::123456789012:role/deploy"),
		Ec2TagFilters: []api.EC2TagFilter{
			{Type: new("KEY_ONLY"), Key: new("env")},
		},
		AutoScalingGroups: []string{"workers"},
		AlarmConfiguration: &api.AlarmConfiguration{
			Enabled: new(true),
			Alarms:  []api.Alarm{{Name: new("alert")}},
		},
		AutoRollbackConfiguration: &api.AutoRollbackConfiguration{
			Enabled: new(true),
			Events:  []string{"DEPLOYMENT_FAILURE"},
		},
	}

	g, err := s.CreateDeploymentGroup(ctx, group)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateDeploymentGroup(ctx, group)
	wantCode(t, err, "DeploymentGroupAlreadyExistsException")
	before, err := s.GetDeploymentGroup(
		ctx,
		&api.GetDeploymentGroupInput{ApplicationName: new("app"), DeploymentGroupName: new("group")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.UpdateDeploymentGroup(ctx, &api.UpdateDeploymentGroupInput{
		ApplicationName:            new("app"),
		CurrentDeploymentGroupName: new("group"),
		NewDeploymentGroupName: new(
			"renamed",
		),
		ServiceRoleArn:       new("arn:aws:iam::123456789012:role/changed"),
		DeploymentConfigName: new("CodeDeployDefault.LambdaAllAtOnce"),
	})
	wantCode(t, err, "InvalidDeploymentConfigNameException")
	after, err := s.GetDeploymentGroup(
		ctx,
		&api.GetDeploymentGroupInput{ApplicationName: new("app"), DeploymentGroupName: new("group")},
	)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rejected update mutated record", before, after, err)
	}

	_, err = s.UpdateDeploymentGroup(ctx, &api.UpdateDeploymentGroupInput{
		ApplicationName:            new("app"),
		CurrentDeploymentGroupName: new("group"),
		NewDeploymentGroupName:     new("renamed"),
		AutoScalingGroups:          []string{},
		AlarmConfiguration: &api.AlarmConfiguration{
			Enabled: new(false),
		},
		AutoRollbackConfiguration: &api.AutoRollbackConfiguration{Enabled: new(false)},
	})
	if err != nil {
		t.Fatal(err)
	}

	after, err = s.GetDeploymentGroup(
		ctx,
		&api.GetDeploymentGroupInput{ApplicationName: new("app"), DeploymentGroupName: new("renamed")},
	)
	if err != nil || *after.DeploymentGroupInfo.DeploymentGroupId != *g.DeploymentGroupId ||
		len(after.DeploymentGroupInfo.AutoScalingGroups) != 0 ||
		len(after.DeploymentGroupInfo.AlarmConfiguration.Alarms) != 0 ||
		len(after.DeploymentGroupInfo.AutoRollbackConfiguration.Events) != 0 {
		t.Fatal(after, err)
	}

	_, err = s.UpdateApplication(
		ctx,
		&api.UpdateApplicationInput{ApplicationName: new("app"), NewApplicationName: new("new-app")},
	)
	if err != nil {
		t.Fatal(err)
	}

	after, err = s.GetDeploymentGroup(
		ctx,
		&api.GetDeploymentGroupInput{ApplicationName: new("new-app"), DeploymentGroupName: new("renamed")},
	)
	if err != nil || *after.DeploymentGroupInfo.ApplicationName != "new-app" {
		t.Fatal(after, err)
	}

	for _, arn := range []string{
		"arn:aws:codedeploy:us-east-1:123456789012:application:new-app",
		"arn:aws:codedeploy:us-east-1:123456789012:deploymentgroup:new-app/renamed",
	} {
		_, err = s.TagResource(
			ctx,
			&api.TagResourceInput{ResourceArn: new(arn), Tags: []api.Tag{{Key: new("owner"), Value: new("team")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := s.ListTagsForResource(ctx, &api.ListTagsForResourceInput{ResourceArn: new(arn)})
		if e != nil || len(tags.Tags) == 0 {
			t.Fatal(tags, e)
		}

		_, err = s.UntagResource(ctx, &api.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"owner"}})
		if err != nil {
			t.Fatal(err)
		}
	}

	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 2 {
		t.Fatal(resources, err)
	}

	for _, r := range resources {
		if r.Details.(map[string]any)["RecordsOnly"] != true {
			t.Fatal(r)
		}
	}

	_, err = s.DeleteApplication(ctx, &api.DeleteApplicationInput{ApplicationName: new("new-app")})
	if err != nil {
		t.Fatal(err)
	}

	resources, err = s.Resources(ctx)
	if err != nil || len(resources) != 0 {
		t.Fatal(resources, err)
	}
}

func TestConfigsAndPagination(t *testing.T) {
	s := codedeploy.New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")
	builtin, err := s.GetDeploymentConfig(
		ctx,
		&api.GetDeploymentConfigInput{DeploymentConfigName: new("CodeDeployDefault.OneAtATime")},
	)
	if err != nil || *builtin.DeploymentConfigInfo.MinimumHealthyHosts.Type != "MOST_CONCURRENCY" {
		t.Fatal(builtin, err)
	}

	again, err := s.GetDeploymentConfig(
		ctx,
		&api.GetDeploymentConfigInput{DeploymentConfigName: new("CodeDeployDefault.OneAtATime")},
	)
	if err != nil || !reflect.DeepEqual(builtin, again) {
		t.Fatal("unstable builtin", builtin, again, err)
	}

	configs, err := s.ListDeploymentConfigs(ctx, &api.ListDeploymentConfigsInput{})
	if err != nil || len(configs.DeploymentConfigsList) != 17 {
		t.Fatal(configs, err)
	}

	_, err = s.DeleteDeploymentConfig(
		ctx,
		&api.DeleteDeploymentConfigInput{DeploymentConfigName: new("CodeDeployDefault.AllAtOnce")},
	)
	wantCode(t, err, "InvalidOperationException")
	c := &api.CreateDeploymentConfigInput{
		DeploymentConfigName: new("custom"),
		MinimumHealthyHosts:  &api.MinimumHealthyHosts{Type: new("FLEET_PERCENT"), Value: new(int64(75))},
	}

	_, err = s.CreateDeploymentConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateDeploymentConfig(ctx, c)
	wantCode(t, err, "DeploymentConfigAlreadyExistsException")
	_, err = s.CreateApplication(ctx, &api.CreateApplicationInput{ApplicationName: new("app")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateDeploymentGroup(
		ctx,
		&api.CreateDeploymentGroupInput{
			ApplicationName:      new("app"),
			DeploymentGroupName:  new("group"),
			ServiceRoleArn:       new("arn:aws:iam::123456789012:role/role"),
			DeploymentConfigName: new("custom"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteDeploymentConfig(ctx, &api.DeleteDeploymentConfigInput{DeploymentConfigName: new("custom")})
	wantCode(t, err, "DeploymentConfigInUseException")
	_, err = s.DeleteDeploymentGroup(
		ctx,
		&api.DeleteDeploymentGroupInput{ApplicationName: new("app"), DeploymentGroupName: new("group")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteDeploymentConfig(ctx, &api.DeleteDeploymentConfigInput{DeploymentConfigName: new("custom")})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 101 {
		_, err = s.CreateApplication(ctx, &api.CreateApplicationInput{ApplicationName: new(fmt.Sprintf("p%03d", i))})
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.ListApplications(ctx, &api.ListApplicationsInput{})
	if err != nil || len(page.Applications) != 100 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	_, err = s.DeleteApplication(ctx, &api.DeleteApplicationInput{ApplicationName: new(page.Applications[99])})
	if err != nil {
		t.Fatal(err)
	}

	rest, err := s.ListApplications(ctx, &api.ListApplicationsInput{NextToken: page.NextToken})
	if err != nil || len(rest.Applications) != 2 || rest.Applications[0] != "p099" {
		t.Fatal(rest, err)
	}

	_, err = s.ListApplications(ctx, &api.ListApplicationsInput{NextToken: new("%%%")})
	wantCode(t, err, "InvalidNextTokenException")
}

func TestValidationAndECSCandidate(t *testing.T) {
	s := codedeploy.New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")
	for _, in := range []*api.CreateApplicationInput{
		{ApplicationName: new("bad/name")}, {ApplicationName: new("valid"), ComputePlatform: new("invalid")},
	} {
		_, err := s.CreateApplication(ctx, in)
		if err == nil {
			t.Fatal("accepted invalid application", in)
		}
	}

	for _, p := range []string{"Lambda", "ECS"} {
		_, err := s.CreateApplication(
			ctx,
			&api.CreateApplicationInput{ApplicationName: new(p), ComputePlatform: new(p)},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, in := range []*api.CreateDeploymentConfigInput{
		{
			DeploymentConfigName: new("bad-host-type"),
			MinimumHealthyHosts:  &api.MinimumHealthyHosts{Type: new("MOST_CONCURRENCY"), Value: new(int64(1))},
		},
		{
			DeploymentConfigName: new("bad-host-value"),
			MinimumHealthyHosts:  &api.MinimumHealthyHosts{Type: new("FLEET_PERCENT"), Value: new(int64(101))},
		},
		{
			DeploymentConfigName: new("bad-traffic"), ComputePlatform: new("Lambda"),
			TrafficRoutingConfig: &api.TrafficRoutingConfig{Type: new("invalid")},
		},
		{
			DeploymentConfigName: new("bad-canary"), ComputePlatform: new("Lambda"),
			TrafficRoutingConfig: &api.TrafficRoutingConfig{
				Type:            new("TimeBasedCanary"),
				TimeBasedCanary: &api.TimeBasedCanary{CanaryPercentage: new(int64(100)), CanaryInterval: new(int64(1))},
			},
		},
		{
			DeploymentConfigName: new("bad-linear"), ComputePlatform: new("ECS"),
			TrafficRoutingConfig: &api.TrafficRoutingConfig{
				Type:            new("TimeBasedLinear"),
				TimeBasedLinear: &api.TimeBasedLinear{LinearPercentage: new(int64(10)), LinearInterval: new(int64(0))},
			},
		},
	} {
		_, err := s.CreateDeploymentConfig(ctx, in)
		if err == nil {
			t.Fatal("accepted invalid config", in)
		}
	}

	group := &api.CreateDeploymentGroupInput{
		ApplicationName:      new("ECS"),
		DeploymentConfigName: new("CodeDeployDefault.ECSAllAtOnce"),
		DeploymentGroupName:  new("ecs-group"),
		ServiceRoleArn:       new("arn:aws:iam::123456789012:role/role"),
		EcsServices:          []api.ECSService{{ClusterName: new("cluster"), ServiceName: new("service")}},
		BlueGreenDeploymentConfiguration: &api.BlueGreenDeploymentConfiguration{
			DeploymentReadyOption: &api.DeploymentReadyOption{
				ActionOnTimeout: new("CONTINUE_DEPLOYMENT"),
			},
			TerminateBlueInstancesOnDeploymentSuccess: &api.BlueInstanceTerminationOption{
				Action:                       new("TERMINATE"),
				TerminationWaitTimeInMinutes: new(int64(5)),
			},
		},
		LoadBalancerInfo: &api.LoadBalancerInfo{
			TargetGroupPairInfoList: []api.TargetGroupPairInfo{
				{
					TargetGroups: []api.TargetGroupInfo{{Name: new("blue")}, {Name: new("green")}},
					ProdTrafficRoute: &api.TrafficRoute{
						ListenerArns: []string{
							"arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/lb/blue/production",
						},
					},
				},
			},
		},
	}

	_, err := s.CreateDeploymentGroup(ctx, group)
	if err != nil {
		t.Fatal(err)
	}

	for _, mutate := range []func(*api.UpdateDeploymentGroupInput){
		func(in *api.UpdateDeploymentGroupInput) {
			in.DeploymentStyle = &api.DeploymentStyle{DeploymentType: new("invalid")}
		},
		func(in *api.UpdateDeploymentGroupInput) {
			in.DeploymentStyle = &api.DeploymentStyle{DeploymentOption: new("invalid")}
		},
		func(in *api.UpdateDeploymentGroupInput) {
			in.AutoRollbackConfiguration = &api.AutoRollbackConfiguration{Enabled: new(true), Events: []string{"invalid"}}
		},
		func(in *api.UpdateDeploymentGroupInput) {
			in.BlueGreenDeploymentConfiguration = &api.BlueGreenDeploymentConfiguration{
				DeploymentReadyOption: &api.DeploymentReadyOption{ActionOnTimeout: new("invalid")},
			}
		},
		func(in *api.UpdateDeploymentGroupInput) {
			in.BlueGreenDeploymentConfiguration = &api.BlueGreenDeploymentConfiguration{
				TerminateBlueInstancesOnDeploymentSuccess: &api.BlueInstanceTerminationOption{Action: new("invalid")},
			}
		},
		func(in *api.UpdateDeploymentGroupInput) { in.EcsServices = []api.ECSService{} },
		func(in *api.UpdateDeploymentGroupInput) { in.LoadBalancerInfo = &api.LoadBalancerInfo{} },
		func(in *api.UpdateDeploymentGroupInput) {
			in.Ec2TagFilters = []api.EC2TagFilter{{Type: new("invalid")}}
		},
		func(in *api.UpdateDeploymentGroupInput) {
			in.OnPremisesInstanceTagFilters = []api.TagFilter{{Type: new("invalid")}}
		},
	} {
		in := &api.UpdateDeploymentGroupInput{ApplicationName: new("ECS"), CurrentDeploymentGroupName: new("ecs-group")}
		mutate(in)
		_, err = s.UpdateDeploymentGroup(ctx, in)
		if err == nil {
			t.Fatal("accepted invalid candidate", in)
		}

		got, e := s.GetDeploymentGroup(
			ctx,
			&api.GetDeploymentGroupInput{ApplicationName: new("ECS"), DeploymentGroupName: new("ecs-group")},
		)
		if e != nil || *got.DeploymentGroupInfo.DeploymentStyle.DeploymentType != "BLUE_GREEN" {
			t.Fatal(got, e)
		}
	}

	copied := common.Clone(group)
	copied.ApplicationName = new("Lambda")
	_, err = s.CreateDeploymentGroup(ctx, copied)
	wantCode(t, err, "InvalidECSServiceException")
}

package integration_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codedeploy"
	"github.com/aws/aws-sdk-go-v2/service/codedeploy/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func codeDeployClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestCodeDeploySDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codeDeployClient(s)
	app := new("sdk-app")
	created, err := c.CreateApplication(
		ctx,
		&sdk.CreateApplicationInput{ApplicationName: app, Tags: []types.Tag{{Key: new("env"), Value: new("dev")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateApplication(ctx, &sdk.CreateApplicationInput{ApplicationName: app})
	wantCode(t, err, "ApplicationAlreadyExistsException")
	got, err := c.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationName: app})
	if err != nil || aws.ToString(got.Application.ApplicationId) != aws.ToString(created.ApplicationId) ||
		got.Application.ComputePlatform != types.ComputePlatformServer {
		t.Fatal(got, err)
	}

	batch, err := c.BatchGetApplications(ctx, &sdk.BatchGetApplicationsInput{ApplicationNames: []string{*app}})
	if err != nil || len(batch.ApplicationsInfo) != 1 {
		t.Fatal(batch, err)
	}

	_, err = c.BatchGetApplications(ctx, &sdk.BatchGetApplicationsInput{ApplicationNames: []string{*app, "missing"}})
	wantCode(t, err, "ApplicationDoesNotExistException")
	apps, err := c.ListApplications(ctx, &sdk.ListApplicationsInput{})
	if err != nil || len(apps.Applications) != 1 || apps.Applications[0] != *app {
		t.Fatal(apps, err)
	}

	cfg := new("sdk-config")
	configCreated, err := c.CreateDeploymentConfig(
		ctx,
		&sdk.CreateDeploymentConfigInput{
			DeploymentConfigName: cfg,
			MinimumHealthyHosts: &types.MinimumHealthyHosts{
				Type:  types.MinimumHealthyHostsTypeFleetPercent,
				Value: 75,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	config, err := c.GetDeploymentConfig(ctx, &sdk.GetDeploymentConfigInput{DeploymentConfigName: cfg})
	if err != nil ||
		aws.ToString(
			config.DeploymentConfigInfo.DeploymentConfigId,
		) != aws.ToString(
			configCreated.DeploymentConfigId,
		) ||
		config.DeploymentConfigInfo.MinimumHealthyHosts.Value != 75 {
		t.Fatal(config, err)
	}

	configs, err := c.ListDeploymentConfigs(ctx, &sdk.ListDeploymentConfigsInput{})
	if err != nil || len(configs.DeploymentConfigsList) != 18 {
		t.Fatal(configs, err)
	}

	for _, platform := range []types.ComputePlatform{types.ComputePlatformLambda, types.ComputePlatformEcs} {
		_, err = c.CreateDeploymentConfig(ctx, &sdk.CreateDeploymentConfigInput{
			DeploymentConfigName: new("canary-" + string(platform)),
			ComputePlatform:      platform,
			TrafficRoutingConfig: &types.TrafficRoutingConfig{
				Type:            types.TrafficRoutingTypeTimeBasedCanary,
				TimeBasedCanary: &types.TimeBasedCanary{CanaryPercentage: 10, CanaryInterval: 5},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	group := new("sdk-group")
	g, err := c.CreateDeploymentGroup(ctx, &sdk.CreateDeploymentGroupInput{
		ApplicationName:      app,
		DeploymentGroupName:  group,
		DeploymentConfigName: cfg,
		ServiceRoleArn: new(
			"arn:aws:iam::000000000000:role/deploy",
		),
		AutoScalingGroups: []string{"workers"},
		Tags:              []types.Tag{{Key: new("env"), Value: new("dev")}},
		Ec2TagFilters: []types.EC2TagFilter{
			{Type: types.EC2TagFilterTypeKeyAndValue, Key: new("env"), Value: new("dev")},
		},
		OnPremisesInstanceTagFilters: []types.TagFilter{{Type: types.TagFilterTypeKeyOnly, Key: new("registered")}},
		AutoRollbackConfiguration: &types.AutoRollbackConfiguration{
			Enabled: true,
			Events:  []types.AutoRollbackEvent{types.AutoRollbackEventDeploymentFailure},
		},
		AlarmConfiguration: &types.AlarmConfiguration{
			Enabled: true,
			Alarms:  []types.Alarm{{Name: new("alert")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	dg, err := c.GetDeploymentGroup(ctx, &sdk.GetDeploymentGroupInput{ApplicationName: app, DeploymentGroupName: group})
	if err != nil || aws.ToString(dg.DeploymentGroupInfo.DeploymentGroupId) != aws.ToString(g.DeploymentGroupId) ||
		aws.ToString(dg.DeploymentGroupInfo.AutoScalingGroups[0].Name) != "workers" {
		t.Fatal(dg, err)
	}

	groups, err := c.ListDeploymentGroups(ctx, &sdk.ListDeploymentGroupsInput{ApplicationName: app})
	if err != nil || len(groups.DeploymentGroups) != 1 {
		t.Fatal(groups, err)
	}

	bg, err := c.BatchGetDeploymentGroups(
		ctx,
		&sdk.BatchGetDeploymentGroupsInput{ApplicationName: app, DeploymentGroupNames: []string{*group, "missing"}},
	)
	if err != nil || len(bg.DeploymentGroupsInfo) != 1 || bg.ErrorMessage == nil {
		t.Fatal(bg, err)
	}

	_, err = c.DeleteDeploymentConfig(ctx, &sdk.DeleteDeploymentConfigInput{DeploymentConfigName: cfg})
	wantCode(t, err, "DeploymentConfigInUseException")
	_, err = c.UpdateDeploymentGroup(
		ctx,
		&sdk.UpdateDeploymentGroupInput{
			ApplicationName:            app,
			CurrentDeploymentGroupName: group,
			NewDeploymentGroupName:     new("renamed"),
			DeploymentConfigName:       new("CodeDeployDefault.AllAtOnce"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	group = new("renamed")
	_, err = c.UpdateApplication(
		ctx,
		&sdk.UpdateApplicationInput{ApplicationName: app, NewApplicationName: new("renamed-app")},
	)
	if err != nil {
		t.Fatal(err)
	}

	app = new("renamed-app")
	arns := []string{
		"arn:aws:codedeploy:us-east-1:000000000000:application:renamed-app",
		"arn:aws:codedeploy:us-east-1:000000000000:deploymentgroup:renamed-app/renamed",
	}

	for _, arn := range arns {
		_, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: new(arn), Tags: []types.Tag{{Key: new("owner"), Value: new("team")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(arn)})
		if e != nil || len(tags.Tags) != 2 {
			t.Fatal(tags, e)
		}

		_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"env"}})
		if err != nil {
			t.Fatal(err)
		}
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: arns, Tags: map[string]string{"rgta": "works"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	untagged, err := tagging.UntagResources(
		ctx,
		&rgta.UntagResourcesInput{ResourceARNList: arns, TagKeys: []string{"rgta"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal(untagged, err)
	}

	deleted, err := c.DeleteDeploymentGroup(
		ctx,
		&sdk.DeleteDeploymentGroupInput{ApplicationName: app, DeploymentGroupName: group},
	)
	if err != nil || len(deleted.HooksNotCleanedUp) != 0 {
		t.Fatal(deleted, err)
	}

	_, err = c.DeleteDeploymentConfig(ctx, &sdk.DeleteDeploymentConfigInput{DeploymentConfigName: cfg})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApplication(ctx, &sdk.DeleteApplicationInput{ApplicationName: app})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationName: app})
	wantCode(t, err, "ApplicationDoesNotExistException")
	_, err = c.CreateDeployment(ctx, &sdk.CreateDeploymentInput{ApplicationName: app})
	if err == nil {
		t.Fatal("deployment execution must be unsupported")
	}
}

func TestCodeDeployECSRecordsAndRejectedUpdate(t *testing.T) {
	s := server(t)
	c := codeDeployClient(s)
	ctx := context.Background()
	app := new("ecs-app")
	_, err := c.CreateApplication(
		ctx,
		&sdk.CreateApplicationInput{ApplicationName: app, ComputePlatform: types.ComputePlatformEcs},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateDeploymentGroup(ctx, &sdk.CreateDeploymentGroupInput{
		ApplicationName: app, DeploymentGroupName: new("ecs-group"),
		ServiceRoleArn:       new("arn:aws:iam::000000000000:role/deploy"),
		DeploymentConfigName: new("CodeDeployDefault.ECSAllAtOnce"),
		EcsServices:          []types.ECSService{{ClusterName: new("cluster"), ServiceName: new("service")}},
		BlueGreenDeploymentConfiguration: &types.BlueGreenDeploymentConfiguration{
			DeploymentReadyOption: &types.DeploymentReadyOption{
				ActionOnTimeout: types.DeploymentReadyActionContinueDeployment,
			},
			TerminateBlueInstancesOnDeploymentSuccess: &types.BlueInstanceTerminationOption{
				Action:                       types.InstanceActionTerminate,
				TerminationWaitTimeInMinutes: 5,
			},
		},
		LoadBalancerInfo: &types.LoadBalancerInfo{TargetGroupPairInfoList: []types.TargetGroupPairInfo{
			{
				TargetGroups: []types.TargetGroupInfo{{Name: new("blue")}, {Name: new("green")}},
				ProdTrafficRoute: &types.TrafficRoute{
					ListenerArns: []string{
						"arn:aws:elasticloadbalancing:us-east-1:000000000000:listener/app/lb/blue/prod",
					},
				},
				TestTrafficRoute: &types.TrafficRoute{
					ListenerArns: []string{
						"arn:aws:elasticloadbalancing:us-east-1:000000000000:listener/app/lb/green/test",
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateDeploymentGroup(ctx, &sdk.UpdateDeploymentGroupInput{
		ApplicationName:            app,
		CurrentDeploymentGroupName: new("ecs-group"),
		NewDeploymentGroupName:     new("invalid-change"),
		DeploymentStyle:            &types.DeploymentStyle{DeploymentType: types.DeploymentTypeInPlace},
	})
	wantCode(t, err, "InvalidDeploymentStyleException")
	got, err := c.GetDeploymentGroup(
		ctx,
		&sdk.GetDeploymentGroupInput{ApplicationName: app, DeploymentGroupName: new("ecs-group")},
	)
	if err != nil || got.DeploymentGroupInfo.DeploymentStyle.DeploymentType != types.DeploymentTypeBlueGreen ||
		aws.ToString(
			got.DeploymentGroupInfo.DeploymentConfigName,
		) != "CodeDeployDefault.ECSAllAtOnce" || len(got.DeploymentGroupInfo.EcsServices) != 1 {
		t.Fatal(got, err)
	}

	_, err = c.DeleteApplication(ctx, &sdk.DeleteApplicationInput{ApplicationName: app})
	if err != nil {
		t.Fatal(err)
	}
}

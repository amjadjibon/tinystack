package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cf "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cft "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/codedeploy"
)

func codeDeployTemplate(configName, role string, tag bool, unsupported string) string {
	app := map[string]any{"ApplicationName": "cfn-app"}
	group := map[string]any{
		"ApplicationName": map[string]any{"Ref": "App"}, "DeploymentGroupName": "cfn-group",
		"DeploymentConfigName": map[string]any{"Ref": "Config"}, "ServiceRoleArn": role,
	}

	if tag {
		app["Tags"] = []any{map[string]any{"Key": "env", "Value": "dev"}}
		group["Tags"] = []any{map[string]any{"Key": "env", "Value": "dev"}}
		group["AutoScalingGroups"] = []any{"workers"}
		group["AlarmConfiguration"] = map[string]any{"Enabled": true, "Alarms": []any{map[string]any{"Name": "alert"}}}
		group["AutoRollbackConfiguration"] = map[string]any{"Enabled": true, "Events": []any{"DEPLOYMENT_FAILURE"}}
	}

	config := map[string]any{
		"DeploymentConfigName": configName,
		"MinimumHealthyHosts":  map[string]any{"Type": "FLEET_PERCENT", "Value": 75},
	}

	if unsupported == "ZonalConfig" {
		config[unsupported] = map[string]any{}
	} else if unsupported != "" {
		group[unsupported] = map[string]any{}
	}

	data, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"App":    map[string]any{"Type": "AWS::CodeDeploy::Application", "Properties": app},
			"Config": map[string]any{"Type": "AWS::CodeDeploy::DeploymentConfig", "Properties": config},
			"Group":  map[string]any{"Type": "AWS::CodeDeploy::DeploymentGroup", "Properties": group},
		},
		"Outputs": map[string]any{
			"App": map[string]any{"Value": map[string]any{"Ref": "App"}},
			"Config": map[string]any{
				"Value": map[string]any{"Ref": "Config"},
			},
			"Group": map[string]any{"Value": map[string]any{"Ref": "Group"}},
		},
	})
	return string(data)
}

func TestCloudFormationCodeDeployLifecycleAndUnsupportedUpdates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	cd := codeDeployClient(s)
	role := "arn:aws:iam::000000000000:role/first"
	_, err := c.CreateStack(
		ctx,
		&cf.CreateStackInput{
			StackName:    new("cd-stack"),
			TemplateBody: new(codeDeployTemplate("cfn-config", role, true, "")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, c, "cd-stack", cft.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	if outputs["App"] != "cfn-app" || outputs["Config"] != "cfn-config" || outputs["Group"] != "cfn-group" {
		t.Fatal(outputs)
	}

	before, err := cd.GetDeploymentGroup(
		ctx,
		&sdk.GetDeploymentGroupInput{ApplicationName: new("cfn-app"), DeploymentGroupName: new("cfn-group")},
	)
	if err != nil {
		t.Fatal(err)
	}

	role = "arn:aws:iam::000000000000:role/second"
	_, err = c.UpdateStack(
		ctx,
		&cf.UpdateStackInput{
			StackName:    new("cd-stack"),
			TemplateBody: new(codeDeployTemplate("cfn-config-2", role, false, "")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "cd-stack", cft.StackStatusUpdateComplete)
	after, err := cd.GetDeploymentGroup(
		ctx,
		&sdk.GetDeploymentGroupInput{ApplicationName: new("cfn-app"), DeploymentGroupName: new("cfn-group")},
	)
	if err != nil || aws.ToString(after.DeploymentGroupInfo.ServiceRoleArn) != role ||
		aws.ToString(after.DeploymentGroupInfo.DeploymentConfigName) != "cfn-config-2" ||
		aws.ToString(
			after.DeploymentGroupInfo.DeploymentGroupId,
		) != aws.ToString(
			before.DeploymentGroupInfo.DeploymentGroupId,
		) ||
		len(
			after.DeploymentGroupInfo.AutoScalingGroups,
		) != 0 || len(after.DeploymentGroupInfo.AlarmConfiguration.Alarms) != 0 ||
		len(after.DeploymentGroupInfo.AutoRollbackConfiguration.Events) != 0 {
		t.Fatal(after, err)
	}

	_, err = cd.GetDeploymentConfig(ctx, &sdk.GetDeploymentConfigInput{DeploymentConfigName: new("cfn-config")})
	wantCode(t, err, "DeploymentConfigDoesNotExistException")
	for _, kind := range []string{"application:cfn-app", "deploymentgroup:cfn-app/cfn-group"} {
		tags, e := cd.ListTagsForResource(
			ctx,
			&sdk.ListTagsForResourceInput{ResourceArn: new("arn:aws:codedeploy:us-east-1:000000000000:" + kind)},
		)
		if e != nil || len(tags.Tags) != 0 {
			t.Fatal(tags, e)
		}
	}

	for _, property := range []string{
		"Deployment", "TriggerConfigurations", "TerminationHookEnabled", "OutdatedInstancesStrategy",
		"Ec2TagSet", "OnPremisesTagSet",
		"ZonalConfig",
	} {
		t.Run(property, func(t *testing.T) {
			configName := "cfn-config-2"
			if property == "ZonalConfig" {
				configName = "cfn-config-3"
			}

			_, err = c.UpdateStack(
				ctx,
				&cf.UpdateStackInput{
					StackName:    new("cd-stack"),
					TemplateBody: new(codeDeployTemplate(configName, role, false, property)),
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			wantStatus(ctx, t, c, "cd-stack", cft.StackStatusUpdateRollbackComplete)
			got, e := cd.GetDeploymentGroup(
				ctx,
				&sdk.GetDeploymentGroupInput{ApplicationName: new("cfn-app"), DeploymentGroupName: new("cfn-group")},
			)
			if e != nil || aws.ToString(got.DeploymentGroupInfo.ServiceRoleArn) != role {
				t.Fatal(got, e)
			}
		})
	}

	_, err = c.DeleteStack(ctx, &cf.DeleteStackInput{StackName: stack.StackId})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, *stack.StackId, cft.StackStatusDeleteComplete)
	_, err = cd.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationName: new("cfn-app")})
	wantCode(t, err, "ApplicationDoesNotExistException")
	_, err = cd.GetDeploymentConfig(ctx, &sdk.GetDeploymentConfigInput{DeploymentConfigName: new("cfn-config-2")})
	wantCode(t, err, "DeploymentConfigDoesNotExistException")
}

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/applicationautoscaling"
	"github.com/aws/aws-sdk-go-v2/service/applicationautoscaling/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

func applicationScalingTemplate(maximum int, targetValue float64) string {
	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"Target": map[string]any{
				"Type": "AWS::ApplicationAutoScaling::ScalableTarget",
				"Properties": map[string]any{
					"ServiceNamespace":  "ecs",
					"ResourceId":        "service/cluster/cf",
					"ScalableDimension": "ecs:service:DesiredCount",
					"MinCapacity":       1,
					"MaxCapacity":       maximum,
				},
			},
			"Policy": map[string]any{"Type": "AWS::ApplicationAutoScaling::ScalingPolicy", "Properties": map[string]any{
				"PolicyName":      "cf-cpu",
				"PolicyType":      "TargetTrackingScaling",
				"ScalingTargetId": map[string]any{"Ref": "Target"},
				"TargetTrackingScalingPolicyConfiguration": map[string]any{
					"TargetValue": targetValue,
					"PredefinedMetricSpecification": map[string]any{
						"PredefinedMetricType": "ECSServiceAverageCPUUtilization",
					},
				},
			}},
		},
		"Outputs": map[string]any{
			"TargetRef": map[string]any{"Value": map[string]any{"Ref": "Target"}},
			"PolicyRef": map[string]any{"Value": map[string]any{"Ref": "Policy"}},
			"PolicyArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Policy", "Arn"}}},
		},
	})
	return string(raw)
}

func TestCloudFormationApplicationAutoScalingLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := applicationScalingClient(s, "test", "us-east-1")
	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("application-scaling"),
			TemplateBody: new(applicationScalingTemplate(10, 50)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, "application-scaling", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)
	if outputs["TargetRef"] != "service/cluster/cf|ecs:service:DesiredCount|ecs" ||
		outputs["PolicyRef"] != outputs["PolicyArn"] ||
		!strings.Contains(outputs["PolicyRef"], ":policyName/cf-cpu") {
		t.Fatal(outputs)
	}

	before, err := c.DescribeScalableTargets(
		ctx,
		&sdk.DescribeScalableTargetsInput{
			ServiceNamespace: types.ServiceNamespaceEcs,
			ResourceIds:      []string{"service/cluster/cf"},
		},
	)
	if err != nil || len(before.ScalableTargets) != 1 {
		t.Fatal(before, err)
	}

	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName:    new("application-scaling"),
			TemplateBody: new(applicationScalingTemplate(20, 70)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, "application-scaling", cfntypes.StackStatusUpdateComplete)
	if outputsOf(&st)["PolicyRef"] != outputs["PolicyRef"] {
		t.Fatal("Policy ARN changed")
	}

	after, err := c.DescribeScalableTargets(
		ctx,
		&sdk.DescribeScalableTargetsInput{
			ServiceNamespace: types.ServiceNamespaceEcs,
			ResourceIds:      []string{"service/cluster/cf"},
		},
	)
	if err != nil || len(after.ScalableTargets) != 1 || *after.ScalableTargets[0].MaxCapacity != 20 ||
		*after.ScalableTargets[0].ScalableTargetARN != *before.ScalableTargets[0].ScalableTargetARN {
		t.Fatal(after, err)
	}

	policies, err := c.DescribeScalingPolicies(
		ctx,
		&sdk.DescribeScalingPoliciesInput{
			ServiceNamespace: types.ServiceNamespaceEcs,
			ResourceId:       new("service/cluster/cf"),
		},
	)
	if err != nil || len(policies.ScalingPolicies) != 1 ||
		*policies.ScalingPolicies[0].TargetTrackingScalingPolicyConfiguration.TargetValue != 70 {
		t.Fatal(policies, err)
	}

	// Removing a conditional configuration must not silently retain live metadata.
	var missingConfiguration map[string]any
	if err = json.Unmarshal([]byte(applicationScalingTemplate(20, 70)), &missingConfiguration); err != nil {
		t.Fatal(err)
	}

	resources := missingConfiguration["Resources"].(map[string]any)
	policyProperties := resources["Policy"].(map[string]any)["Properties"].(map[string]any)
	delete(policyProperties, "TargetTrackingScalingPolicyConfiguration")
	missingTemplate, err := json.Marshal(missingConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("application-scaling"), TemplateBody: new(string(missingTemplate))},
	)
	if err == nil {
		wantStatus(ctx, t, cf, "application-scaling", cfntypes.StackStatusUpdateRollbackComplete)
	} else {
		code(t, err, "ValidationError")
	}

	// Nested CF scheduled actions are explicitly outside this slice, including empty arrays.
	bad := strings.Replace(
		applicationScalingTemplate(20, 70),
		`"MaxCapacity":20`,
		`"ScheduledActions":[],"MaxCapacity":20`,
		1,
	)
	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("application-scaling"), TemplateBody: new(bad)})
	if err == nil {
		wantStatus(ctx, t, cf, "application-scaling", cfntypes.StackStatusUpdateRollbackComplete)
	} else {
		code(t, err, "ValidationError")
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("application-scaling")})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	after, err = c.DescribeScalableTargets(
		ctx,
		&sdk.DescribeScalableTargetsInput{
			ServiceNamespace: types.ServiceNamespaceEcs,
			ResourceIds:      []string{"service/cluster/cf"},
		},
	)
	if err != nil || len(after.ScalableTargets) != 0 {
		t.Fatal(after, err)
	}

	policies, err = c.DescribeScalingPolicies(
		ctx,
		&sdk.DescribeScalingPoliciesInput{
			ServiceNamespace: types.ServiceNamespaceEcs,
			ResourceId:       new("service/cluster/cf"),
		},
	)
	if err != nil || len(policies.ScalingPolicies) != 0 {
		t.Fatal(policies, err)
	}
}

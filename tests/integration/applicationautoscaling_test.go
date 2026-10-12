package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/applicationautoscaling"
	"github.com/aws/aws-sdk-go-v2/service/applicationautoscaling/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func applicationScalingClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func applicationScalingTarget(resource string) *sdk.RegisterScalableTargetInput {
	return &sdk.RegisterScalableTargetInput{
		ServiceNamespace:  types.ServiceNamespaceEcs,
		ResourceId:        new(resource),
		ScalableDimension: types.ScalableDimensionECSServiceDesiredCount,
		MinCapacity:       new(int32(1)),
		MaxCapacity:       new(int32(10)),
	}
}

func TestApplicationAutoScalingSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := applicationScalingClient(s, "test", "us-east-1")
	in := applicationScalingTarget("service/cluster/api")
	in.Tags = map[string]string{"owner": "test"}
	out, err := c.RegisterScalableTarget(ctx, in)
	if err != nil ||
		!strings.HasPrefix(aws.ToString(out.ScalableTargetARN), "arn:aws:application-autoscaling:us-east-1:") {
		t.Fatal(out, err)
	}

	arn := out.ScalableTargetARN
	in.Tags = nil
	in.MinCapacity = new(int32(2))
	in.SuspendedState = &types.SuspendedState{DynamicScalingInSuspended: new(true)}
	out, err = c.RegisterScalableTarget(ctx, in)
	if err != nil || *out.ScalableTargetARN != *arn {
		t.Fatal(out, err)
	}

	for _, resource := range []string{"service/cluster/worker", "service/cluster/other"} {
		if _, err = c.RegisterScalableTarget(ctx, applicationScalingTarget(resource)); err != nil {
			t.Fatal(err)
		}
	}

	targets, err := c.DescribeScalableTargets(
		ctx,
		&sdk.DescribeScalableTargetsInput{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceIds:       []string{*in.ResourceId},
			ScalableDimension: in.ScalableDimension,
		},
	)
	if err != nil || len(targets.ScalableTargets) != 1 || *targets.ScalableTargets[0].MinCapacity != 2 ||
		!*targets.ScalableTargets[0].SuspendedState.DynamicScalingInSuspended {
		t.Fatal(targets, err)
	}

	paginator := sdk.NewDescribeScalableTargetsPaginator(
		c,
		&sdk.DescribeScalableTargetsInput{ServiceNamespace: in.ServiceNamespace, MaxResults: new(int32(1))},
	)
	count := 0
	for paginator.HasMorePages() {
		page, e := paginator.NextPage(ctx)
		if e != nil {
			t.Fatal(e)
		}

		count += len(page.ScalableTargets)
	}

	if count != 3 {
		t.Fatal(count)
	}

	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceARN: arn, Tags: map[string]string{"added": "yes"}},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceARN: arn, TagKeys: []string{"owner"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: arn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["added"] != "yes" {
		t.Fatal(tags, err)
	}

	taggingClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := taggingClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*arn}, Tags: map[string]string{"rgta": "works"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	untagged, err := taggingClient.UntagResources(
		ctx,
		&rgta.UntagResourcesInput{ResourceARNList: []string{*arn}, TagKeys: []string{"rgta"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal(untagged, err)
	}

	p := &sdk.PutScalingPolicyInput{
		ServiceNamespace:  in.ServiceNamespace,
		ResourceId:        in.ResourceId,
		ScalableDimension: in.ScalableDimension,
		PolicyName:        new("cpu"),
		PolicyType:        types.PolicyTypeTargetTrackingScaling,
		TargetTrackingScalingPolicyConfiguration: &types.TargetTrackingScalingPolicyConfiguration{
			TargetValue:     new(50.0),
			ScaleInCooldown: new(int32(60)),
			DisableScaleIn:  new(true),
			PredefinedMetricSpecification: &types.PredefinedMetricSpecification{
				PredefinedMetricType: types.MetricTypeECSServiceAverageCPUUtilization,
			},
		},
	}

	po, err := c.PutScalingPolicy(ctx, p)
	if err != nil ||
		!strings.Contains(aws.ToString(po.PolicyARN), ":resource/ecs/service/cluster/api:policyName/cpu") ||
		len(po.Alarms) != 0 {
		t.Fatal(po, err)
	}

	policyARN := po.PolicyARN
	p.TargetTrackingScalingPolicyConfiguration.TargetValue = new(70.0)
	po, err = c.PutScalingPolicy(ctx, p)
	if err != nil || *po.PolicyARN != *policyARN {
		t.Fatal(po, err)
	}

	policies, err := c.DescribeScalingPolicies(
		ctx,
		&sdk.DescribeScalingPoliciesInput{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceId:        in.ResourceId,
			ScalableDimension: in.ScalableDimension,
			PolicyNames:       []string{"cpu"},
		},
	)
	if err != nil || len(policies.ScalingPolicies) != 1 ||
		*policies.ScalingPolicies[0].TargetTrackingScalingPolicyConfiguration.TargetValue != 70 ||
		len(policies.ScalingPolicies[0].Alarms) != 0 {
		t.Fatal(policies, err)
	}

	if _, err = c.DeleteScalingPolicy(
		ctx,
		&sdk.DeleteScalingPolicyInput{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceId:        in.ResourceId,
			ScalableDimension: in.ScalableDimension,
			PolicyName:        p.PolicyName,
		},
	); err != nil {
		t.Fatal(err)
	}

	p.PolicyName = new("custom")
	p.TargetTrackingScalingPolicyConfiguration.PredefinedMetricSpecification = nil
	p.TargetTrackingScalingPolicyConfiguration.CustomizedMetricSpecification = &types.CustomizedMetricSpecification{
		MetricName: new("Load"),
		Namespace:  new("Example"),
		Statistic:  types.MetricStatisticAverage,
		Dimensions: []types.MetricDimension{{Name: new("Queue"), Value: new("api")}},
	}

	if _, err = c.PutScalingPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	p.PolicyName = new("step")
	p.PolicyType = types.PolicyTypeStepScaling
	p.TargetTrackingScalingPolicyConfiguration = nil
	p.StepScalingPolicyConfiguration = &types.StepScalingPolicyConfiguration{
		AdjustmentType:  types.AdjustmentTypeChangeInCapacity,
		StepAdjustments: []types.StepAdjustment{{MetricIntervalLowerBound: new(0.0), ScalingAdjustment: new(int32(2))}},
	}

	if _, err = c.PutScalingPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	a := &sdk.PutScheduledActionInput{
		ServiceNamespace:     in.ServiceNamespace,
		ResourceId:           in.ResourceId,
		ScalableDimension:    in.ScalableDimension,
		ScheduledActionName:  new("morning"),
		Schedule:             new("cron(0 8 * * ? *)"),
		Timezone:             new("Asia/Kuala_Lumpur"),
		ScalableTargetAction: &types.ScalableTargetAction{MinCapacity: new(int32(3)), MaxCapacity: new(int32(8))},
	}

	if _, err = c.PutScheduledAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	a.ScalableTargetAction.MinCapacity = new(int32(4))
	if _, err = c.PutScheduledAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	actions, err := c.DescribeScheduledActions(
		ctx,
		&sdk.DescribeScheduledActionsInput{
			ServiceNamespace:     in.ServiceNamespace,
			ResourceId:           in.ResourceId,
			ScalableDimension:    in.ScalableDimension,
			ScheduledActionNames: []string{"morning"},
		},
	)
	if err != nil || len(actions.ScheduledActions) != 1 ||
		*actions.ScheduledActions[0].ScalableTargetAction.MinCapacity != 4 ||
		*actions.ScheduledActions[0].Timezone != *a.Timezone {
		t.Fatal(actions, err)
	}

	if _, err = c.DeleteScheduledAction(
		ctx,
		&sdk.DeleteScheduledActionInput{
			ServiceNamespace:    in.ServiceNamespace,
			ResourceId:          in.ResourceId,
			ScalableDimension:   in.ScalableDimension,
			ScheduledActionName: a.ScheduledActionName,
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutScheduledAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeregisterScalableTarget(
		ctx,
		&sdk.DeregisterScalableTargetInput{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceId:        in.ResourceId,
			ScalableDimension: in.ScalableDimension,
		},
	); err != nil {
		t.Fatal(err)
	}

	policies, err = c.DescribeScalingPolicies(
		ctx,
		&sdk.DescribeScalingPoliciesInput{ServiceNamespace: in.ServiceNamespace, ResourceId: in.ResourceId},
	)
	if err != nil || len(policies.ScalingPolicies) != 0 {
		t.Fatal(policies, err)
	}

	actions, err = c.DescribeScheduledActions(
		ctx,
		&sdk.DescribeScheduledActionsInput{ServiceNamespace: in.ServiceNamespace, ResourceId: in.ResourceId},
	)
	if err != nil || len(actions.ScheduledActions) != 0 {
		t.Fatal(actions, err)
	}

	_, err = c.PutScalingPolicy(ctx, p)
	code(t, err, "ObjectNotFoundException")
	var missing *types.ObjectNotFoundException
	if !errors.As(err, &missing) {
		t.Fatal("Expected modeled SDK error", err)
	}

	bad := applicationScalingTarget("service/cluster/bad")
	bad.MinCapacity = new(int32(11))
	_, err = c.RegisterScalableTarget(ctx, bad)
	code(t, err, "ValidationException")
	var invalid *types.ValidationException
	if !errors.As(err, &invalid) {
		t.Fatal(err)
	}

	// The generated decoder must reject excluded metric math, even an empty list.
	p.TargetTrackingScalingPolicyConfiguration = &types.TargetTrackingScalingPolicyConfiguration{
		TargetValue: new(1.0),
		CustomizedMetricSpecification: &types.CustomizedMetricSpecification{
			Metrics: []types.TargetTrackingMetricDataQuery{},
		},
	}

	p.StepScalingPolicyConfiguration = nil
	p.PolicyType = types.PolicyTypeTargetTrackingScaling
	_, err = c.PutScalingPolicy(ctx, p)
	code(t, err, "ValidationException")
}

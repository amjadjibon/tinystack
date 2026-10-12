package applicationautoscaling

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func targetInput(resource string) *api.RegisterScalableTargetRequest {
	return &api.RegisterScalableTargetRequest{
		ServiceNamespace:  new("ecs"),
		ResourceId:        new(resource),
		ScalableDimension: new("ecs:service:DesiredCount"),
		MinCapacity:       new(int64(1)),
		MaxCapacity:       new(int64(10)),
	}
}

func policyInput(resource, name string) *api.PutScalingPolicyRequest {
	return &api.PutScalingPolicyRequest{
		ServiceNamespace:  new("ecs"),
		ResourceId:        new(resource),
		ScalableDimension: new("ecs:service:DesiredCount"),
		PolicyName:        new(name),
		PolicyType:        new("TargetTrackingScaling"),
		TargetTrackingScalingPolicyConfiguration: &api.TargetTrackingScalingPolicyConfiguration{
			TargetValue: new(50.0),
			PredefinedMetricSpecification: &api.PredefinedMetricSpecification{
				PredefinedMetricType: new("ECSServiceAverageCPUUtilization"),
			},
		},
	}
}

func actionInput(resource, name string) *api.PutScheduledActionRequest {
	return &api.PutScheduledActionRequest{
		ServiceNamespace:     new("ecs"),
		ResourceId:           new(resource),
		ScalableDimension:    new("ecs:service:DesiredCount"),
		ScheduledActionName:  new(name),
		Schedule:             new("rate(1 day)"),
		ScalableTargetAction: &api.ScalableTargetAction{MinCapacity: new(int64(2))},
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestRecordsLifecycleUpsertsTagsAndCascade(t *testing.T) {
	s := New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")
	in := targetInput("service/cluster/api")
	in.Tags = map[string]string{"owner": "test"}
	out, err := s.RegisterScalableTarget(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	original := *out.ScalableTargetARN
	in.Tags["owner"] = "mutated"
	in.MinCapacity = new(int64(3))
	in.RoleARN = new("arn:aws:iam::123456789012:role/custom")
	in.Tags = nil
	in.SuspendedState = &api.SuspendedState{DynamicScalingInSuspended: new(true)}
	out, err = s.RegisterScalableTarget(ctx, in)
	if err != nil || *out.ScalableTargetARN != original {
		t.Fatal(out, err)
	}

	targets, err := s.DescribeScalableTargets(
		ctx,
		&api.DescribeScalableTargetsRequest{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceIds:       []string{*in.ResourceId},
			ScalableDimension: in.ScalableDimension,
		},
	)
	if err != nil || len(targets.ScalableTargets) != 1 {
		t.Fatal(targets, err)
	}

	live := targets.ScalableTargets[0]
	if *live.MinCapacity != 3 || *live.RoleARN != *in.RoleARN || !*live.SuspendedState.DynamicScalingInSuspended ||
		*live.SuspendedState.DynamicScalingOutSuspended {
		t.Fatal(live)
	}

	_, err = s.TagResource(
		ctx,
		&api.TagResourceRequest{ResourceARN: out.ScalableTargetARN, Tags: map[string]string{"added": "yes"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{ResourceARN: out.ScalableTargetARN, TagKeys: []string{"owner"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceARN: out.ScalableTargetARN})
	if err != nil || !reflect.DeepEqual(tags.Tags, map[string]string{"added": "yes"}) {
		t.Fatal(tags, err)
	}

	p := policyInput(*in.ResourceId, "cpu")
	po, err := s.PutScalingPolicy(ctx, p)
	if err != nil || !strings.Contains(*po.PolicyARN, ":resource/ecs/service/cluster/api:policyName/cpu") ||
		len(po.Alarms) != 0 {
		t.Fatal(po, err)
	}

	p.TargetTrackingScalingPolicyConfiguration.TargetValue = new(60.0)
	next, err := s.PutScalingPolicy(ctx, p)
	if err != nil || *next.PolicyARN != *po.PolicyARN {
		t.Fatal(next, err)
	}

	policies, err := s.DescribeScalingPolicies(
		ctx,
		&api.DescribeScalingPoliciesRequest{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceId:        in.ResourceId,
			ScalableDimension: in.ScalableDimension,
			PolicyNames:       []string{"cpu"},
		},
	)
	if err != nil || len(policies.ScalingPolicies) != 1 ||
		*policies.ScalingPolicies[0].TargetTrackingScalingPolicyConfiguration.TargetValue != 60 {
		t.Fatal(policies, err)
	}

	_, err = s.DeleteScalingPolicy(
		ctx,
		&api.DeleteScalingPolicyRequest{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceId:        in.ResourceId,
			ScalableDimension: in.ScalableDimension,
			PolicyName:        p.PolicyName,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// A customized metric and a step policy are ordinary stored metadata too.
	p.TargetTrackingScalingPolicyConfiguration.CustomizedMetricSpecification = &api.CustomizedMetricSpecification{
		MetricName: new("Load"),
		Namespace:  new("App"),
		Statistic:  new("Average"),
		Unit:       new("Count"),
		Dimensions: []api.MetricDimension{{Name: new("Queue"), Value: new("api")}},
	}

	p.TargetTrackingScalingPolicyConfiguration.PredefinedMetricSpecification = nil
	if _, err = s.PutScalingPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	p.PolicyName = new("steps")
	p.PolicyType = new("StepScaling")
	p.TargetTrackingScalingPolicyConfiguration = nil
	p.StepScalingPolicyConfiguration = &api.StepScalingPolicyConfiguration{
		AdjustmentType:        new("ChangeInCapacity"),
		Cooldown:              new(int64(60)),
		MetricAggregationType: new("Average"),
		StepAdjustments: []api.StepAdjustment{
			{MetricIntervalLowerBound: new(0.0), ScalingAdjustment: new(int64(1))},
		},
	}

	if _, err = s.PutScalingPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	a := actionInput(*in.ResourceId, "morning")
	if _, err = s.PutScheduledAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	a.Schedule = new("cron(0 8 * * ? *)")
	a.Timezone = new("Asia/Kuala_Lumpur")
	a.StartTime = new(1900000000.0)
	a.EndTime = new(2000000000.0)
	if _, err = s.PutScheduledAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	actions, err := s.DescribeScheduledActions(
		ctx,
		&api.DescribeScheduledActionsRequest{
			ServiceNamespace:     in.ServiceNamespace,
			ResourceId:           in.ResourceId,
			ScalableDimension:    in.ScalableDimension,
			ScheduledActionNames: []string{"morning"},
		},
	)
	if err != nil || len(actions.ScheduledActions) != 1 || *actions.ScheduledActions[0].Schedule != *a.Schedule {
		t.Fatal(actions, err)
	}

	arn := *actions.ScheduledActions[0].ScheduledActionARN
	if _, err = s.DeleteScheduledAction(
		ctx,
		&api.DeleteScheduledActionRequest{
			ServiceNamespace:    in.ServiceNamespace,
			ResourceId:          in.ResourceId,
			ScalableDimension:   in.ScalableDimension,
			ScheduledActionName: a.ScheduledActionName,
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = s.PutScheduledAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	actions, err = s.DescribeScheduledActions(
		ctx,
		&api.DescribeScheduledActionsRequest{ServiceNamespace: in.ServiceNamespace},
	)
	if err != nil || *actions.ScheduledActions[0].ScheduledActionARN == arn {
		t.Fatal(actions, err)
	}

	inventory, err := s.Resources(ctx)
	if err != nil || len(inventory) != 4 {
		t.Fatal(inventory, err)
	}

	for _, r := range inventory {
		if r.Details.(map[string]any)["RecordsOnly"] != true {
			t.Fatal(r)
		}
	}

	if _, err = s.DeregisterScalableTarget(
		ctx,
		&api.DeregisterScalableTargetRequest{
			ServiceNamespace:  in.ServiceNamespace,
			ResourceId:        in.ResourceId,
			ScalableDimension: in.ScalableDimension,
		},
	); err != nil {
		t.Fatal(err)
	}

	policies, err = s.DescribeScalingPolicies(
		ctx,
		&api.DescribeScalingPoliciesRequest{ServiceNamespace: in.ServiceNamespace},
	)
	if err != nil || len(policies.ScalingPolicies) != 0 {
		t.Fatal(policies, err)
	}

	actions, err = s.DescribeScheduledActions(
		ctx,
		&api.DescribeScheduledActionsRequest{ServiceNamespace: in.ServiceNamespace},
	)
	if err != nil || len(actions.ScheduledActions) != 0 {
		t.Fatal(actions, err)
	}

	_, err = s.PutScalingPolicy(ctx, policyInput(*in.ResourceId, "missing"))
	requireCode(t, err, "ObjectNotFoundException")
	_, err = s.PutScheduledAction(ctx, a)
	requireCode(t, err, "ObjectNotFoundException")
	_, err = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceARN: new(original)})
	requireCode(t, err, "ResourceNotFoundException")
}

func TestValidationAndScopeIsolation(t *testing.T) {
	s := New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")
	in := targetInput("service/cluster/api")
	for _, change := range []func(*api.RegisterScalableTargetRequest){
		func(i *api.RegisterScalableTargetRequest) { i.ServiceNamespace = new("invalid") },
		func(i *api.RegisterScalableTargetRequest) {
			i.ScalableDimension = new("dynamodb:table:ReadCapacityUnits")
		},
		func(i *api.RegisterScalableTargetRequest) { i.MinCapacity = new(int64(11)) },
		func(i *api.RegisterScalableTargetRequest) { i.MaxCapacity = nil },
	} {
		bad := common.Clone(in)
		change(bad)
		_, err := s.RegisterScalableTarget(ctx, bad)
		requireCode(t, err, "ValidationException")
	}

	out, err := s.RegisterScalableTarget(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	contexts := []context.Context{testContext("999999999999", "us-east-1"), testContext("123456789012", "us-west-2")}
	for _, other := range contexts {
		listed, e := s.DescribeScalableTargets(
			other,
			&api.DescribeScalableTargetsRequest{ServiceNamespace: in.ServiceNamespace},
		)
		if e != nil || len(listed.ScalableTargets) != 0 {
			t.Fatal(listed, e)
		}

		_, e = s.PutScalingPolicy(other, policyInput(*in.ResourceId, "cpu"))
		requireCode(t, e, "ObjectNotFoundException")
		_, e = s.ListTagsForResource(other, &api.ListTagsForResourceRequest{ResourceARN: out.ScalableTargetARN})
		requireCode(t, e, "ResourceNotFoundException")
		if _, e = s.RegisterScalableTarget(other, in); e != nil {
			t.Fatal(e)
		}
	}

	_, err = s.TagResource(
		ctx,
		&api.TagResourceRequest{ResourceARN: out.ScalableTargetARN, Tags: map[string]string{"aws:reserved": "bad"}},
	)
	requireCode(t, err, "ValidationException")
	p := policyInput(*in.ResourceId, "bad")
	p.TargetTrackingScalingPolicyConfiguration.TargetValue = new(-1.0)
	_, err = s.PutScalingPolicy(ctx, p)
	requireCode(t, err, "ValidationException")
	a := actionInput(*in.ResourceId, "bad")
	a.ScalableTargetAction = &api.ScalableTargetAction{MinCapacity: new(int64(4)), MaxCapacity: new(int64(2))}
	_, err = s.PutScheduledAction(ctx, a)
	requireCode(t, err, "ValidationException")
	// Same resource with a second dimension must have a distinct target identity.
	d := targetInput("table/example")
	d.ServiceNamespace = new("dynamodb")
	d.ScalableDimension = new("dynamodb:table:ReadCapacityUnits")
	first, err := s.RegisterScalableTarget(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	d.ScalableDimension = new("dynamodb:table:WriteCapacityUnits")
	second, err := s.RegisterScalableTarget(ctx, d)
	if err != nil || *first.ScalableTargetARN == *second.ScalableTargetARN {
		t.Fatal(second, err)
	}
}

func TestDescribePaginationAndFilters(t *testing.T) {
	s := New(memory.New(nil, nil))
	ctx := testContext("123456789012", "us-east-1")
	for _, resource := range []string{"service/c/a", "service/c/b", "service/c/c"} {
		if _, err := s.RegisterScalableTarget(ctx, targetInput(resource)); err != nil {
			t.Fatal(err)
		}

		if _, err := s.PutScalingPolicy(ctx, policyInput(resource, "cpu")); err != nil {
			t.Fatal(err)
		}

		if _, err := s.PutScheduledAction(ctx, actionInput(resource, "day")); err != nil {
			t.Fatal(err)
		}
	}

	namespace := new("ecs")
	limit := new(int64(1))
	in := &api.DescribeScalableTargetsRequest{ServiceNamespace: namespace, MaxResults: limit}
	seen := map[string]bool{}
	for {
		out, err := s.DescribeScalableTargets(ctx, in)
		if err != nil || len(out.ScalableTargets) != 1 {
			t.Fatal(out, err)
		}

		id := *out.ScalableTargets[0].ResourceId
		if seen[id] {
			t.Fatal("Repeated target")
		}

		seen[id] = true
		if out.NextToken == nil {
			break
		}

		in.NextToken = out.NextToken
	}

	if len(seen) != 3 {
		t.Fatal(seen)
	}

	pi := &api.DescribeScalingPoliciesRequest{
		ServiceNamespace: namespace,
		PolicyNames:      []string{"cpu"},
		MaxResults:       limit,
	}

	po, err := s.DescribeScalingPolicies(ctx, pi)
	if err != nil || len(po.ScalingPolicies) != 1 || po.NextToken == nil {
		t.Fatal(po, err)
	}

	pi.NextToken = po.NextToken
	pn, err := s.DescribeScalingPolicies(ctx, pi)
	if err != nil || len(pn.ScalingPolicies) != 1 ||
		*pn.ScalingPolicies[0].PolicyARN == *po.ScalingPolicies[0].PolicyARN {
		t.Fatal(pn, err)
	}

	_, err = s.DescribeScalingPolicies(testContext("123456789012", "us-west-2"), pi)
	requireCode(t, err, "InvalidNextTokenException")
	pi.PolicyNames = []string{"other"}
	_, err = s.DescribeScalingPolicies(ctx, pi)
	requireCode(t, err, "InvalidNextTokenException")
	ai := &api.DescribeScheduledActionsRequest{
		ServiceNamespace:     namespace,
		ScheduledActionNames: []string{"day"},
		MaxResults:           limit,
	}

	ao, err := s.DescribeScheduledActions(ctx, ai)
	if err != nil || len(ao.ScheduledActions) != 1 || ao.NextToken == nil {
		t.Fatal(ao, err)
	}

	ai.NextToken = ao.NextToken
	an, err := s.DescribeScheduledActions(ctx, ai)
	if err != nil || *an.ScheduledActions[0].ScheduledActionARN == *ao.ScheduledActions[0].ScheduledActionARN {
		t.Fatal(an, err)
	}

	filtered, err := s.DescribeScalableTargets(
		ctx,
		&api.DescribeScalableTargetsRequest{ServiceNamespace: namespace, ResourceIds: []string{"service/c/b"}},
	)
	if err != nil || len(filtered.ScalableTargets) != 1 || *filtered.ScalableTargets[0].ResourceId != "service/c/b" {
		t.Fatal(filtered, err)
	}

	_, err = s.DescribeScalableTargets(
		ctx,
		&api.DescribeScalableTargetsRequest{ServiceNamespace: namespace, NextToken: new("invalid")},
	)
	requireCode(t, err, "InvalidNextTokenException")
	_, err = s.DescribeScalableTargets(
		ctx,
		&api.DescribeScalableTargetsRequest{ServiceNamespace: namespace, MaxResults: new(int64(51))},
	)
	requireCode(t, err, "ValidationException")
}

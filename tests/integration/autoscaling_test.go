package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

func autoscalingClient(s *httptest.Server) *autoscaling.Client {
	return autoscaling.New(autoscaling.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}

	return ""
}

func describeGroup(t *testing.T, c *autoscaling.Client, name string) astypes.AutoScalingGroup {
	t.Helper()
	out, err := c.DescribeAutoScalingGroups(context.Background(), &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.AutoScalingGroups) != 1 {
		t.Fatalf("groups: %+v", out.AutoScalingGroups)
	}

	return out.AutoScalingGroups[0]
}

func groupInstanceIDs(g *astypes.AutoScalingGroup) []string {
	var ids []string
	for _, i := range g.Instances {
		ids = append(ids, aws.ToString(i.InstanceId))
	}

	slices.Sort(ids)
	return ids
}

// instanceStates returns the EC2 state of each instance.
func instanceStates(t *testing.T, c *ec2.Client, ids []string) map[string]ec2types.InstanceStateName {
	t.Helper()
	out, err := c.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		t.Fatal(err)
	}

	states := map[string]ec2types.InstanceStateName{}
	for _, r := range out.Reservations {
		for i := range r.Instances {
			states[aws.ToString(r.Instances[i].InstanceId)] = r.Instances[i].State.Name
		}
	}

	return states
}

func TestAutoScalingGroups(t *testing.T) {
	s := server(t)
	c := autoscalingClient(s)
	vm := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	_, err := vm.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{
			ImageId: latestImage(t, s), InstanceType: ec2types.InstanceTypeT3Micro,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	subnets, err := vm.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{ec2Filter("default-for-az", "true")},
	})
	if err != nil {
		t.Fatal(err)
	}

	zoneIdentifier := aws.ToString(subnets.Subnets[0].SubnetId) + "," + aws.ToString(subnets.Subnets[1].SubnetId)
	_, err = c.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String("web"), MinSize: aws.Int32(1), MaxSize: aws.Int32(4),
		DesiredCapacity: aws.Int32(2), VPCZoneIdentifier: &zoneIdentifier,
		LaunchTemplate: &astypes.LaunchTemplateSpecification{LaunchTemplateName: aws.String("web")},
		Tags: []astypes.Tag{
			{Key: aws.String("team"), Value: aws.String("core"), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String("cost"), Value: aws.String("7"), PropagateAtLaunch: aws.Bool(false)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	g := describeGroup(t, c, "web")
	zones := map[string]bool{}
	for _, i := range g.Instances {
		zones[aws.ToString(i.AvailabilityZone)] = true
		if i.LifecycleState != astypes.LifecycleStateInService || aws.ToString(i.HealthStatus) != "Healthy" ||
			!strings.HasPrefix(aws.ToString(i.LaunchTemplate.LaunchTemplateId), "lt-") {
			t.Fatalf("instance: %+v", i)
		}
	}

	if len(g.Instances) != 2 || len(zones) != 2 || len(g.AvailabilityZones) != 2 ||
		!strings.HasPrefix(aws.ToString(g.AutoScalingGroupARN), "arn:aws:autoscaling:us-east-1:000000000000:"+
			"autoScalingGroup:") || aws.ToString(g.LaunchTemplate.Version) != "$Default" {
		t.Fatalf("group: %+v", g)
	}

	// Only tags that propagate reach the instances.
	described, err := vm.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: groupInstanceIDs(&g)})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range described.Reservations {
		for _, i := range r.Instances {
			if len(i.Tags) != 1 || aws.ToString(i.Tags[0].Key) != "team" {
				t.Fatalf("instance tags: %+v", i.Tags)
			}
		}
	}

	if _, err = c.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String("web"), DesiredCapacity: aws.Int32(3),
	}); err != nil {
		t.Fatal(err)
	}

	if n := len(describeGroup(t, c, "web").Instances); n != 3 {
		t.Fatalf("after scaling out: %d instances", n)
	}

	// A lower maximum pulls the desired capacity down with it.
	before := groupInstanceIDs(new(describeGroup(t, c, "web")))
	if _, err = c.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String("web"), MaxSize: aws.Int32(2),
	}); err != nil {
		t.Fatal(err)
	}

	g = describeGroup(t, c, "web")
	if aws.ToInt32(g.DesiredCapacity) != 2 || len(g.Instances) != 2 {
		t.Fatalf("after lowering the maximum: %+v", g)
	}

	states := instanceStates(t, vm, before)
	if terminated := len(slices.DeleteFunc(slices.Clone(before), func(id string) bool {
		return states[id] != ec2types.InstanceStateNameTerminated
	})); terminated != 1 {
		t.Fatalf("states after scaling in: %v", states)
	}

	// An instance EC2 no longer runs is replaced.
	victim := groupInstanceIDs(&g)[0]
	if _, err = vm.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{victim}}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the replacement of "+victim, func() bool {
		ids := groupInstanceIDs(new(describeGroup(t, c, "web")))
		return len(ids) == 2 && !slices.Contains(ids, victim)
	})

	id := groupInstanceIDs(new(describeGroup(t, c, "web")))[0]
	terminated, err := c.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId: &id, ShouldDecrementDesiredCapacity: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	g = describeGroup(t, c, "web")
	if aws.ToInt32(g.DesiredCapacity) != 1 || len(g.Instances) != 1 ||
		!strings.Contains(aws.ToString(terminated.Activity.Description), id) {
		t.Fatalf("after terminating %s: %+v %+v", id, g, terminated.Activity)
	}

	_, err = c.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId: g.Instances[0].InstanceId, ShouldDecrementDesiredCapacity: aws.Bool(true),
	})
	if code := errorCode(err); code != "ValidationError" {
		t.Fatalf("terminating below the minimum: %v", err)
	}

	instances, err := c.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(instances.AutoScalingInstances) != 1 ||
		aws.ToString(instances.AutoScalingInstances[0].AutoScalingGroupName) != "web" ||
		aws.ToString(instances.AutoScalingInstances[0].HealthStatus) != "HEALTHY" {
		t.Fatalf("instances: %+v", instances.AutoScalingInstances)
	}

	activities, err := c.DescribeScalingActivities(ctx, &autoscaling.DescribeScalingActivitiesInput{
		AutoScalingGroupName: aws.String("web"),
	})
	if err != nil {
		t.Fatal(err)
	}

	first := activities.Activities[0]
	if len(activities.Activities) < 7 || !strings.HasPrefix(aws.ToString(first.Description), "Terminating") ||
		first.StatusCode != astypes.ScalingActivityStatusCodeSuccessful {
		t.Fatalf("%d activities, newest %+v", len(activities.Activities), first)
	}

	// Suspending Launch keeps the group from replacing what it loses.
	if _, err = c.SuspendProcesses(ctx, &autoscaling.SuspendProcessesInput{
		AutoScalingGroupName: aws.String("web"), ScalingProcesses: []string{"Launch"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String("web"), DesiredCapacity: aws.Int32(2),
	}); err != nil {
		t.Fatal(err)
	}

	if g = describeGroup(t, c, "web"); len(g.Instances) != 1 || len(g.SuspendedProcesses) != 1 {
		t.Fatalf("suspended: %+v", g)
	}

	if _, err = c.ResumeProcesses(ctx, &autoscaling.ResumeProcessesInput{
		AutoScalingGroupName: aws.String("web"),
	}); err != nil {
		t.Fatal(err)
	}

	if g = describeGroup(t, c, "web"); len(g.Instances) != 2 || len(g.SuspendedProcesses) != 0 {
		t.Fatalf("resumed: %+v", g)
	}

	_, err = c.DeleteAutoScalingGroup(ctx, &autoscaling.DeleteAutoScalingGroupInput{
		AutoScalingGroupName: aws.String("web"),
	})
	if code := errorCode(err); code != "ResourceInUse" {
		t.Fatalf("deleting a group with instances: %v", err)
	}

	last := groupInstanceIDs(&g)
	if _, err = c.DeleteAutoScalingGroup(ctx, &autoscaling.DeleteAutoScalingGroupInput{
		AutoScalingGroupName: aws.String("web"), ForceDelete: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	for id, state := range instanceStates(t, vm, last) {
		if state != ec2types.InstanceStateNameTerminated {
			t.Fatalf("%s is %s after the group was deleted", id, state)
		}
	}

	groups, err := c.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(groups.AutoScalingGroups) != 0 {
		t.Fatalf("groups after deletion: %+v", groups.AutoScalingGroups)
	}
}

func TestAutoScalingPoliciesAndSchedules(t *testing.T) {
	s := server(t)
	c := autoscalingClient(s)
	ctx := context.Background()
	_, err := c.CreateLaunchConfiguration(ctx, &autoscaling.CreateLaunchConfigurationInput{
		LaunchConfigurationName: aws.String("small"), ImageId: latestImage(t, s), InstanceType: aws.String("t3.micro"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String("jobs"), MinSize: aws.Int32(0), MaxSize: aws.Int32(6),
		LaunchConfigurationName: aws.String("small"), AvailabilityZones: []string{"us-east-1a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if g := describeGroup(t, c, "jobs"); aws.ToInt32(g.DesiredCapacity) != 0 || len(g.Instances) != 0 {
		t.Fatalf("new group: %+v", g)
	}

	_, err = c.PutScalingPolicy(ctx, &autoscaling.PutScalingPolicyInput{
		AutoScalingGroupName: aws.String("jobs"), PolicyName: aws.String("up"),
		AdjustmentType: aws.String("ChangeInCapacity"), ScalingAdjustment: aws.Int32(2), Cooldown: aws.Int32(600),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.ExecutePolicy(ctx, &autoscaling.ExecutePolicyInput{
		AutoScalingGroupName: aws.String("jobs"), PolicyName: aws.String("up"),
	}); err != nil {
		t.Fatal(err)
	}

	if g := describeGroup(t, c, "jobs"); aws.ToInt32(g.DesiredCapacity) != 2 || len(g.Instances) != 2 ||
		aws.ToString(g.Instances[0].LaunchConfigurationName) != "small" {
		t.Fatalf("after executing up: %+v", g)
	}

	_, err = c.ExecutePolicy(ctx, &autoscaling.ExecutePolicyInput{
		AutoScalingGroupName: aws.String("jobs"), PolicyName: aws.String("up"), HonorCooldown: aws.Bool(true),
	})
	if code := errorCode(err); code != "ScalingActivityInProgress" {
		t.Fatalf("executing during the cooldown: %v", err)
	}

	_, err = c.PutScalingPolicy(ctx, &autoscaling.PutScalingPolicyInput{
		AutoScalingGroupName: aws.String(
			"jobs",
		),
		PolicyName:     aws.String("steps"),
		PolicyType:     aws.String("StepScaling"),
		AdjustmentType: aws.String("ExactCapacity"),
		StepAdjustments: []astypes.StepAdjustment{
			{
				MetricIntervalLowerBound: aws.Float64(0), MetricIntervalUpperBound: aws.Float64(10),
				ScalingAdjustment: aws.Int32(3),
			},
			{MetricIntervalLowerBound: aws.Float64(10), ScalingAdjustment: aws.Int32(5)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.ExecutePolicy(ctx, &autoscaling.ExecutePolicyInput{
		AutoScalingGroupName: aws.String("jobs"), PolicyName: aws.String("steps"),
		MetricValue: aws.Float64(95), BreachThreshold: aws.Float64(80),
	}); err != nil {
		t.Fatal(err)
	}

	if g := describeGroup(t, c, "jobs"); aws.ToInt32(g.DesiredCapacity) != 5 || len(g.Instances) != 5 {
		t.Fatalf("after executing steps: %+v", g)
	}

	_, err = c.ExecutePolicy(ctx, &autoscaling.ExecutePolicyInput{
		AutoScalingGroupName: aws.String("jobs"), PolicyName: aws.String("steps"),
	})
	if code := errorCode(err); code != "ValidationError" {
		t.Fatalf("executing steps without a metric: %v", err)
	}

	// A target tracking policy owns two CloudWatch alarms that execute it.
	tracked, err := c.PutScalingPolicy(ctx, &autoscaling.PutScalingPolicyInput{
		AutoScalingGroupName: aws.String("jobs"), PolicyName: aws.String("cpu"),
		PolicyType: aws.String("TargetTrackingScaling"),
		TargetTrackingConfiguration: &astypes.TargetTrackingConfiguration{
			TargetValue: aws.Float64(50),
			PredefinedMetricSpecification: &astypes.PredefinedMetricSpecification{
				PredefinedMetricType: astypes.MetricTypeASGAverageCPUUtilization,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cw := cloudwatchClient(s)
	alarms, err := cw.DescribeAlarms(
		ctx,
		&cloudwatch.DescribeAlarmsInput{AlarmNamePrefix: aws.String("TargetTracking-jobs-")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(tracked.Alarms) != 2 || len(alarms.MetricAlarms) != 2 ||
		aws.ToString(alarms.MetricAlarms[0].MetricName) != "CPUUtilization" ||
		alarms.MetricAlarms[0].AlarmActions[0] != aws.ToString(tracked.PolicyARN) {
		t.Fatalf("target tracking alarms: %+v %+v", tracked.Alarms, alarms.MetricAlarms)
	}

	policies, err := c.DescribePolicies(
		ctx,
		&autoscaling.DescribePoliciesInput{AutoScalingGroupName: aws.String("jobs")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(policies.ScalingPolicies) != 3 {
		t.Fatalf("policies: %+v", policies.ScalingPolicies)
	}

	if _, err = c.DeletePolicy(ctx, &autoscaling.DeletePolicyInput{PolicyName: tracked.PolicyARN}); err != nil {
		t.Fatal(err)
	}

	alarms, err = cw.DescribeAlarms(
		ctx,
		&cloudwatch.DescribeAlarmsInput{AlarmNamePrefix: aws.String("TargetTracking-")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(alarms.MetricAlarms) != 0 {
		t.Fatalf("alarms after deleting the policy: %+v", alarms.MetricAlarms)
	}

	// A scheduled action changes the group's size when it is due.
	_, err = c.PutScheduledUpdateGroupAction(ctx, &autoscaling.PutScheduledUpdateGroupActionInput{
		AutoScalingGroupName: aws.String("jobs"), ScheduledActionName: aws.String("shrink"),
		StartTime: aws.Time(time.Now().Add(time.Second)), MinSize: aws.Int32(1), DesiredCapacity: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutScheduledUpdateGroupAction(ctx, &autoscaling.PutScheduledUpdateGroupActionInput{
		AutoScalingGroupName: aws.String("jobs"), ScheduledActionName: aws.String("weekdays"),
		Recurrence: aws.String("0 9 * * 1-5"), TimeZone: aws.String("America/New_York"), DesiredCapacity: aws.Int32(4),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the scheduled action", func() bool {
		g := describeGroup(t, c, "jobs")
		return aws.ToInt32(g.DesiredCapacity) == 1 && aws.ToInt32(g.MinSize) == 1 && len(g.Instances) == 1
	})

	actions, err := c.DescribeScheduledActions(ctx, &autoscaling.DescribeScheduledActionsInput{
		AutoScalingGroupName: aws.String("jobs"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(actions.ScheduledUpdateGroupActions) != 1 {
		t.Fatalf("scheduled actions: %+v", actions.ScheduledUpdateGroupActions)
	}

	next := aws.ToTime(actions.ScheduledUpdateGroupActions[0].StartTime).In(mustLocation(t, "America/New_York"))
	if next.Hour() != 9 || next.Minute() != 0 || next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		t.Fatalf("next weekday run: %s", next)
	}

	_, err = c.PutScheduledUpdateGroupAction(ctx, &autoscaling.PutScheduledUpdateGroupActionInput{
		AutoScalingGroupName: aws.String("jobs"), ScheduledActionName: aws.String("bad"),
		Recurrence: aws.String("0 9 1 * 1"), DesiredCapacity: aws.Int32(1),
	})
	if code := errorCode(err); code != "ValidationError" {
		t.Fatalf("both day fields: %v", err)
	}

	_, err = c.DeleteLaunchConfiguration(ctx, &autoscaling.DeleteLaunchConfigurationInput{
		LaunchConfigurationName: aws.String("small"),
	})
	if code := errorCode(err); code != "ResourceInUse" {
		t.Fatalf("deleting a launch configuration in use: %v", err)
	}
}

func TestAutoScalingTagsAndValidation(t *testing.T) {
	s := server(t)
	c := autoscalingClient(s)
	vm := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	_, err := vm.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{ImageId: latestImage(t, s)},
	})
	if err != nil {
		t.Fatal(err)
	}

	template := &astypes.LaunchTemplateSpecification{LaunchTemplateName: aws.String("web")}
	for name, in := range map[string]*autoscaling.CreateAutoScalingGroupInput{
		"desired above max": {
			MinSize: aws.Int32(0), MaxSize: aws.Int32(1), DesiredCapacity: aws.Int32(2),
			AvailabilityZones: []string{"us-east-1a"}, LaunchTemplate: template,
		},
		"no zones": {MinSize: aws.Int32(0), MaxSize: aws.Int32(1), LaunchTemplate: template},
		"no template": {
			MinSize: aws.Int32(0), MaxSize: aws.Int32(1), AvailabilityZones: []string{"us-east-1a"},
			LaunchTemplate: &astypes.LaunchTemplateSpecification{LaunchTemplateName: aws.String("missing")},
		},
		"no subnet": {
			MinSize: aws.Int32(0), MaxSize: aws.Int32(1), VPCZoneIdentifier: aws.String("subnet-0"),
			LaunchTemplate: template,
		},
	} {
		in.AutoScalingGroupName = aws.String("bad")
		if _, err = c.CreateAutoScalingGroup(ctx, in); errorCode(err) != "ValidationError" {
			t.Fatalf("%s: %v", name, err)
		}
	}

	for _, name := range []string{"a", "b"} {
		_, err = c.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
			AutoScalingGroupName: aws.String(name), MinSize: aws.Int32(0), MaxSize: aws.Int32(1),
			AvailabilityZones: []string{"us-east-1a"}, LaunchTemplate: template,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String("a"), MinSize: aws.Int32(0), MaxSize: aws.Int32(1),
		AvailabilityZones: []string{"us-east-1a"}, LaunchTemplate: template,
	})
	if code := errorCode(err); code != "AlreadyExists" {
		t.Fatalf("duplicate group: %v", err)
	}

	tag := func(group, k, v string) astypes.Tag {
		return astypes.Tag{
			ResourceId: aws.String(group), ResourceType: aws.String("auto-scaling-group"), Key: aws.String(k),
			Value: aws.String(v), PropagateAtLaunch: aws.Bool(true),
		}
	}
	if _, err = c.CreateOrUpdateTags(ctx, &autoscaling.CreateOrUpdateTagsInput{
		Tags: []astypes.Tag{tag("a", "env", "dev"), tag("b", "env", "prod"), tag("b", "team", "core")},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.DescribeTags(ctx, &autoscaling.DescribeTagsInput{Filters: []astypes.Filter{
		{Name: aws.String("key"), Values: []string{"env"}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if len(tags.Tags) != 2 || aws.ToString(tags.Tags[1].Value) != "prod" {
		t.Fatalf("tags: %+v", tags.Tags)
	}

	groups, err := c.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		Filters: []astypes.Filter{{Name: aws.String("tag:env"), Values: []string{"prod"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(groups.AutoScalingGroups) != 1 || aws.ToString(groups.AutoScalingGroups[0].AutoScalingGroupName) != "b" {
		t.Fatalf("groups by tag: %+v", groups.AutoScalingGroups)
	}

	if _, err = c.DeleteTags(ctx, &autoscaling.DeleteTagsInput{Tags: []astypes.Tag{tag("b", "env", "")}}); err != nil {
		t.Fatal(err)
	}

	if g := describeGroup(t, c, "b"); len(g.Tags) != 1 || aws.ToString(g.Tags[0].Key) != "team" {
		t.Fatalf("tags after deleting: %+v", g.Tags)
	}

	page, err := c.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{MaxRecords: aws.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}

	if len(page.AutoScalingGroups) != 1 || page.NextToken == nil {
		t.Fatalf("first page: %+v", page)
	}

	page, err = c.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		MaxRecords: aws.Int32(1), NextToken: page.NextToken,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(page.AutoScalingGroups) != 1 || aws.ToString(page.AutoScalingGroups[0].AutoScalingGroupName) != "b" ||
		page.NextToken != nil {
		t.Fatalf("second page: %+v", page)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}

	return loc
}

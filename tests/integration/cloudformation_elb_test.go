package integration_test

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
)

func classicTemplate(subnets []string, properties map[string]any) string {
	base := map[string]any{"LoadBalancerName": "cfn-classic", "Subnets": subnets}
	for k, v := range properties {
		base[k] = v
	}

	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{"Balancer": map[string]any{
			"Type": "AWS::ElasticLoadBalancing::LoadBalancer", "Properties": base,
		}},
		"Outputs": map[string]any{
			"Name": map[string]any{"Value": map[string]any{"Ref": "Balancer"}},
			"DNS":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Balancer", "DNSName"}}},
			"Zone": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Balancer", "CanonicalHostedZoneNameID"}},
			},
			"Owner": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Balancer", "SourceSecurityGroup.OwnerAlias"}},
			},
			"Group": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Balancer", "SourceSecurityGroup.GroupName"}},
			},
		},
	})
	return string(raw)
}

func TestCloudFormationClassicLoadBalancerLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf, c := cfnClient(s), classicClient(s)
	_, subnets := elbSubnets(t, s)
	launched, err := ec2Client(s, "test", "us-east-1").RunInstances(ctx, &ec2sdk.RunInstancesInput{
		ImageId: latestImage(
			t,
			s,
		),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(2),
		MaxCount:     aws.Int32(2),
	})
	if err != nil {
		t.Fatal(err)
	}

	first, second := *launched.Instances[0].InstanceId, *launched.Instances[1].InstanceId
	http80 := map[string]any{"Protocol": "HTTP", "LoadBalancerPort": "80", "InstancePort": "8080"}
	tcp90 := map[string]any{"Protocol": "TCP", "LoadBalancerPort": "90", "InstancePort": "9090"}
	_, err = cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("classic"), TemplateBody: new(classicTemplate(subnets, map[string]any{
			"Listeners": []any{http80, tcp90},
			"HealthCheck": map[string]any{
				"Target":             "TCP:8080",
				"Interval":           "20",
				"Timeout":            "5",
				"UnhealthyThreshold": "3",
				"HealthyThreshold":   "3",
			},
			"Instances":                []any{first},
			"CrossZone":                true,
			"ConnectionDrainingPolicy": map[string]any{"Enabled": true, "Timeout": 60},
			"Tags": []any{
				map[string]any{"Key": "version", "Value": "one"},
				map[string]any{"Key": "keep", "Value": "yes"},
			},
		}))},
	)
	if err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, cf, "classic", cfntypes.StackStatusCreateComplete)
	out := outputsOf(&stack)
	if out["Name"] != "cfn-classic" ||
		!regexp.MustCompile(`^cfn-classic-\d+\.us-east-1\.elb\.amazonaws\.com$`).MatchString(out["DNS"]) ||
		out["Zone"] != "Z35SXDOTRQ7X7K" ||
		out["Owner"] != "amazon-elb" ||
		out["Group"] != "amazon-elb-sg" {
		t.Fatal(out)
	}

	described, err := c.DescribeLoadBalancers(
		ctx,
		&sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"cfn-classic"}},
	)
	if err != nil || len(described.LoadBalancerDescriptions[0].ListenerDescriptions) != 2 ||
		len(described.LoadBalancerDescriptions[0].Instances) != 1 ||
		*described.LoadBalancerDescriptions[0].HealthCheck.Interval != 20 {
		t.Fatal(described, err)
	}

	attributes, err := c.DescribeLoadBalancerAttributes(
		ctx,
		&sdk.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("cfn-classic")},
	)
	if err != nil || !attributes.LoadBalancerAttributes.CrossZoneLoadBalancing.Enabled ||
		*attributes.LoadBalancerAttributes.ConnectionDraining.Timeout != 60 {
		t.Fatal(attributes, err)
	}

	// An update reconciles listeners, the health check, instances, attributes and tags in place.
	http80["InstancePort"] = "8081"
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("classic"), TemplateBody: new(classicTemplate(subnets, map[string]any{
			"Listeners": []any{http80},
			"HealthCheck": map[string]any{
				"Target":             "HTTP:8081/ping",
				"Interval":           "30",
				"Timeout":            "5",
				"UnhealthyThreshold": "2",
				"HealthyThreshold":   "2",
			},
			"Instances": []any{second},
			"Tags": []any{
				map[string]any{"Key": "version", "Value": "two"},
				map[string]any{"Key": "keep", "Value": "yes"},
			},
		}))},
	)
	if err != nil {
		t.Fatal(err)
	}

	updated := wantStatus(ctx, t, cf, "classic", cfntypes.StackStatusUpdateComplete)
	if next := outputsOf(&updated); next["DNS"] != out["DNS"] || next["Name"] != out["Name"] {
		t.Fatal("an in-place update replaced the load balancer:", next)
	}

	described, err = c.DescribeLoadBalancers(
		ctx,
		&sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"cfn-classic"}},
	)
	lb := described.LoadBalancerDescriptions[0]
	if err != nil || len(lb.ListenerDescriptions) != 1 || *lb.ListenerDescriptions[0].Listener.InstancePort != 8081 ||
		len(lb.Instances) != 1 || *lb.Instances[0].InstanceId != second || *lb.HealthCheck.Target != "HTTP:8081/ping" {
		t.Fatalf("%+v, %v", lb, err)
	}

	attributes, err = c.DescribeLoadBalancerAttributes(
		ctx,
		&sdk.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("cfn-classic")},
	)
	if err != nil || attributes.LoadBalancerAttributes.CrossZoneLoadBalancing.Enabled ||
		attributes.LoadBalancerAttributes.ConnectionDraining.Enabled {
		t.Fatal("removed attribute properties were not reset:", attributes, err)
	}

	tags, err := c.DescribeTags(ctx, &sdk.DescribeTagsInput{LoadBalancerNames: []string{"cfn-classic"}})
	if err != nil || len(tags.TagDescriptions[0].Tags) != 2 || *tags.TagDescriptions[0].Tags[1].Value != "two" {
		t.Fatal(tags, err)
	}

	// Properties that need policies, and removing the health check, fail explicitly and roll back.
	for name, properties := range map[string]map[string]any{
		"policies": {"Listeners": []any{http80}, "LBCookieStickinessPolicy": []any{map[string]any{"PolicyName": "sticky"}}},
		"names": {"Listeners": []any{
			map[string]any{"Protocol": "HTTP", "LoadBalancerPort": "80", "InstancePort": "8081", "PolicyNames": []any{"p"}},
		}},
		"health": {"Listeners": []any{http80}},
	} {
		if name != "health" {
			properties["HealthCheck"] = map[string]any{
				"Target":             "TCP:8081",
				"Interval":           "30",
				"Timeout":            "5",
				"UnhealthyThreshold": "2",
				"HealthyThreshold":   "2",
			}
		}

		_, err = cf.UpdateStack(
			ctx,
			&cfn.UpdateStackInput{StackName: new("classic"), TemplateBody: new(classicTemplate(subnets, properties))},
		)
		if err != nil {
			t.Fatal(name, err)
		}

		wantStatus(ctx, t, cf, "classic", cfntypes.StackStatusUpdateRollbackComplete)
	}

	described, err = c.DescribeLoadBalancers(
		ctx,
		&sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"cfn-classic"}},
	)
	if err != nil || *described.LoadBalancerDescriptions[0].HealthCheck.Target != "HTTP:8081/ping" {
		t.Fatal("a rolled back update changed the load balancer:", described, err)
	}

	// Changing Subnets replaces the load balancer.
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("classic"), TemplateBody: new(classicTemplate(subnets[:1], map[string]any{
			"Listeners": []any{http80},
			"HealthCheck": map[string]any{
				"Target":             "HTTP:8081/ping",
				"Interval":           "30",
				"Timeout":            "5",
				"UnhealthyThreshold": "2",
				"HealthyThreshold":   "2",
			},
		}))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "classic", cfntypes.StackStatusUpdateComplete)
	described, err = c.DescribeLoadBalancers(
		ctx,
		&sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"cfn-classic"}},
	)
	if err != nil || len(described.LoadBalancerDescriptions[0].Subnets) != 1 {
		t.Fatal(described, err)
	}

	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("classic")}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)
	_, err = c.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"cfn-classic"}})
	code(t, err, "LoadBalancerNotFound")
}

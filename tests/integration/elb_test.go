package integration_test

import (
	"context"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	elbv2sdk "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func classicClient(s *httptest.Server) *sdk.Client {
	return sdk.New(sdk.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{},
	})
}

func TestClassicLoadBalancerLifecycle(t *testing.T) {
	s := server(t)
	c := classicClient(s)
	ctx := context.Background()
	vpc, subnets := elbSubnets(t, s)
	listeners := []types.Listener{
		{Protocol: new("HTTP"), LoadBalancerPort: 80, InstanceProtocol: new("HTTP"), InstancePort: new(int32(8080))},
		{
			Protocol: new("HTTPS"), LoadBalancerPort: 443, InstancePort: new(int32(8443)),
			SSLCertificateId: new("arn:aws:iam::123456789012:server-certificate/web"),
		},
	}
	created, err := c.CreateLoadBalancer(ctx, &sdk.CreateLoadBalancerInput{
		LoadBalancerName: new("web"), Listeners: listeners, Subnets: subnets,
		Tags: []types.Tag{{Key: new("env"), Value: new("test")}},
	})
	if err != nil || !regexp.MustCompile(`^web-\d+\.us-east-1\.elb\.amazonaws\.com$`).MatchString(*created.DNSName) {
		t.Fatal(created, err)
	}

	for name, in := range map[string]*sdk.CreateLoadBalancerInput{
		"DuplicateLoadBalancerName": {LoadBalancerName: new("web"), Listeners: listeners[:1], Subnets: subnets},
		"InvalidScheme": {
			LoadBalancerName: new("x"), Listeners: listeners[:1], Subnets: subnets, Scheme: new("public"),
		},
		"SubnetNotFound": {
			LoadBalancerName: new("x"), Listeners: listeners[:1], Subnets: []string{"subnet-00000000"},
		},
		"InvalidConfigurationRequest": {
			LoadBalancerName: new("x"), Listeners: listeners[:1], Subnets: subnets,
			AvailabilityZones: []string{"us-east-1a"},
		},
		"CertificateNotFound": {
			LoadBalancerName: new("x"), Subnets: subnets,
			Listeners: []types.Listener{{Protocol: new("HTTPS"), LoadBalancerPort: 443, InstancePort: new(int32(80))}},
		},
		"UnsupportedProtocol": {
			LoadBalancerName: new("x"), Subnets: subnets,
			Listeners: []types.Listener{{Protocol: new("UDP"), LoadBalancerPort: 53, InstancePort: new(int32(53))}},
		},
		"ValidationException": {LoadBalancerName: new("-bad-"), Listeners: listeners[:1], Subnets: subnets},
	} {
		_, err = c.CreateLoadBalancer(ctx, in)
		code(t, err, name)
	}

	described, err := c.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"web"}})
	if err != nil || len(described.LoadBalancerDescriptions) != 1 {
		t.Fatal(described, err)
	}

	lb := described.LoadBalancerDescriptions[0]
	if *lb.VPCId != vpc || len(lb.Subnets) != 2 || len(lb.SecurityGroups) != 1 || len(lb.ListenerDescriptions) != 2 ||
		*lb.Scheme != "internet-facing" || *lb.HealthCheck.Target != "TCP:8080" || *lb.HealthCheck.Interval != 30 ||
		*lb.DNSName != *created.DNSName || *lb.CanonicalHostedZoneNameID != "Z35SXDOTRQ7X7K" ||
		*lb.SourceSecurityGroup.OwnerAlias != "amazon-elb" || lb.CreatedTime == nil {
		t.Fatalf("%+v", lb)
	}

	_, err = c.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"missing"}})
	code(t, err, "LoadBalancerNotFound")

	_, err = c.CreateLoadBalancer(ctx, &sdk.CreateLoadBalancerInput{
		LoadBalancerName: new("zone"), Listeners: listeners[:1], AvailabilityZones: []string{"us-east-1a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{PageSize: new(int32(1))})
	if err != nil || len(page.LoadBalancerDescriptions) != 1 ||
		*page.LoadBalancerDescriptions[0].LoadBalancerName != "web" ||
		page.NextMarker == nil {
		t.Fatal(page, err)
	}

	page, err = c.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{Marker: page.NextMarker})
	if err != nil || len(page.LoadBalancerDescriptions) != 1 ||
		aws.ToString(page.LoadBalancerDescriptions[0].VPCId) != "" ||
		*page.LoadBalancerDescriptions[0].LoadBalancerName != "zone" ||
		page.NextMarker != nil {
		t.Fatal(page, err)
	}

	// Listeners: the same settings are accepted again, different ones on a used port are a duplicate.
	extra := types.Listener{Protocol: new("TCP"), LoadBalancerPort: 9000, InstancePort: new(int32(9001))}
	for range 2 {
		if _, err = c.CreateLoadBalancerListeners(ctx, &sdk.CreateLoadBalancerListenersInput{
			LoadBalancerName: new("web"), Listeners: []types.Listener{extra},
		}); err != nil {
			t.Fatal(err)
		}
	}

	extra.InstancePort = new(int32(9002))
	_, err = c.CreateLoadBalancerListeners(ctx, &sdk.CreateLoadBalancerListenersInput{
		LoadBalancerName: new("web"), Listeners: []types.Listener{extra},
	})
	code(t, err, "DuplicateListener")
	_, err = c.DeleteLoadBalancerListeners(ctx, &sdk.DeleteLoadBalancerListenersInput{
		LoadBalancerName: new("web"), LoadBalancerPorts: []int32{9000, 9999},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Health check.
	health := &types.HealthCheck{
		Target: new("HTTP:8080/ping"), Interval: new(int32(10)), Timeout: new(int32(5)),
		UnhealthyThreshold: new(int32(3)), HealthyThreshold: new(int32(4)),
	}
	checked, err := c.ConfigureHealthCheck(
		ctx,
		&sdk.ConfigureHealthCheckInput{LoadBalancerName: new("web"), HealthCheck: health},
	)
	if err != nil || *checked.HealthCheck.Target != "HTTP:8080/ping" {
		t.Fatal(checked, err)
	}

	health.Timeout = new(int32(10))
	_, err = c.ConfigureHealthCheck(
		ctx,
		&sdk.ConfigureHealthCheckInput{LoadBalancerName: new("web"), HealthCheck: health},
	)
	code(t, err, "ValidationException")

	// Instances.
	ec2 := ec2Client(s, "test", "us-east-1")
	launched, err := ec2.RunInstances(ctx, &ec2sdk.RunInstancesInput{
		ImageId: latestImage(
			t,
			s,
		),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	instance := []types.Instance{{InstanceId: launched.Instances[0].InstanceId}}
	registered, err := c.RegisterInstancesWithLoadBalancer(ctx, &sdk.RegisterInstancesWithLoadBalancerInput{
		LoadBalancerName: new("web"), Instances: instance,
	})
	if err != nil || len(registered.Instances) != 1 {
		t.Fatal(registered, err)
	}

	_, err = c.RegisterInstancesWithLoadBalancer(ctx, &sdk.RegisterInstancesWithLoadBalancerInput{
		LoadBalancerName: new("web"), Instances: []types.Instance{{InstanceId: new("i-0123456789abcdef0")}},
	})
	code(t, err, "InvalidInstance")

	states, err := c.DescribeInstanceHealth(ctx, &sdk.DescribeInstanceHealthInput{LoadBalancerName: new("web")})
	if err != nil || len(states.InstanceStates) != 1 || *states.InstanceStates[0].State != "Unknown" ||
		*states.InstanceStates[0].ReasonCode != "N/A" {
		t.Fatal(states, err)
	}

	left, err := c.DeregisterInstancesFromLoadBalancer(ctx, &sdk.DeregisterInstancesFromLoadBalancerInput{
		LoadBalancerName: new("web"), Instances: instance,
	})
	if err != nil || len(left.Instances) != 0 {
		t.Fatal(left, err)
	}

	states, err = c.DescribeInstanceHealth(ctx, &sdk.DescribeInstanceHealthInput{
		LoadBalancerName: new("web"), Instances: instance,
	})
	if err != nil || *states.InstanceStates[0].State != "OutOfService" ||
		*states.InstanceStates[0].ReasonCode != "Instance" {
		t.Fatal(states, err)
	}

	// Attributes: a rejected change leaves every attribute as it was.
	attributes, err := c.DescribeLoadBalancerAttributes(
		ctx,
		&sdk.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("web")},
	)
	if err != nil || attributes.LoadBalancerAttributes.CrossZoneLoadBalancing.Enabled ||
		*attributes.LoadBalancerAttributes.ConnectionSettings.IdleTimeout != 60 ||
		*attributes.LoadBalancerAttributes.ConnectionDraining.Timeout != 300 {
		t.Fatal(attributes, err)
	}

	_, err = c.ModifyLoadBalancerAttributes(ctx, &sdk.ModifyLoadBalancerAttributesInput{
		LoadBalancerName: new("web"), LoadBalancerAttributes: &types.LoadBalancerAttributes{
			CrossZoneLoadBalancing: &types.CrossZoneLoadBalancing{Enabled: true},
			AccessLog:              &types.AccessLog{Enabled: true},
		},
	})
	code(t, err, "InvalidConfigurationRequest")

	modified, err := c.ModifyLoadBalancerAttributes(ctx, &sdk.ModifyLoadBalancerAttributesInput{
		LoadBalancerName: new("web"), LoadBalancerAttributes: &types.LoadBalancerAttributes{
			CrossZoneLoadBalancing: &types.CrossZoneLoadBalancing{Enabled: true},
			ConnectionSettings:     &types.ConnectionSettings{IdleTimeout: new(int32(120))},
			ConnectionDraining:     &types.ConnectionDraining{Enabled: true, Timeout: new(int32(30))},
			AccessLog: &types.AccessLog{
				Enabled: true, S3BucketName: new("logs"), EmitInterval: new(int32(5)), S3BucketPrefix: new("elb"),
			},
		},
	})
	if err != nil || !modified.LoadBalancerAttributes.CrossZoneLoadBalancing.Enabled ||
		*modified.LoadBalancerAttributes.AccessLog.S3BucketName != "logs" {
		t.Fatal(modified, err)
	}

	attributes, err = c.DescribeLoadBalancerAttributes(
		ctx,
		&sdk.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("web")},
	)
	if err != nil || !attributes.LoadBalancerAttributes.CrossZoneLoadBalancing.Enabled ||
		*attributes.LoadBalancerAttributes.ConnectionSettings.IdleTimeout != 120 ||
		*attributes.LoadBalancerAttributes.ConnectionDraining.Timeout != 30 {
		t.Fatal(attributes, err)
	}

	// Tags, also through the Resource Groups Tagging API.
	_, err = c.AddTags(ctx, &sdk.AddTagsInput{
		LoadBalancerNames: []string{"web", "zone"}, Tags: []types.Tag{{Key: new("team"), Value: new("edge")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AddTags(ctx, &sdk.AddTagsInput{
		LoadBalancerNames: []string{
			"web",
		},
		Tags: []types.Tag{{Key: new("a"), Value: new("1")}, {Key: new("a"), Value: new("2")}},
	})
	code(t, err, "DuplicateTagKeys")

	tagged, err := c.DescribeTags(ctx, &sdk.DescribeTagsInput{LoadBalancerNames: []string{"web", "zone"}})
	if err != nil || len(tagged.TagDescriptions) != 2 || len(tagged.TagDescriptions[0].Tags) != 2 ||
		len(tagged.TagDescriptions[1].Tags) != 1 {
		t.Fatal(tagged, err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Credentials: creds("test")})
	resources, err := tagging.GetResources(
		ctx,
		&rgta.GetResourcesInput{ResourceTypeFilters: []string{"elasticloadbalancing:loadbalancer"}},
	)
	if err != nil || len(resources.ResourceTagMappingList) != 2 {
		t.Fatal(resources, err)
	}

	arn := *resources.ResourceTagMappingList[0].ResourceARN
	if result, tagErr := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{arn}, Tags: map[string]string{"rgta": "works"},
	}); tagErr != nil || len(result.FailedResourcesMap) != 0 {
		t.Fatal(result, tagErr)
	}

	if result, untagErr := tagging.UntagResources(ctx, &rgta.UntagResourcesInput{
		ResourceARNList: []string{arn}, TagKeys: []string{"env"},
	}); untagErr != nil || len(result.FailedResourcesMap) != 0 {
		t.Fatal(result, untagErr)
	}

	_, err = c.RemoveTags(
		ctx,
		&sdk.RemoveTagsInput{LoadBalancerNames: []string{"web"}, Tags: []types.TagKeyOnly{{Key: new("team")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tagged, err = c.DescribeTags(ctx, &sdk.DescribeTagsInput{LoadBalancerNames: []string{"web"}})
	if err != nil || len(tagged.TagDescriptions[0].Tags) != 1 || *tagged.TagDescriptions[0].Tags[0].Key != "rgta" {
		t.Fatal(tagged, err)
	}

	// Deleting is idempotent.
	for range 2 {
		if _, err = c.DeleteLoadBalancer(ctx, &sdk.DeleteLoadBalancerInput{LoadBalancerName: new("web")}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DescribeLoadBalancerAttributes(
		ctx,
		&sdk.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("web")},
	)
	code(t, err, "LoadBalancerNotFound")
}

// Classic and v2 Elastic Load Balancing share the signing name and several action names, so the API version of the
// request decides which service answers.
func TestClassicAndV2LoadBalancersSideBySide(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	_, subnets := elbSubnets(t, s)
	classic, v2 := classicClient(s), elbClient(s)
	if _, err := classic.CreateLoadBalancer(ctx, &sdk.CreateLoadBalancerInput{
		LoadBalancerName: new("old"), Subnets: subnets,
		Listeners: []types.Listener{{Protocol: new("TCP"), LoadBalancerPort: 80, InstancePort: new(int32(80))}},
	}); err != nil {
		t.Fatal(err)
	}

	modern, err := v2.CreateLoadBalancer(ctx, &elbv2sdk.CreateLoadBalancerInput{Name: new("new"), Subnets: subnets})
	if err != nil || len(modern.LoadBalancers) != 1 {
		t.Fatal(modern, err)
	}

	old, err := classic.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{})
	if err != nil || len(old.LoadBalancerDescriptions) != 1 ||
		*old.LoadBalancerDescriptions[0].LoadBalancerName != "old" {
		t.Fatal(old, err)
	}

	current, err := v2.DescribeLoadBalancers(ctx, &elbv2sdk.DescribeLoadBalancersInput{})
	if err != nil || len(current.LoadBalancers) != 1 || *current.LoadBalancers[0].LoadBalancerName != "new" {
		t.Fatal(current, err)
	}

	_, err = classic.DescribeLoadBalancers(ctx, &sdk.DescribeLoadBalancersInput{LoadBalancerNames: []string{"new"}})
	code(t, err, "LoadBalancerNotFound")
	_, err = v2.DescribeLoadBalancers(ctx, &elbv2sdk.DescribeLoadBalancersInput{Names: []string{"old"}})
	code(t, err, "LoadBalancerNotFound")
}

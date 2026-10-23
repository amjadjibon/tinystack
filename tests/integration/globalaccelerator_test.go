package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/globalaccelerator"
	"github.com/aws/aws-sdk-go-v2/service/globalaccelerator/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func gaClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func gaError(t *testing.T, err error, code string) {
	t.Helper()
	var apiError smithy.APIError
	if !errors.As(err, &apiError) || apiError.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func gaCreate(t *testing.T, c *sdk.Client, name string) *types.Accelerator {
	t.Helper()
	out, err := c.CreateAccelerator(context.Background(), &sdk.CreateAcceleratorInput{Name: new(name)})
	if err != nil {
		t.Fatal(err)
	}

	return out.Accelerator
}

func TestGlobalAcceleratorLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := gaClient(s, "test", "us-west-2")
	in := &sdk.CreateAcceleratorInput{
		Name: new("accelerator"), IpAddressType: types.IpAddressTypeDualStack,
		IdempotencyToken: new("create-token"), Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	}

	created, err := c.CreateAccelerator(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	a := created.Accelerator
	if a.Status != types.AcceleratorStatusInProgress || len(a.IpSets) != 2 || len(a.IpSets[0].IpAddresses) != 2 ||
		!regexp.MustCompile(`^a[0-9a-f]{16}\.awsglobalaccelerator\.com$`).
			MatchString(*a.DnsName) ||
		a.DualStackDnsName == nil {
		t.Fatalf("create snapshot: %+v", a)
	}

	again, err := c.CreateAccelerator(ctx, in)
	if err != nil || *again.Accelerator.AcceleratorArn != *a.AcceleratorArn {
		t.Fatalf("idempotency: %+v %v", again, err)
	}

	in.Name = new("changed-token-params")
	_, err = c.CreateAccelerator(ctx, in)
	gaError(t, err, "InvalidArgumentException")
	// Existing global service convention: the signing Region does not partition records.
	elsewhere := gaClient(s, "test", "eu-west-1")
	described, err := elsewhere.DescribeAccelerator(
		ctx,
		&sdk.DescribeAcceleratorInput{AcceleratorArn: a.AcceleratorArn},
	)
	if err != nil || described.Accelerator.Status != types.AcceleratorStatusDeployed {
		t.Fatalf("global describe: %+v %v", described, err)
	}

	_, err = gaClient(
		s,
		"111122223333",
		"us-west-2",
	).DescribeAccelerator(ctx, &sdk.DescribeAcceleratorInput{AcceleratorArn: a.AcceleratorArn})
	gaError(t, err, "AcceleratorNotFoundException")
	second := gaCreate(t, c, "second")
	if reflect.DeepEqual(second.IpSets[0].IpAddresses, a.IpSets[0].IpAddresses) {
		t.Fatal("duplicate static IPs")
	}

	page, err := c.ListAccelerators(ctx, &sdk.ListAcceleratorsInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Accelerators) != 1 || page.NextToken == nil {
		t.Fatalf("page: %+v %v", page, err)
	}

	page2, err := c.ListAccelerators(
		ctx,
		&sdk.ListAcceleratorsInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(page2.Accelerators) != 1 || page2.NextToken != nil ||
		*page.Accelerators[0].AcceleratorArn == *page2.Accelerators[0].AcceleratorArn {
		t.Fatalf("second page: %+v %v", page2, err)
	}

	_, err = c.ListAccelerators(ctx, &sdk.ListAcceleratorsInput{NextToken: new("bad-token")})
	gaError(t, err, "InvalidArgumentException")
	_, err = c.DeleteAccelerator(ctx, &sdk.DeleteAcceleratorInput{AcceleratorArn: a.AcceleratorArn})
	gaError(t, err, "AcceleratorNotDisabledException")
	updated, err := c.UpdateAccelerator(ctx, &sdk.UpdateAcceleratorInput{
		AcceleratorArn: a.AcceleratorArn, Enabled: new(false),
		Name: new("renamed"), IpAddressType: types.IpAddressTypeIpv4,
	})
	if err != nil || updated.Accelerator.Status != types.AcceleratorStatusInProgress ||
		len(updated.Accelerator.IpSets) != 1 {
		t.Fatalf("update: %+v %v", updated, err)
	}

	dual, err := c.UpdateAccelerator(
		ctx,
		&sdk.UpdateAcceleratorInput{AcceleratorArn: a.AcceleratorArn, IpAddressType: types.IpAddressTypeDualStack},
	)
	if err != nil || !reflect.DeepEqual(dual.Accelerator.IpSets, a.IpSets) {
		t.Fatalf("stable addresses: %+v %v", dual, err)
	}

	_, err = c.UpdateAcceleratorAttributes(ctx, &sdk.UpdateAcceleratorAttributesInput{
		AcceleratorArn:  a.AcceleratorArn,
		FlowLogsEnabled: new(true), FlowLogsS3Bucket: new("absent-bucket"), FlowLogsS3Prefix: new("logs/"),
	})
	gaError(t, err, "InvalidArgumentException")
	buckets := s3.New(
		s3.Options{Region: "us-west-2", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	_, err = buckets.CreateBucket(
		ctx,
		&s3.CreateBucketInput{
			Bucket: new("ga-flow-logs"),
			CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraintUsWest2,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateAcceleratorAttributes(ctx, &sdk.UpdateAcceleratorAttributesInput{
		AcceleratorArn:  a.AcceleratorArn,
		FlowLogsEnabled: new(true), FlowLogsS3Bucket: new("ga-flow-logs"), FlowLogsS3Prefix: new("logs/"),
	})
	if err != nil {
		t.Fatal(err)
	}

	attrs, err := c.DescribeAcceleratorAttributes(
		ctx,
		&sdk.DescribeAcceleratorAttributesInput{AcceleratorArn: a.AcceleratorArn},
	)
	if err != nil || !*attrs.AcceleratorAttributes.FlowLogsEnabled ||
		*attrs.AcceleratorAttributes.FlowLogsS3Bucket != "ga-flow-logs" {
		t.Fatalf("flow logs: %+v %v", attrs, err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceArn: a.AcceleratorArn,
			Tags:        []types.Tag{{Key: new("owner"), Value: new("platform")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: a.AcceleratorArn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: a.AcceleratorArn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "owner" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	tagger := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagger.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{*a.AcceleratorArn},
			Tags:            map[string]string{"inventory": "yes"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("inventory tag: %+v %v", tagged, err)
	}

	resources, err := tagger.GetResources(
		ctx,
		&rgta.GetResourcesInput{ResourceTypeFilters: []string{"globalaccelerator:accelerator"}},
	)
	if err != nil || len(resources.ResourceTagMappingList) != 1 {
		t.Fatalf("inventory: %+v %v", resources, err)
	}

	listener, err := c.CreateListener(ctx, &sdk.CreateListenerInput{
		AcceleratorArn: a.AcceleratorArn, Protocol: types.ProtocolTcp,
		PortRanges: []types.PortRange{{FromPort: new(int32(80)), ToPort: new(int32(90))}},
	})
	if err != nil {
		t.Fatal(err)
	}

	l := listener.Listener.ListenerArn
	_, err = c.DeleteAccelerator(ctx, &sdk.DeleteAcceleratorInput{AcceleratorArn: a.AcceleratorArn})
	gaError(t, err, "AssociatedListenerFoundException")
	_, err = c.CreateListener(ctx, &sdk.CreateListenerInput{
		AcceleratorArn: a.AcceleratorArn, Protocol: types.ProtocolTcp,
		PortRanges: []types.PortRange{{FromPort: new(int32(90)), ToPort: new(int32(100))}},
	})
	gaError(t, err, "InvalidPortRangeException")
	udp, err := c.CreateListener(ctx, &sdk.CreateListenerInput{
		AcceleratorArn: a.AcceleratorArn, Protocol: types.ProtocolUdp,
		PortRanges: []types.PortRange{{FromPort: new(int32(80)), ToPort: new(int32(90))}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateListener(
		ctx,
		&sdk.UpdateListenerInput{ListenerArn: udp.Listener.ListenerArn, Protocol: types.ProtocolTcp},
	)
	gaError(t, err, "InvalidPortRangeException")
	_, err = c.UpdateListener(
		ctx,
		&sdk.UpdateListenerInput{ListenerArn: l, ClientAffinity: types.ClientAffinitySourceIp},
	)
	if err != nil {
		t.Fatal(err)
	}

	ld, err := c.DescribeListener(ctx, &sdk.DescribeListenerInput{ListenerArn: l})
	if err != nil || ld.Listener.ClientAffinity != types.ClientAffinitySourceIp {
		t.Fatalf("listener: %+v %v", ld, err)
	}

	listeners, err := c.ListListeners(
		ctx,
		&sdk.ListListenersInput{AcceleratorArn: a.AcceleratorArn, MaxResults: new(int32(1))},
	)
	if err != nil || listeners.NextToken == nil {
		t.Fatalf("listener page: %+v %v", listeners, err)
	}

	listeners2, err := c.ListListeners(
		ctx,
		&sdk.ListListenersInput{AcceleratorArn: a.AcceleratorArn, NextToken: listeners.NextToken},
	)
	if err != nil || len(listeners2.Listeners) != 1 {
		t.Fatalf("listener page2: %+v %v", listeners2, err)
	}

	// This accelerator has no endpoints; switch to IPV4 for EIP/IPv4 load balancer endpoints.
	_, err = c.UpdateAccelerator(
		ctx,
		&sdk.UpdateAcceleratorInput{AcceleratorArn: a.AcceleratorArn, IpAddressType: types.IpAddressTypeIpv4},
	)
	if err != nil {
		t.Fatal(err)
	}

	net := ec2Client(s, "test", "us-east-1")
	eip, err := net.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeVpc})
	if err != nil {
		t.Fatal(err)
	}

	_, subnets := elbSubnets(t, s)
	alb, err := elbClient(
		s,
	).CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{Name: new("ga-alb"), Subnets: subnets})
	if err != nil {
		t.Fatal(err)
	}

	nlb, err := elbClient(
		s,
	).CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{
		Name: new("ga-nlb"), Subnets: subnets,
		Type: elbtypes.LoadBalancerTypeEnumNetwork,
	})
	if err != nil {
		t.Fatal(err)
	}

	launched, err := net.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      latestImage(t, s),
		InstanceType: ec2types.InstanceTypeT3Micro, MinCount: new(int32(1)), MaxCount: new(int32(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	instanceID := launched.Instances[0].InstanceId
	waiter := ec2.NewInstanceRunningWaiter(net, func(options *ec2.InstanceRunningWaiterOptions) {
		options.MinDelay = 10 * time.Millisecond
		options.MaxDelay = 20 * time.Millisecond
	})
	if err = waiter.Wait(
		ctx,
		&ec2.DescribeInstancesInput{InstanceIds: []string{*instanceID}},
		5*time.Second,
	); err != nil {
		t.Fatal(err)
	}

	groupIn := &sdk.CreateEndpointGroupInput{
		ListenerArn:            l,
		EndpointGroupRegion:    new("us-east-1"),
		EndpointConfigurations: []types.EndpointConfiguration{{EndpointId: eip.AllocationId, Weight: new(int32(12))}},
		PortOverrides: []types.PortOverride{
			{ListenerPort: new(int32(80)), EndpointPort: new(int32(8080))},
		},
		TrafficDialPercentage: new(float32(75)),
	}

	group, err := c.CreateEndpointGroup(ctx, groupIn)
	if err != nil {
		t.Fatal(err)
	}

	g := group.EndpointGroup.EndpointGroupArn
	if group.EndpointGroup.EndpointDescriptions[0].HealthState != types.HealthStateInitial {
		t.Fatal("unprobed endpoint must be INITIAL")
	}

	groupIn.IdempotencyToken = new("another-group-token")
	_, err = c.CreateEndpointGroup(ctx, groupIn)
	gaError(t, err, "EndpointGroupAlreadyExistsException")
	_, err = c.DeleteListener(ctx, &sdk.DeleteListenerInput{ListenerArn: l})
	gaError(t, err, "AssociatedEndpointGroupFoundException")
	for _, id := range []string{
		"eipalloc-missing", "i-missing",
		"arn:aws:elasticloadbalancing:us-west-2:000000000000:loadbalancer/app/missing/1234",
	} {
		_, err = c.AddEndpoints(
			ctx,
			&sdk.AddEndpointsInput{
				EndpointGroupArn:       g,
				EndpointConfigurations: []types.EndpointConfiguration{{EndpointId: new(id)}},
			},
		)
		gaError(t, err, "InvalidArgumentException")
	}

	instanceEndpoint, err := c.AddEndpoints(ctx, &sdk.AddEndpointsInput{
		EndpointGroupArn: g,
		EndpointConfigurations: []types.EndpointConfiguration{
			{EndpointId: instanceID, ClientIPPreservationEnabled: new(true)},
		},
	})
	if err != nil || len(instanceEndpoint.EndpointDescriptions) != 1 {
		t.Fatalf("EC2 endpoint: %+v %v", instanceEndpoint, err)
	}

	_, err = net.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{*instanceID}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateEndpointGroup(ctx, &sdk.UpdateEndpointGroupInput{
		EndpointGroupArn:       g,
		EndpointConfigurations: []types.EndpointConfiguration{{EndpointId: instanceID}},
	})
	gaError(t, err, "InvalidArgumentException")

	added, err := c.AddEndpoints(
		ctx,
		&sdk.AddEndpointsInput{EndpointGroupArn: g, EndpointConfigurations: []types.EndpointConfiguration{
			{EndpointId: alb.LoadBalancers[0].LoadBalancerArn}, {EndpointId: nlb.LoadBalancers[0].LoadBalancerArn},
		}},
	)
	if err != nil || len(added.EndpointDescriptions) != 2 ||
		!*added.EndpointDescriptions[0].ClientIPPreservationEnabled {
		t.Fatalf("add: %+v %v", added, err)
	}

	_, err = c.RemoveEndpoints(
		ctx,
		&sdk.RemoveEndpointsInput{
			EndpointGroupArn:    g,
			EndpointIdentifiers: []types.EndpointIdentifier{{EndpointId: eip.AllocationId}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := c.UpdateEndpointGroup(ctx, &sdk.UpdateEndpointGroupInput{
		EndpointGroupArn:           g,
		TrafficDialPercentage:      new(float32(25)),
		HealthCheckProtocol:        types.HealthCheckProtocolHttps,
		HealthCheckPath:            new("/ready"),
		HealthCheckIntervalSeconds: new(int32(10)),
		ThresholdCount: new(
			int32(5),
		),
		HealthCheckPort: new(int32(443)),
		EndpointConfigurations: []types.EndpointConfiguration{
			{
				EndpointId:                  alb.LoadBalancers[0].LoadBalancerArn,
				Weight:                      new(int32(64)),
				ClientIPPreservationEnabled: new(false),
			},
		},
		PortOverrides: []types.PortOverride{{ListenerPort: new(int32(81)), EndpointPort: new(int32(8081))}},
	})
	if err != nil || *changed.EndpointGroup.TrafficDialPercentage != 25 {
		t.Fatalf("update group: %+v %v", changed, err)
	}

	gd, err := c.DescribeEndpointGroup(ctx, &sdk.DescribeEndpointGroupInput{EndpointGroupArn: g})
	if err != nil || *gd.EndpointGroup.HealthCheckPath != "/ready" || len(gd.EndpointGroup.EndpointDescriptions) != 1 ||
		*gd.EndpointGroup.EndpointDescriptions[0].ClientIPPreservationEnabled {
		t.Fatalf("group: %+v %v", gd, err)
	}

	otherGroup, err := c.CreateEndpointGroup(
		ctx,
		&sdk.CreateEndpointGroupInput{ListenerArn: l, EndpointGroupRegion: new("eu-west-1")},
	)
	if err != nil {
		t.Fatal(err)
	}

	groups, err := c.ListEndpointGroups(ctx, &sdk.ListEndpointGroupsInput{ListenerArn: l, MaxResults: new(int32(1))})
	if err != nil || groups.NextToken == nil {
		t.Fatalf("group page: %+v %v", groups, err)
	}

	groups2, err := c.ListEndpointGroups(ctx, &sdk.ListEndpointGroupsInput{ListenerArn: l, NextToken: groups.NextToken})
	if err != nil || len(groups2.EndpointGroups) != 1 {
		t.Fatalf("group page2: %+v %v", groups2, err)
	}

	for _, arn := range []*string{g, otherGroup.EndpointGroup.EndpointGroupArn} {
		if _, err = c.DeleteEndpointGroup(ctx, &sdk.DeleteEndpointGroupInput{EndpointGroupArn: arn}); err != nil {
			t.Fatal(err)
		}
	}

	for _, arn := range []*string{l, udp.Listener.ListenerArn} {
		if _, err = c.DeleteListener(ctx, &sdk.DeleteListenerInput{ListenerArn: arn}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteAccelerator(ctx, &sdk.DeleteAcceleratorInput{AcceleratorArn: a.AcceleratorArn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeAccelerator(ctx, &sdk.DescribeAcceleratorInput{AcceleratorArn: a.AcceleratorArn})
	gaError(t, err, "AcceleratorNotFoundException")
}

func TestGlobalAcceleratorSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := gaClient(s, "test", "us-west-2")
	a := gaCreate(t, c, "persistent")
	l, err := c.CreateListener(ctx, &sdk.CreateListenerInput{
		AcceleratorArn: a.AcceleratorArn, Protocol: types.ProtocolTcp,
		PortRanges: []types.PortRange{{FromPort: new(int32(443)), ToPort: new(int32(443))}},
	})
	if err != nil {
		t.Fatal(err)
	}

	g, err := c.CreateEndpointGroup(
		ctx,
		&sdk.CreateEndpointGroupInput{ListenerArn: l.Listener.ListenerArn, EndpointGroupRegion: new("us-east-1")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceArn: a.AcceleratorArn,
			Tags:        []types.Tag{{Key: new("persist"), Value: new("yes")}},
		},
	); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = gaClient(s, "test", "ap-southeast-1")
	d, err := c.DescribeAccelerator(ctx, &sdk.DescribeAcceleratorInput{AcceleratorArn: a.AcceleratorArn})
	if err != nil || !reflect.DeepEqual(d.Accelerator.IpSets, a.IpSets) || *d.Accelerator.DnsName != *a.DnsName ||
		d.Accelerator.Status != types.AcceleratorStatusDeployed {
		t.Fatalf("restart accelerator: %+v %v", d, err)
	}

	if _, err = c.DescribeListener(ctx, &sdk.DescribeListenerInput{ListenerArn: l.Listener.ListenerArn}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DescribeEndpointGroup(
		ctx,
		&sdk.DescribeEndpointGroupInput{EndpointGroupArn: g.EndpointGroup.EndpointGroupArn},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: a.AcceleratorArn})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatalf("restart tags: %+v %v", tags, err)
	}

	b := gaCreate(t, c, "another")
	if reflect.DeepEqual(b.IpSets, a.IpSets) {
		t.Fatal("restart reused live static IPs")
	}
}

func TestGlobalAcceleratorUnsupportedInputs(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := gaClient(s, "test", "us-west-2")
	_, err := c.CreateAccelerator(
		ctx,
		&sdk.CreateAcceleratorInput{Name: new("byoip"), IpAddresses: []string{"192.0.2.1"}},
	)
	gaError(t, err, "ValidationException")
	_, err = c.ProvisionByoipCidr(ctx, &sdk.ProvisionByoipCidrInput{
		Cidr:                     new("192.0.2.0/24"),
		CidrAuthorizationContext: &types.CidrAuthorizationContext{Message: new("fixture"), Signature: new("fixture")},
	})
	gaError(t, err, "UnknownOperationException")
	_, err = c.ListCustomRoutingAccelerators(ctx, &sdk.ListCustomRoutingAcceleratorsInput{})
	gaError(t, err, "UnknownOperationException")
	_, err = c.ListCrossAccountAttachments(ctx, &sdk.ListCrossAccountAttachmentsInput{})
	gaError(t, err, "UnknownOperationException")
}

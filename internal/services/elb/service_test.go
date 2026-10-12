package elb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/elb"
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

func listener(port, instancePort int64) api.Listener {
	return api.Listener{Protocol: new("http"), LoadBalancerPort: new(port), InstancePort: new(instancePort)}
}

// fixture returns a service with a VPC of two subnets in different zones.
func fixture(t *testing.T, ctx context.Context) (s *elb.Service, subnets []string) {
	t.Helper()
	store := memory.New(nil, nil)
	network := ec2.New(store)
	s = elb.New(store, network)
	vpc, err := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	for i, zone := range []string{"us-east-1a", "us-east-1b"} {
		sn, e := network.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{
			VpcId:            vpc.Vpc.VpcId,
			CidrBlock:        new([]string{"10.0.1.0/24", "10.0.2.0/24"}[i]),
			AvailabilityZone: new(zone),
		})
		if e != nil {
			t.Fatal(e)
		}

		subnets = append(subnets, *sn.Subnet.SubnetId)
	}

	return s, subnets
}

func TestLoadBalancerLifecycleScopeAndValidation(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	s, subnets := fixture(t, ctx)
	in := &api.CreateAccessPointInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{listener(80, 8080)}, Subnets: subnets,
		Tags: []api.Tag{{Key: new("env"), Value: new("test")}},
	}
	created, err := s.CreateLoadBalancer(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	in.Listeners[0].Protocol = new("mutated")
	described, err := s.DescribeLoadBalancers(ctx, &api.DescribeAccessPointsInput{LoadBalancerNames: []string{"web"}})
	if err != nil || len(described.LoadBalancerDescriptions) != 1 {
		t.Fatal(described, err)
	}

	lb := described.LoadBalancerDescriptions[0]
	if *lb.DNSName != *created.DNSName || *lb.ListenerDescriptions[0].Listener.Protocol != "HTTP" ||
		*lb.ListenerDescriptions[0].Listener.InstanceProtocol != "HTTP" || *lb.HealthCheck.Target != "TCP:8080" ||
		*lb.Scheme != "internet-facing" || len(lb.SecurityGroups) != 1 {
		t.Fatalf("%+v", lb)
	}

	_, err = s.CreateLoadBalancer(ctx, &api.CreateAccessPointInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{listener(80, 80)}, Subnets: subnets,
	})
	wantCode(t, err, "DuplicateAccessPointNameException")

	for name, bad := range map[string]*api.CreateAccessPointInput{
		"ValidationException": {LoadBalancerName: new("-x"), Listeners: []api.Listener{listener(80, 80)}, Subnets: subnets},
		"InvalidSubnetException": {
			LoadBalancerName: new("zones"), Listeners: []api.Listener{listener(80, 80)},
			Subnets: []string{subnets[0], subnets[0]},
		},
		"InvalidConfigurationRequestException": {
			LoadBalancerName: new("mixed"), Listeners: []api.Listener{listener(80, 80)}, Subnets: subnets,
			AvailabilityZones: []string{"us-east-1a"},
		},
		"DuplicateListenerException": {
			LoadBalancerName: new("ports"), Subnets: subnets,
			Listeners: []api.Listener{listener(80, 80), listener(80, 81)},
		},
	} {
		_, err = s.CreateLoadBalancer(ctx, bad)
		wantCode(t, err, name)
	}

	// Another account or Region does not see the load balancer, and may reuse its name.
	other := testContext("210987654321", "us-east-1")
	_, err = s.DescribeLoadBalancers(other, &api.DescribeAccessPointsInput{LoadBalancerNames: []string{"web"}})
	wantCode(t, err, "AccessPointNotFoundException")

	_, err = s.CreateLoadBalancer(testContext("123456789012", "us-west-2"), &api.CreateAccessPointInput{
		LoadBalancerName: new(
			"web",
		),
		Listeners:         []api.Listener{listener(80, 80)},
		AvailabilityZones: []string{"us-west-2a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.DeleteLoadBalancer(ctx, &api.DeleteAccessPointInput{LoadBalancerName: new("web")}); err != nil {
		t.Fatal(err)
	}

	if _, err = s.DeleteLoadBalancer(ctx, &api.DeleteAccessPointInput{LoadBalancerName: new("web")}); err != nil {
		t.Fatal("delete is idempotent:", err)
	}
}

func TestListenersAreCheckedAsAWholeSet(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	s, subnets := fixture(t, ctx)
	_, err := s.CreateLoadBalancer(ctx, &api.CreateAccessPointInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{listener(80, 8080)}, Subnets: subnets,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A secure and a plain listener cannot share an instance port; the rejected request leaves the listeners alone.
	_, err = s.CreateLoadBalancerListeners(ctx, &api.CreateLoadBalancerListenerInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{
			listener(81, 9000),
			{
				Protocol: new("HTTPS"), LoadBalancerPort: new(int64(443)), InstancePort: new(int64(8080)),
				SSLCertificateId: new("arn:aws:iam::123456789012:server-certificate/web"),
			},
		},
	})
	wantCode(t, err, "InvalidConfigurationRequestException")

	described, err := s.DescribeLoadBalancers(ctx, &api.DescribeAccessPointsInput{})
	if err != nil || len(described.LoadBalancerDescriptions[0].ListenerDescriptions) != 1 {
		t.Fatal(described, err)
	}

	_, err = s.CreateLoadBalancerListeners(ctx, &api.CreateLoadBalancerListenerInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{listener(80, 8081)},
	})
	wantCode(t, err, "DuplicateListenerException")

	_, err = s.CreateLoadBalancerListeners(ctx, &api.CreateLoadBalancerListenerInput{
		LoadBalancerName: new("missing"), Listeners: []api.Listener{listener(81, 9000)},
	})
	wantCode(t, err, "AccessPointNotFoundException")
}

func TestHealthCheckAndAttributesRejectWholeCandidates(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	s, subnets := fixture(t, ctx)
	_, err := s.CreateLoadBalancer(ctx, &api.CreateAccessPointInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{listener(80, 8080)}, Subnets: subnets,
	})
	if err != nil {
		t.Fatal(err)
	}

	check := func(target string, interval, timeout int64) error {
		_, e := s.ConfigureHealthCheck(ctx, &api.ConfigureHealthCheckInput{
			LoadBalancerName: new("web"), HealthCheck: &api.HealthCheck{
				Target: new(target), Interval: new(interval), Timeout: new(timeout),
				UnhealthyThreshold: new(int64(2)), HealthyThreshold: new(int64(2)),
			},
		})
		return e
	}

	for _, bad := range []struct {
		target            string
		interval, timeout int64
	}{{"HTTP:80", 30, 5}, {"TCP:80/x", 30, 5}, {"FTP:80", 30, 5}, {"TCP:0", 30, 5}, {"TCP:80", 10, 10}, {"TCP:80", 4, 2}} {
		wantCode(t, check(bad.target, bad.interval, bad.timeout), "ValidationException")
	}

	if err = check("HTTPS:443/ping", 10, 5); err != nil {
		t.Fatal(err)
	}

	// The valid cross-zone change is dropped together with the invalid access log.
	_, err = s.ModifyLoadBalancerAttributes(ctx, &api.ModifyLoadBalancerAttributesInput{
		LoadBalancerName: new("web"), LoadBalancerAttributes: &api.LoadBalancerAttributes{
			CrossZoneLoadBalancing: &api.CrossZoneLoadBalancing{Enabled: new(true)},
			AccessLog: &api.AccessLog{
				Enabled:      new(true),
				S3BucketName: new("logs"),
				EmitInterval: new(int64(7)),
			},
		},
	})
	wantCode(t, err, "ValidationException")

	attributes, err := s.DescribeLoadBalancerAttributes(
		ctx,
		&api.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("web")},
	)
	if err != nil || *attributes.LoadBalancerAttributes.CrossZoneLoadBalancing.Enabled {
		t.Fatal(attributes, err)
	}

	// Switching a group off drops the settings that depended on it.
	for _, enabled := range []bool{true, false} {
		_, err = s.ModifyLoadBalancerAttributes(ctx, &api.ModifyLoadBalancerAttributesInput{
			LoadBalancerName: new("web"), LoadBalancerAttributes: &api.LoadBalancerAttributes{
				AccessLog: &api.AccessLog{Enabled: new(enabled), S3BucketName: new("logs"), S3BucketPrefix: new("p")},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	attributes, err = s.DescribeLoadBalancerAttributes(
		ctx,
		&api.DescribeLoadBalancerAttributesInput{LoadBalancerName: new("web")},
	)
	if log := attributes.LoadBalancerAttributes.AccessLog; err != nil || *log.Enabled || log.S3BucketName != nil ||
		log.EmitInterval != nil {
		t.Fatal(attributes, err)
	}
}

func TestInstancesAndTags(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	s, subnets := fixture(t, ctx)
	_, err := s.CreateLoadBalancer(ctx, &api.CreateAccessPointInput{
		LoadBalancerName: new("web"), Listeners: []api.Listener{listener(80, 8080)}, Subnets: subnets,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.RegisterInstancesWithLoadBalancer(ctx, &api.RegisterEndPointsInput{
		LoadBalancerName: new("web"), Instances: []api.Instance{{InstanceId: new("i-0123456789abcdef0")}},
	})
	wantCode(t, err, "InvalidEndPointException")

	_, err = s.DeregisterInstancesFromLoadBalancer(ctx, &api.DeregisterEndPointsInput{
		LoadBalancerName: new("web"), Instances: []api.Instance{{InstanceId: new("i-0123456789abcdef0")}},
	})
	wantCode(t, err, "InvalidEndPointException")

	_, err = s.AddTags(ctx, &api.AddTagsInput{
		LoadBalancerNames: []string{"web", "missing"}, Tags: []api.Tag{{Key: new("a"), Value: new("1")}},
	})
	wantCode(t, err, "AccessPointNotFoundException")

	tags := &api.DescribeTagsInput{LoadBalancerNames: []string{"web"}}
	described, err := s.DescribeTags(ctx, tags)
	if err != nil || len(described.TagDescriptions[0].Tags) != 0 {
		t.Fatal("the failed AddTags changed tags:", described, err)
	}

	for _, bad := range [][]api.Tag{
		{{Key: new("a"), Value: new("1")}, {Key: new("a"), Value: new("2")}},
		{{Key: new("aws:x"), Value: new("1")}},
		{{Key: new("bad key!"), Value: new("1")}},
	} {
		_, err = s.AddTags(ctx, &api.AddTagsInput{LoadBalancerNames: []string{"web"}, Tags: bad})
		if err == nil {
			t.Fatal("accepted", bad)
		}
	}

	many := make([]api.Tag, 51)
	for i := range many {
		many[i] = api.Tag{Key: new(string(rune('a'+i%26)) + string(rune('a'+i/26))), Value: new("v")}
	}

	_, err = s.AddTags(ctx, &api.AddTagsInput{LoadBalancerNames: []string{"web"}, Tags: many})
	wantCode(t, err, "TooManyTagsException")

	if _, err = s.AddTags(ctx, &api.AddTagsInput{
		LoadBalancerNames: []string{"web"}, Tags: []api.Tag{{Key: new("a"), Value: new("1")}, {Key: new("b")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = s.RemoveTags(ctx, &api.RemoveTagsInput{
		LoadBalancerNames: []string{"web"}, Tags: []api.TagKeyOnly{{Key: new("a")}},
	}); err != nil {
		t.Fatal(err)
	}

	described, err = s.DescribeTags(ctx, tags)
	if tags := described.TagDescriptions[0].Tags; err != nil || len(tags) != 1 || *tags[0].Key != "b" ||
		*tags[0].Value != "" {
		t.Fatal(described, err)
	}
}

func TestDescribeLoadBalancersPaginatesAndResumesAfterDeletion(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	s, _ := fixture(t, ctx)
	for _, name := range []string{"a", "b", "c"} {
		_, err := s.CreateLoadBalancer(ctx, &api.CreateAccessPointInput{
			LoadBalancerName: new(name), Listeners: []api.Listener{listener(80, 80)},
			AvailabilityZones: []string{"us-east-1a"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.DescribeLoadBalancers(ctx, &api.DescribeAccessPointsInput{PageSize: new(int64(2))})
	if err != nil || len(first.LoadBalancerDescriptions) != 2 || first.NextMarker == nil {
		t.Fatal(first, err)
	}

	if _, err = s.DeleteLoadBalancer(ctx, &api.DeleteAccessPointInput{LoadBalancerName: new("b")}); err != nil {
		t.Fatal(err)
	}

	rest, err := s.DescribeLoadBalancers(ctx, &api.DescribeAccessPointsInput{Marker: first.NextMarker})
	if err != nil || len(rest.LoadBalancerDescriptions) != 1 ||
		*rest.LoadBalancerDescriptions[0].LoadBalancerName != "c" {
		t.Fatal(rest, err)
	}

	_, err = s.DescribeLoadBalancers(ctx, &api.DescribeAccessPointsInput{PageSize: new(int64(401))})
	wantCode(t, err, "ValidationException")
	_, err = s.DescribeLoadBalancers(ctx, &api.DescribeAccessPointsInput{Marker: new("!")})
	wantCode(t, err, "ValidationException")
}

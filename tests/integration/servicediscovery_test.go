package integration_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
)

func creds(key string) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
	})
}

func TestServiceDiscoveryLifecycle(t *testing.T) {
	s := server(t)
	// Keep SDK host-prefix resolution intact; supply local DNS at the transport boundary.
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		address = strings.TrimPrefix(address, "data-")
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	ctx := context.Background()
	c := servicediscovery.New(
		servicediscovery.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Retryer:      aws.NopRetryer{},
			Credentials:  creds("test"),
			HTTPClient:   &http.Client{Transport: transport},
		},
	)
	created, err := c.CreateHttpNamespace(ctx, &servicediscovery.CreateHttpNamespaceInput{Name: new("local")})
	if err != nil {
		t.Fatal(err)
	}

	op, err := c.GetOperation(ctx, &servicediscovery.GetOperationInput{OperationId: created.OperationId})
	if err != nil || op.Operation.Status != types.OperationStatusSuccess {
		t.Fatalf("operation %v %+v", err, op)
	}

	nsID := op.Operation.Targets["NAMESPACE"]
	ns, err := c.GetNamespace(ctx, &servicediscovery.GetNamespaceInput{Id: new(nsID)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateHttpNamespace(ctx, &servicediscovery.CreateHttpNamespaceInput{Name: new("local")})
	code(t, err, "NamespaceAlreadyExists")
	_, err = c.GetNamespace(ctx, &servicediscovery.GetNamespaceInput{Id: new("missing")})
	code(t, err, "NamespaceNotFound")
	_, err = c.GetOperation(ctx, &servicediscovery.GetOperationInput{OperationId: new("missing")})
	code(t, err, "OperationNotFound")
	_, err = c.CreatePublicDnsNamespace(ctx, &servicediscovery.CreatePublicDnsNamespaceInput{Name: new("public.local")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreatePrivateDnsNamespace(
		ctx,
		&servicediscovery.CreatePrivateDnsNamespaceInput{Name: new("private.local"), Vpc: new("vpc-record")},
	)
	if err != nil {
		t.Fatal(err)
	}

	pages, err := c.ListNamespaces(ctx, &servicediscovery.ListNamespacesInput{MaxResults: new(int32(1))})
	if err != nil || len(pages.Namespaces) != 1 || pages.NextToken == nil {
		t.Fatalf("page %v %+v", err, pages)
	}

	next, err := c.ListNamespaces(
		ctx,
		&servicediscovery.ListNamespacesInput{NextToken: pages.NextToken, MaxResults: new(int32(1))},
	)
	if err != nil || len(next.Namespaces) != 1 {
		t.Fatalf("next %v %+v", err, next)
	}

	filtered, err := c.ListNamespaces(
		ctx,
		&servicediscovery.ListNamespacesInput{
			Filters: []types.NamespaceFilter{{Name: types.NamespaceFilterNameType, Values: []string{"HTTP"}}},
		},
	)
	if err != nil || len(filtered.Namespaces) != 1 {
		t.Fatalf("filter %v %+v", err, filtered)
	}

	_, err = c.UpdateHttpNamespace(
		ctx,
		&servicediscovery.UpdateHttpNamespaceInput{
			Id:        new(nsID),
			Namespace: &types.HttpNamespaceChange{Description: new("updated")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	svc, err := c.CreateService(
		ctx,
		&servicediscovery.CreateServiceInput{
			Name:                    new("api"),
			NamespaceId:             new(nsID),
			HealthCheckCustomConfig: &types.HealthCheckCustomConfig{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	sid := svc.Service.Id
	_, err = c.CreateService(ctx, &servicediscovery.CreateServiceInput{Name: new("api"), NamespaceId: new(nsID)})
	code(t, err, "ServiceAlreadyExists")
	_, err = c.GetService(ctx, &servicediscovery.GetServiceInput{Id: new("missing")})
	code(t, err, "ServiceNotFound")
	_, err = c.DeleteNamespace(ctx, &servicediscovery.DeleteNamespaceInput{Id: new(nsID)})
	code(t, err, "ResourceInUse")
	_, err = c.GetService(ctx, &servicediscovery.GetServiceInput{Id: sid})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateService(
		ctx,
		&servicediscovery.UpdateServiceInput{
			Id:      sid,
			Service: &types.ServiceChange{Description: new("updated service")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListServices(
		ctx,
		&servicediscovery.ListServicesInput{
			Filters: []types.ServiceFilter{{Name: types.ServiceFilterNameNamespaceId, Values: []string{nsID}}},
		},
	)
	if err != nil || len(listed.Services) != 1 {
		t.Fatalf("services %v %+v", err, listed)
	}

	for _, id := range []string{"a", "b", "c"} {
		_, err = c.RegisterInstance(
			ctx,
			&servicediscovery.RegisterInstanceInput{
				ServiceId:  sid,
				InstanceId: new(id),
				Attributes: map[string]string{"version": id, "AWS_INSTANCE_IPV4": "127.0.0.1"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteService(ctx, &servicediscovery.DeleteServiceInput{Id: sid})
	code(t, err, "ResourceInUse")
	_, err = c.GetInstance(ctx, &servicediscovery.GetInstanceInput{ServiceId: sid, InstanceId: new("missing")})
	code(t, err, "InstanceNotFound")
	inst, err := c.GetInstance(ctx, &servicediscovery.GetInstanceInput{ServiceId: sid, InstanceId: new("a")})
	if err != nil || inst.Instance.Attributes["version"] != "a" {
		t.Fatalf("instance %v", err)
	}

	instances, err := c.ListInstances(
		ctx,
		&servicediscovery.ListInstancesInput{ServiceId: sid, MaxResults: new(int32(1))},
	)
	if err != nil || len(instances.Instances) != 1 || instances.NextToken == nil {
		t.Fatalf("instances %v %+v", err, instances)
	}

	// BaseEndpoint and the SDK's own endpoint resolver, without disabling host prefixes.
	found, err := c.DiscoverInstances(
		ctx,
		&servicediscovery.DiscoverInstancesInput{
			NamespaceName: new("local"),
			ServiceName:   new("api"),
			MaxResults:    new(int32(2)),
		},
	)
	if err != nil || len(found.Instances) != 2 {
		t.Fatalf("discover SDK endpoint: %v %+v", err, found)
	}

	revision, err := c.DiscoverInstancesRevision(
		ctx,
		&servicediscovery.DiscoverInstancesRevisionInput{NamespaceName: new("local"), ServiceName: new("api")},
	)
	if err != nil || aws.ToInt64(revision.InstancesRevision) != aws.ToInt64(found.InstancesRevision) {
		t.Fatalf("revision %v %+v", err, revision)
	}

	query, err := c.DiscoverInstances(
		ctx,
		&servicediscovery.DiscoverInstancesInput{
			NamespaceName:   new("local"),
			ServiceName:     new("api"),
			QueryParameters: map[string]string{"version": "b"},
		},
	)
	if err != nil || len(query.Instances) != 1 || aws.ToString(query.Instances[0].InstanceId) != "b" {
		t.Fatalf("query %v %+v", err, query)
	}

	optional, err := c.DiscoverInstances(
		ctx,
		&servicediscovery.DiscoverInstancesInput{
			NamespaceName:      new("local"),
			ServiceName:        new("api"),
			OptionalParameters: map[string]string{"version": "c"},
		},
	)
	if err != nil || len(optional.Instances) != 1 {
		t.Fatalf("optional %v %+v", err, optional)
	}

	fallback, err := c.DiscoverInstances(
		ctx,
		&servicediscovery.DiscoverInstancesInput{
			NamespaceName:      new("local"),
			ServiceName:        new("api"),
			OptionalParameters: map[string]string{"version": "none"},
		},
	)
	if err != nil || len(fallback.Instances) != 3 {
		t.Fatalf("optional fallback %v %+v", err, fallback)
	}

	_, err = c.UpdateInstanceCustomHealthStatus(
		ctx,
		&servicediscovery.UpdateInstanceCustomHealthStatusInput{
			ServiceId:  sid,
			InstanceId: new("b"),
			Status:     types.CustomHealthStatusUnhealthy,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	health, err := c.GetInstancesHealthStatus(
		ctx,
		&servicediscovery.GetInstancesHealthStatusInput{ServiceId: sid, Instances: []string{"b"}},
	)
	if err != nil || health.Status["b"] != types.HealthStatusUnhealthy {
		t.Fatalf("health %v %+v", err, health)
	}

	found, err = c.DiscoverInstances(
		ctx,
		&servicediscovery.DiscoverInstancesInput{NamespaceName: new("local"), ServiceName: new("api")},
	)
	if err != nil || len(found.Instances) != 2 {
		t.Fatalf("healthy filter %v %+v", err, found)
	}

	found, err = c.DiscoverInstances(
		ctx,
		&servicediscovery.DiscoverInstancesInput{
			NamespaceName: new("local"),
			ServiceName:   new("api"),
			HealthStatus:  types.HealthStatusFilterUnhealthy,
		},
	)
	if err != nil || len(found.Instances) != 1 {
		t.Fatalf("unhealthy filter %v %+v", err, found)
	}

	_, err = c.TagResource(
		ctx,
		&servicediscovery.TagResourceInput{
			ResourceARN: ns.Namespace.Arn,
			Tags:        []types.Tag{{Key: new("env"), Value: new("test")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ts, err := c.ListTagsForResource(ctx, &servicediscovery.ListTagsForResourceInput{ResourceARN: ns.Namespace.Arn})
	if err != nil || len(ts.Tags) != 1 {
		t.Fatalf("tags %v %+v", err, ts)
	}

	_, err = c.UntagResource(
		ctx,
		&servicediscovery.UntagResourceInput{ResourceARN: ns.Namespace.Arn, TagKeys: []string{"env"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	ops, err := c.ListOperations(ctx, &servicediscovery.ListOperationsInput{MaxResults: new(int32(1))})
	if err != nil || ops.NextToken == nil {
		t.Fatalf("operations page %v %+v", err, ops)
	}

	for _, identity := range []struct{ key, region string }{{"test", "eu-west-1"}, {"team-b", "us-east-1"}} {
		other := servicediscovery.New(
			servicediscovery.Options{
				Region:       identity.region,
				BaseEndpoint: new(s.URL),
				Retryer:      aws.NopRetryer{},
				Credentials:  creds(identity.key),
				HTTPClient:   &http.Client{Transport: transport},
			},
		)
		_, err = other.GetNamespace(ctx, &servicediscovery.GetNamespaceInput{Id: new(nsID)})
		code(t, err, "NamespaceNotFound")
		_, err = other.DiscoverInstances(
			ctx,
			&servicediscovery.DiscoverInstancesInput{NamespaceName: new("local"), ServiceName: new("api")},
		)
		code(t, err, "NamespaceNotFound")
	}

	_, err = c.CreateService(
		ctx,
		&servicediscovery.CreateServiceInput{
			Name:              new("unsupported"),
			NamespaceId:       new(nsID),
			HealthCheckConfig: &types.HealthCheckConfig{Type: types.HealthCheckTypeHttp},
		},
	)
	code(t, err, "ValidationException")
	_, err = c.ListOperations(
		ctx,
		&servicediscovery.ListOperationsInput{
			Filters: []types.OperationFilter{{Name: types.OperationFilterNameUpdateDate, Values: []string{"0"}}},
		},
	)
	code(t, err, "NotImplemented")
	for _, id := range []string{"a", "b", "c"} {
		_, err = c.DeregisterInstance(
			ctx,
			&servicediscovery.DeregisterInstanceInput{ServiceId: sid, InstanceId: new(id)},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteService(ctx, &servicediscovery.DeleteServiceInput{Id: sid})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteNamespace(ctx, &servicediscovery.DeleteNamespaceInput{Id: new(nsID)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceDiscoveryDNSRecordsAndWire(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := servicediscovery.New(
		servicediscovery.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	ns, err := c.CreatePrivateDnsNamespace(
		ctx,
		&servicediscovery.CreatePrivateDnsNamespaceInput{Name: new("dns.local"), Vpc: new("vpc-record")},
	)
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.GetOperation(ctx, &servicediscovery.GetOperationInput{OperationId: ns.OperationId})
	if err != nil {
		t.Fatal(err)
	}
	nsID := op.Operation.Targets["NAMESPACE"]
	created, err := c.CreateService(
		ctx,
		&servicediscovery.CreateServiceInput{
			Name:        new("dns"),
			NamespaceId: new(nsID),
			DnsConfig: &types.DnsConfig{
				RoutingPolicy: types.RoutingPolicyMultivalue,
				DnsRecords:    []types.DnsRecord{{Type: types.RecordTypeA, TTL: new(int64(60))}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdateService(
		ctx,
		&servicediscovery.UpdateServiceInput{
			Id: created.Service.Id,
			Service: &types.ServiceChange{
				DnsConfig: &types.DnsConfigChange{
					DnsRecords: []types.DnsRecord{{Type: types.RecordTypeA, TTL: new(int64(120))}},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetService(ctx, &servicediscovery.GetServiceInput{Id: created.Service.Id})
	if err != nil || aws.ToInt64(got.Service.DnsConfig.DnsRecords[0].TTL) != 120 {
		t.Fatalf("DNS config records %v %+v", err, got)
	}
	_, err = c.UpdateService(
		ctx,
		&servicediscovery.UpdateServiceInput{
			Id: created.Service.Id,
			Service: &types.ServiceChange{
				DnsConfig: &types.DnsConfigChange{
					DnsRecords: []types.DnsRecord{{Type: types.RecordTypeAaaa, TTL: new(int64(120))}},
				},
			},
		},
	)
	code(t, err, "InvalidInput")
	_, err = c.RegisterInstance(
		ctx,
		&servicediscovery.RegisterInstanceInput{
			ServiceId:  created.Service.Id,
			InstanceId: new("one"),
			Attributes: map[string]string{"AWS_INSTANCE_IPV4": "127.0.0.1"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.URL,
		strings.NewReader(`{"NamespaceName":"dns.local","ServiceName":"dns"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "data-servicediscovery.us-east-1.amazonaws.com"
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Route53AutoNaming_v20170314.DiscoverInstances")
	response, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"InstanceId":"one"`) {
		t.Fatalf("data-servicediscovery wire routing: %v %d %s", err, response.StatusCode, body)
	}
	_, err = c.ListNamespaces(
		ctx,
		&servicediscovery.ListNamespacesInput{
			Filters: []types.NamespaceFilter{
				{
					Name:      types.NamespaceFilterNameType,
					Values:    []string{"HTTP"},
					Condition: types.FilterCondition("UNKNOWN"),
				},
			},
		},
	)
	code(t, err, "NotImplemented")
}

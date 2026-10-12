package redshift_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/redshift"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func createInput(id string) *api.CreateClusterMessage {
	return &api.CreateClusterMessage{
		ClusterIdentifier: new(id),
		NodeType:          new("dc2.large"),
		ClusterType:       new("single-node"),
		MasterUsername: new(
			"admin",
		),
		MasterUserPassword: new("Password123!"),
		Tags:               []api.Tag{{Key: new("env"), Value: new("test")}},
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestClusterLifecycleAtomicityAndScope(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	store := memory.New(nil, nil)
	s := redshift.New(store, ec2.New(store))
	in := createInput("warehouse")
	created, err := s.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	c := created.Cluster
	if *c.ClusterStatus != "available" || *c.DBName != "dev" || *c.Endpoint.Port != 5439 ||
		!strings.HasSuffix(*c.Endpoint.Address, ".us-east-1.redshift.amazonaws.com") ||
		*c.ClusterParameterGroups[0].ParameterGroupName != "default.redshift-1.0" || !*c.Encrypted {
		t.Fatal(c)
	}

	in.NodeType = new("invalid")
	*created.Cluster.MasterUsername = "mutated"
	described, err := s.DescribeClusters(ctx, &api.DescribeClustersMessage{ClusterIdentifier: new("warehouse")})
	if err != nil || *described.Clusters[0].MasterUsername != "admin" {
		t.Fatal(described, err)
	}

	_, err = s.CreateCluster(ctx, createInput("warehouse"))
	wantCode(t, err, "ClusterAlreadyExists")
	_, err = s.ModifyCluster(
		ctx,
		&api.ModifyClusterMessage{
			ClusterIdentifier:  new("warehouse"),
			PubliclyAccessible: new(true),
			Port:               new(int64(1)),
		},
	)
	wantCode(t, err, "ValidationException")
	after, _ := s.DescribeClusters(ctx, &api.DescribeClustersMessage{ClusterIdentifier: new("warehouse")})
	if !reflect.DeepEqual(described, after) {
		t.Fatal("rejected update changed record")
	}

	_, err = s.ModifyCluster(
		ctx,
		&api.ModifyClusterMessage{
			ClusterIdentifier:         new("warehouse"),
			ClusterParameterGroupName: new("missing"),
			Port:                      new(int64(5440)),
		},
	)
	wantCode(t, err, "ClusterParameterGroupNotFound")
	after, _ = s.DescribeClusters(ctx, &api.DescribeClustersMessage{ClusterIdentifier: new("warehouse")})
	if !reflect.DeepEqual(described, after) {
		t.Fatal("missing reference changed record")
	}

	modified, err := s.ModifyCluster(
		ctx,
		&api.ModifyClusterMessage{
			ClusterIdentifier:  new("warehouse"),
			Port:               new(int64(5440)),
			MasterUserPassword: new("Changed123!"),
		},
	)
	if err != nil || *modified.Cluster.Endpoint.Port != 5440 {
		t.Fatal(modified, err)
	}

	for _, other := range []context.Context{
		testContext("999999999999", "us-east-1"),
		testContext("123456789012", "us-west-2"),
	} {
		_, err = s.DescribeClusters(other, &api.DescribeClustersMessage{ClusterIdentifier: new("warehouse")})
		wantCode(t, err, "ClusterNotFound")
	}

	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 1 || resources[0].Details.(map[string]any)["RecordsOnly"] != true {
		t.Fatal(resources, err)
	}

	resource := resources[0].ARN
	_, err = s.CreateTags(
		ctx,
		&api.CreateTagsMessage{ResourceName: new(resource), Tags: []api.Tag{{Key: new("owner"), Value: new("me")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := s.DescribeTags(ctx, &api.DescribeTagsMessage{ResourceName: new(resource)})
	if err != nil || len(tags.TaggedResources) != 2 {
		t.Fatal(tags, err)
	}

	_, err = s.DeleteTags(ctx, &api.DeleteTagsMessage{ResourceName: new(resource), TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteCluster(
		ctx,
		&api.DeleteClusterMessage{ClusterIdentifier: new("warehouse"), FinalClusterSnapshotIdentifier: new("final")},
	)
	wantCode(t, err, "NotImplemented")
	_, err = s.DeleteCluster(ctx, &api.DeleteClusterMessage{ClusterIdentifier: new("warehouse")})
	wantCode(t, err, "ValidationException")
	store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			raw, _ := tx.Get(key)
			if strings.Contains(string(raw), "Password123!") || strings.Contains(string(raw), "Changed123!") {
				t.Fatal("plaintext password stored")
			}
		}

		return nil
	})
	_, err = s.DeleteCluster(
		ctx,
		&api.DeleteClusterMessage{ClusterIdentifier: new("warehouse"), SkipFinalClusterSnapshot: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DescribeClusters(ctx, &api.DescribeClustersMessage{ClusterIdentifier: new("warehouse")})
	wantCode(t, err, "ClusterNotFound")
}

func TestGroupsAndReferences(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	store := memory.New(nil, nil)
	network := ec2.New(store)
	s := redshift.New(store, network)
	vpc, err := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	sn, err := network.CreateSubnet(
		ctx,
		&ec2api.CreateSubnetRequest{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.0.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	input := &api.CreateClusterSubnetGroupMessage{
		ClusterSubnetGroupName: new("SUBNETS"),
		Description:            new("original"),
		SubnetIds:              []string{*sn.Subnet.SubnetId},
	}

	group, err := s.CreateClusterSubnetGroup(ctx, input)
	if err != nil || *group.ClusterSubnetGroup.ClusterSubnetGroupName != "subnets" {
		t.Fatal(group, err)
	}

	_, err = s.CreateClusterSubnetGroup(ctx, input)
	wantCode(t, err, "ClusterSubnetGroupAlreadyExists")
	_, err = s.ModifyClusterSubnetGroup(
		ctx,
		&api.ModifyClusterSubnetGroupMessage{
			ClusterSubnetGroupName: new("subnets"),
			Description:            new("changed"),
			SubnetIds:              []string{"subnet-deadbeef"},
		},
	)
	wantCode(t, err, "InvalidSubnet")
	readGroup, err := s.DescribeClusterSubnetGroups(
		ctx,
		&api.DescribeClusterSubnetGroupsMessage{ClusterSubnetGroupName: new("subnets")},
	)
	if err != nil || *readGroup.ClusterSubnetGroups[0].Description != "original" {
		t.Fatal(readGroup, err)
	}

	_, err = s.ModifyClusterSubnetGroup(
		ctx,
		&api.ModifyClusterSubnetGroupMessage{
			ClusterSubnetGroupName: new("subnets"),
			Description:            new("changed"),
			SubnetIds:              input.SubnetIds,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	pgIn := &api.CreateClusterParameterGroupMessage{
		ParameterGroupName:   new("PARAMS"),
		ParameterGroupFamily: new("redshift-1.0"),
		Description:          new("parameters"),
	}

	_, err = s.CreateClusterParameterGroup(ctx, pgIn)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateClusterParameterGroup(ctx, pgIn)
	wantCode(t, err, "ClusterParameterGroupAlreadyExists")
	defaults, err := s.DescribeClusterParameterGroups(
		ctx,
		&api.DescribeClusterParameterGroupsMessage{ParameterGroupName: new("default.redshift-1.0")},
	)
	if err != nil {
		t.Fatal(err)
	}

	again, _ := s.DescribeClusterParameterGroups(
		ctx,
		&api.DescribeClusterParameterGroupsMessage{ParameterGroupName: new("default.redshift-1.0")},
	)
	if !reflect.DeepEqual(defaults, again) {
		t.Fatal("default group is unstable")
	}

	in := createInput("warehouse")
	in.ClusterSubnetGroupName, in.ClusterParameterGroupName = new("subnets"), new("params")
	_, err = s.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteClusterSubnetGroup(
		ctx,
		&api.DeleteClusterSubnetGroupMessage{ClusterSubnetGroupName: new("subnets")},
	)
	wantCode(t, err, "InvalidClusterSubnetGroupStateFault")
	_, err = s.DeleteClusterParameterGroup(
		ctx,
		&api.DeleteClusterParameterGroupMessage{ParameterGroupName: new("params")},
	)
	wantCode(t, err, "InvalidClusterParameterGroupState")
	_, err = s.DeleteClusterParameterGroup(
		ctx,
		&api.DeleteClusterParameterGroupMessage{ParameterGroupName: new("default.redshift-1.0")},
	)
	wantCode(t, err, "InvalidClusterParameterGroupState")
	_, err = s.DeleteCluster(
		ctx,
		&api.DeleteClusterMessage{ClusterIdentifier: new("warehouse"), SkipFinalClusterSnapshot: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteClusterSubnetGroup(
		ctx,
		&api.DeleteClusterSubnetGroupMessage{ClusterSubnetGroupName: new("subnets")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteClusterParameterGroup(
		ctx,
		&api.DeleteClusterParameterGroupMessage{ParameterGroupName: new("params")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DescribeClusterSubnetGroups(
		ctx,
		&api.DescribeClusterSubnetGroupsMessage{ClusterSubnetGroupName: new("subnets")},
	)
	wantCode(t, err, "ClusterSubnetGroupNotFoundFault")
	_, err = s.DescribeClusterParameterGroups(
		ctx,
		&api.DescribeClusterParameterGroupsMessage{ParameterGroupName: new("params")},
	)
	wantCode(t, err, "ClusterParameterGroupNotFound")
}

func TestPaginationAndValidation(t *testing.T) {
	ctx := testContext("123456789012", "us-east-1")
	s := redshift.New(memory.New(nil, nil), nil)
	for i := range 21 {
		_, err := s.CreateCluster(ctx, createInput(fmt.Sprintf("cluster-%02d", i)))
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.DescribeClusters(ctx, &api.DescribeClustersMessage{MaxRecords: new(int64(20))})
	if err != nil || len(page.Clusters) != 20 || page.Marker == nil {
		t.Fatal(page, err)
	}

	_, err = s.DeleteCluster(
		ctx,
		&api.DeleteClusterMessage{
			ClusterIdentifier:        page.Clusters[19].ClusterIdentifier,
			SkipFinalClusterSnapshot: new(true),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	next, err := s.DescribeClusters(ctx, &api.DescribeClustersMessage{MaxRecords: new(int64(20)), Marker: page.Marker})
	if err != nil || len(next.Clusters) != 1 || *next.Clusters[0].ClusterIdentifier != "cluster-20" {
		t.Fatal(next, err)
	}

	for _, mutate := range []func(*api.CreateClusterMessage){
		func(in *api.CreateClusterMessage) { in.MasterUserPassword = new("short") },
		func(in *api.CreateClusterMessage) { in.ClusterIdentifier = new("bad--name") },
		func(in *api.CreateClusterMessage) { in.MasterUsername = new("select") },
		func(in *api.CreateClusterMessage) { in.DBName = new("select") },
		func(in *api.CreateClusterMessage) { in.NumberOfNodes = new(int64(2)) },
		func(in *api.CreateClusterMessage) { in.NodeType = new("ra3.xlplus"); in.Port = new(int64(6000)) },
	} {
		in := createInput("invalid")
		mutate(in)
		_, err = s.CreateCluster(ctx, in)
		wantCode(t, err, "ValidationException")
	}

	_, err = s.DescribeClusters(ctx, &api.DescribeClustersMessage{Marker: new("!!!")})
	wantCode(t, err, "ValidationException")
	// The generated descriptor must redact both password-bearing operations.
	d := api.Descriptor(s)
	if !d.Operations["CreateCluster"].Sensitive || !d.Operations["ModifyCluster"].Sensitive {
		t.Fatal("password diagnostics are not redacted")
	}
}

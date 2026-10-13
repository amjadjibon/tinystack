package memorydb_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/memorydb"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

const password = "correct-horse-battery"

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

// must returns v, or fails the test with err.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

type fixture struct {
	*memorydb.Service
	network *ec2.Service
	ctx     context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := memory.New(nil, nil)
	network := ec2.New(store)
	return &fixture{
		Service: memorydb.New(store, network),
		network: network,
		ctx:     testContext("123456789012", "us-east-1"),
	}
}

func (f *fixture) subnet(t *testing.T, cidr string) string {
	t.Helper()
	vpc := must(f.network.CreateVpc(f.ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.0.0.0/16")}))
	sn := must(f.network.CreateSubnet(f.ctx, &ec2api.CreateSubnetRequest{VpcId: vpc.Vpc.VpcId, CidrBlock: &cidr}))
	return *sn.Subnet.SubnetId
}

func (f *fixture) cluster(t *testing.T, name string, mutate ...func(*api.CreateClusterRequest)) *api.Cluster {
	t.Helper()
	in := &api.CreateClusterRequest{ClusterName: &name, NodeType: new("db.r6g.large"), ACLName: new("open-access")}
	for _, m := range mutate {
		m(in)
	}

	return must(f.CreateCluster(f.ctx, in)).Cluster
}

func TestClusterLifecycle(t *testing.T) {
	f := newFixture(t)
	c := f.cluster(t, "Orders", func(in *api.CreateClusterRequest) {
		in.NumShards, in.NumReplicasPerShard = new(int64(3)), new(int64(2))
		in.Tags = []api.Tag{{Key: new("env"), Value: new("test")}}
	})

	if *c.Name != "orders" || *c.Status != "available" || *c.NumberOfShards != 3 || *c.Engine != "valkey" ||
		*c.EngineVersion != "7.2" || *c.EnginePatchVersion != "7.2.6" ||
		*c.ParameterGroupName != "default.memorydb-valkey7" ||
		!*c.TLSEnabled || *c.ClusterEndpoint.Port != 6379 || *c.AvailabilityMode != "multiaz" ||
		*c.ARN != "arn:aws:memorydb:us-east-1:123456789012:cluster/orders" ||
		!strings.HasPrefix(*c.ClusterEndpoint.Address, "clustercfg.orders.") ||
		!strings.HasSuffix(*c.ClusterEndpoint.Address, ".memorydb.us-east-1.amazonaws.com") || c.Shards != nil {
		t.Fatal(c)
	}

	_, err := f.CreateCluster(f.ctx, &api.CreateClusterRequest{
		ClusterName: new("orders"), NodeType: new("db.r6g.large"), ACLName: new("open-access"),
	})
	wantCode(t, err, "ClusterAlreadyExistsFault")

	described := must(f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{
		ClusterName: new("ORDERS"), ShowShardDetails: new(true),
	}))
	got := described.Clusters[0]
	if *got.ClusterEndpoint.Address != *c.ClusterEndpoint.Address || len(got.Shards) != 3 {
		t.Fatal(got)
	}

	slots := 0
	for _, sh := range got.Shards {
		if len(sh.Nodes) != 3 || *sh.NumberOfNodes != 3 || *sh.Nodes[0].Name != "0001" {
			t.Fatal(sh)
		}

		var from, to int
		if _, scanErr := fmt.Sscanf(*sh.Slots, "%d-%d", &from, &to); scanErr != nil || from != slots {
			t.Fatal(*sh.Slots, scanErr)
		}

		slots = to + 1
	}

	if slots != 16384 || *got.Shards[0].Name != "0001" || *got.Shards[2].Name != "0003" {
		t.Fatal(slots, got.Shards)
	}

	deleted := must(f.DeleteCluster(f.ctx, &api.DeleteClusterRequest{ClusterName: new("orders")}))
	if *deleted.Cluster.Status != "deleting" {
		t.Fatal(deleted)
	}

	_, err = f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{ClusterName: new("orders")})
	wantCode(t, err, "ClusterNotFoundFault")
	_, err = f.DeleteCluster(f.ctx, &api.DeleteClusterRequest{ClusterName: new("orders")})
	wantCode(t, err, "ClusterNotFoundFault")
}

func TestClusterValidation(t *testing.T) {
	f := newFixture(t)
	const (
		badValue = "InvalidParameterValueException"
		badMix   = "InvalidParameterCombinationException"
	)

	type change = func(*api.CreateClusterRequest)
	named := func(n string) change { return func(in *api.CreateClusterRequest) { in.ClusterName = &n } }
	window := func(n string) change { return func(in *api.CreateClusterRequest) { in.MaintenanceWindow = &n } }
	for name, tc := range map[string]struct {
		mutate change
		code   string
	}{
		"consecutive hyphens": {named("a--b"), badValue},
		"trailing hyphen":     {named("ab-"), badValue},
		"digit first":         {named("1ab"), badValue},
		"too long":            {named(strings.Repeat("a", 41)), badValue},
		"no node type":        {func(in *api.CreateClusterRequest) { in.NodeType = nil }, badValue},
		"no ACL":              {func(in *api.CreateClusterRequest) { in.ACLName = nil }, badValue},
		"missing ACL":         {func(in *api.CreateClusterRequest) { in.ACLName = new("nope") }, "ACLNotFoundFault"},
		"missing subnets": {
			func(in *api.CreateClusterRequest) { in.SubnetGroupName = new("nope") }, "SubnetGroupNotFoundFault",
		},
		"shards":   {func(in *api.CreateClusterRequest) { in.NumShards = new(int64(501)) }, badValue},
		"replicas": {func(in *api.CreateClusterRequest) { in.NumReplicasPerShard = new(int64(6)) }, badValue},
		"engine":   {func(in *api.CreateClusterRequest) { in.Engine = new("memcached") }, badValue},
		"patch version": {
			func(in *api.CreateClusterRequest) { in.EngineVersion = new("7.2.6") }, badValue,
		},
		"mismatched group": {
			func(in *api.CreateClusterRequest) { in.ParameterGroupName = new("default.memorydb-redis7") }, badMix,
		},
		"unknown group": {
			func(in *api.CreateClusterRequest) { in.ParameterGroupName = new("custom") }, "ParameterGroupNotFoundFault",
		},
		"maintenance too short": {window("sun:23:00-sun:23:30"), badValue},
		"maintenance format":    {window("sun:23:00"), badValue},
		"snapshot window": {
			func(in *api.CreateClusterRequest) { in.SnapshotWindow = new("25:00-26:00") }, badValue,
		},
		"no TLS needs open access": {
			func(in *api.CreateClusterRequest) {
				in.TLSEnabled = new(false)
				in.ACLName = new("other")
			}, badMix,
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := &api.CreateClusterRequest{
				ClusterName: new("valid"), NodeType: new("db.r6g.large"), ACLName: new("open-access"),
			}
			tc.mutate(in)
			_, err := f.CreateCluster(f.ctx, in)
			wantCode(t, err, tc.code)
		})
	}

	// A cluster without TLS can only use the open-access ACL.
	must(f.CreateUser(f.ctx, &api.CreateUserRequest{
		UserName: new("alice"), AccessString: new("on ~* +@all"),
		AuthenticationMode: &api.AuthenticationMode{Type: new("password"), Passwords: []string{password}},
	}))
	must(f.CreateACL(f.ctx, &api.CreateACLRequest{ACLName: new("team"), UserNames: []string{"alice"}}))
	_, err := f.CreateCluster(f.ctx, &api.CreateClusterRequest{
		ClusterName: new("plain"), NodeType: new("db.r6g.large"), ACLName: new("team"), TLSEnabled: new(false),
	})
	wantCode(t, err, "InvalidParameterCombinationException")

	// Nothing from the rejected requests was stored.
	listed := must(f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{}))
	if len(listed.Clusters) != 0 {
		t.Fatal(listed)
	}

	open := f.cluster(t, "open", func(in *api.CreateClusterRequest) { in.TLSEnabled = new(false) })
	if *open.TLSEnabled {
		t.Fatal(open)
	}
}

func TestClusterUpdateIsAtomic(t *testing.T) {
	f := newFixture(t)
	original := f.cluster(t, "cache", func(in *api.CreateClusterRequest) {
		in.Engine, in.EngineVersion = new("redis"), new("7.0")
	})

	updated := must(f.UpdateCluster(f.ctx, &api.UpdateClusterRequest{
		ClusterName:          new("cache"),
		Description:          new("hot"),
		NodeType:             new("db.r6g.xlarge"),
		ShardConfiguration:   &api.ShardConfigurationRequest{ShardCount: new(int64(4))},
		ReplicaConfiguration: &api.ReplicaConfigurationRequest{ReplicaCount: new(int64(0))},
		SecurityGroupIds:     []string{"sg-1"},
	})).Cluster
	if *updated.Description != "hot" || *updated.NumberOfShards != 4 || *updated.AvailabilityMode != "singleaz" ||
		*updated.NodeType != "db.r6g.xlarge" || len(updated.SecurityGroups) != 1 ||
		*updated.ClusterEndpoint.Address != *original.ClusterEndpoint.Address {
		t.Fatal(updated)
	}

	// Upgrading moves to the new version's default parameter group.
	upgraded := must(f.UpdateCluster(f.ctx, &api.UpdateClusterRequest{
		ClusterName: new("cache"), EngineVersion: new("7.1"),
	})).Cluster
	if *upgraded.EngineVersion != "7.1" || *upgraded.ParameterGroupName != "default.memorydb-redis7" {
		t.Fatal(upgraded)
	}

	for name, in := range map[string]*api.UpdateClusterRequest{
		"downgrade":     {ClusterName: new("cache"), EngineVersion: new("6.2")},
		"valkey to old": {ClusterName: new("cache"), Engine: new("valkey"), EngineVersion: new("6.2")},
		"shards": {
			ClusterName: new("cache"), Description: new("changed"),
			ShardConfiguration: &api.ShardConfigurationRequest{ShardCount: new(int64(0))},
		},
		"ACL":    {ClusterName: new("cache"), Description: new("changed"), ACLName: new("nope")},
		"window": {ClusterName: new("cache"), Description: new("changed"), SnapshotWindow: new("nonsense")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.UpdateCluster(f.ctx, in); err == nil {
				t.Fatal("expected an error")
			}

			got := must(f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{ClusterName: new("cache")}))
			if c := got.Clusters[0]; *c.Description != "hot" || *c.EngineVersion != "7.1" || *c.NumberOfShards != 4 {
				t.Fatalf("a rejected update changed the cluster: %v", c)
			}
		})
	}

	// Switching from Redis OSS to Valkey is an upgrade; a non-TLS cluster cannot move to a custom ACL.
	valkey := must(
		f.UpdateCluster(f.ctx, &api.UpdateClusterRequest{ClusterName: new("cache"), Engine: new("valkey")}),
	).Cluster
	if *valkey.Engine != "valkey" || *valkey.EngineVersion != "7.2" ||
		*valkey.ParameterGroupName != "default.memorydb-valkey7" {
		t.Fatal(valkey)
	}

	_, err := f.UpdateCluster(f.ctx, &api.UpdateClusterRequest{ClusterName: new("missing"), Description: new("x")})
	wantCode(t, err, "ClusterNotFoundFault")
}

func TestSubnetGroupsAndClusters(t *testing.T) {
	f := newFixture(t)
	first := f.subnet(t, "10.0.1.0/24")
	_, err := f.CreateSubnetGroup(
		f.ctx,
		&api.CreateSubnetGroupRequest{SubnetGroupName: new("net"), SubnetIds: []string{"subnet-nope"}},
	)
	wantCode(t, err, "InvalidSubnet")

	g := must(f.CreateSubnetGroup(f.ctx, &api.CreateSubnetGroupRequest{
		SubnetGroupName: new("Net"), Description: new("private"), SubnetIds: []string{first},
		Tags: []api.Tag{{Key: new("a"), Value: new("b")}},
	})).SubnetGroup
	if *g.Name != "net" || *g.VpcId == "" || len(g.Subnets) != 1 || *g.Subnets[0].Identifier != first ||
		*g.ARN != "arn:aws:memorydb:us-east-1:123456789012:subnetgroup/net" {
		t.Fatal(g)
	}

	_, err = f.CreateSubnetGroup(
		f.ctx,
		&api.CreateSubnetGroupRequest{SubnetGroupName: new("net"), SubnetIds: []string{first}},
	)
	wantCode(t, err, "SubnetGroupAlreadyExistsFault")

	f.cluster(t, "inside", func(in *api.CreateClusterRequest) { in.SubnetGroupName = new("net") })
	_, err = f.DeleteSubnetGroup(f.ctx, &api.DeleteSubnetGroupRequest{SubnetGroupName: new("net")})
	wantCode(t, err, "SubnetGroupInUseFault")

	updated := must(f.UpdateSubnetGroup(f.ctx, &api.UpdateSubnetGroupRequest{
		SubnetGroupName: new("net"), Description: new("changed"),
	})).SubnetGroup
	if *updated.Description != "changed" || len(updated.Subnets) != 1 {
		t.Fatal(updated)
	}

	_, err = f.UpdateSubnetGroup(
		f.ctx,
		&api.UpdateSubnetGroupRequest{SubnetGroupName: new("none"), Description: new("x")},
	)
	wantCode(t, err, "SubnetGroupNotFoundFault")

	must(f.DeleteCluster(f.ctx, &api.DeleteClusterRequest{ClusterName: new("inside")}))
	must(f.DeleteSubnetGroup(f.ctx, &api.DeleteSubnetGroupRequest{SubnetGroupName: new("net")}))
	_, err = f.DescribeSubnetGroups(f.ctx, &api.DescribeSubnetGroupsRequest{SubnetGroupName: new("net")})
	wantCode(t, err, "SubnetGroupNotFoundFault")
}

func TestUsersAndACLs(t *testing.T) {
	f := newFixture(t)
	users := must(f.DescribeUsers(f.ctx, &api.DescribeUsersRequest{}))
	def := users.Users[0]
	if len(users.Users) != 1 || *def.Name != "default" || *def.AccessString != "on ~* &* +@all" ||
		*def.Authentication.Type != "no-password" || len(def.ACLNames) != 1 || def.ACLNames[0] != "open-access" {
		t.Fatal(users)
	}

	for name, in := range map[string]*api.AuthenticationMode{
		"short":       {Type: new("password"), Passwords: []string{"short"}},
		"forbidden":   {Type: new("password"), Passwords: []string{"abcdefghijklmnop@"}},
		"three":       {Type: new("password"), Passwords: []string{password, password + "1", password + "2"}},
		"iam with pw": {Type: new("iam"), Passwords: []string{password}},
		"type":        {Type: new("kerberos"), Passwords: []string{password}},
	} {
		_, err := f.CreateUser(f.ctx, &api.CreateUserRequest{
			UserName: new("bob"), AccessString: new("on ~* +@all"), AuthenticationMode: in,
		})
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}

	_, err := f.CreateUser(f.ctx, &api.CreateUserRequest{
		UserName: new(
			"default",
		),
		AccessString:       new("on"),
		AuthenticationMode: &api.AuthenticationMode{Type: new("iam")},
	})
	wantCode(t, err, "UserAlreadyExistsFault")

	_, err = f.CreateUser(f.ctx, &api.CreateUserRequest{
		UserName: new(
			"bob",
		),
		AccessString:       new("on reset"),
		AuthenticationMode: &api.AuthenticationMode{Type: new("iam")},
	})
	wantCode(t, err, "InvalidParameterValueException")

	alice := must(f.CreateUser(f.ctx, &api.CreateUserRequest{
		UserName:     new("Alice"),
		AccessString: new("on ~* +@read"),
		AuthenticationMode: &api.AuthenticationMode{
			Type:      new("password"),
			Passwords: []string{password, password + "2"},
		},
	})).User
	if *alice.Name != "alice" || *alice.Authentication.PasswordCount != 2 || *alice.Status != "active" {
		t.Fatal(alice)
	}

	must(f.CreateUser(f.ctx, &api.CreateUserRequest{
		UserName: new(
			"svc",
		),
		AccessString:       new("on ~* +@all"),
		AuthenticationMode: &api.AuthenticationMode{Type: new("iam")},
	}))

	updated := must(f.UpdateUser(f.ctx, &api.UpdateUserRequest{
		UserName: new("alice"), AccessString: new("on ~app::* +@write"),
		AuthenticationMode: &api.AuthenticationMode{Type: new("password"), Passwords: []string{password}},
	})).User
	if *updated.AccessString != "on ~app::* +@write" || *updated.Authentication.PasswordCount != 1 {
		t.Fatal(updated)
	}

	_, err = f.UpdateUser(f.ctx, &api.UpdateUserRequest{UserName: new("default"), AccessString: new("on")})
	wantCode(t, err, "InvalidUserStateFault")
	_, err = f.DeleteUser(f.ctx, &api.DeleteUserRequest{UserName: new("default")})
	wantCode(t, err, "InvalidUserStateFault")
	_, err = f.UpdateUser(f.ctx, &api.UpdateUserRequest{UserName: new("ghost"), AccessString: new("on")})
	wantCode(t, err, "UserNotFoundFault")

	// Filters, a single user and pages.
	byFilter := must(f.DescribeUsers(f.ctx, &api.DescribeUsersRequest{
		Filters: []api.Filter{{Name: new("UserName"), Values: []string{"svc", "default"}}},
	}))
	if len(byFilter.Users) != 2 || *byFilter.Users[0].Name != "default" {
		t.Fatal(byFilter)
	}

	_, err = f.DescribeUsers(
		f.ctx,
		&api.DescribeUsersRequest{Filters: []api.Filter{{Name: new("Other"), Values: []string{"x"}}}},
	)
	wantCode(t, err, "InvalidParameterValueException")
	_, err = f.DescribeUsers(
		f.ctx,
		&api.DescribeUsersRequest{
			UserName: new("alice"),
			Filters:  []api.Filter{{Name: new("UserName"), Values: []string{"x"}}},
		},
	)
	wantCode(t, err, "InvalidParameterCombinationException")

	var seen []string
	var token *string
	for {
		page := must(f.DescribeUsers(f.ctx, &api.DescribeUsersRequest{MaxResults: new(int64(1)), NextToken: token}))
		for _, u := range page.Users {
			seen = append(seen, *u.Name)
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if !slices.Equal(seen, []string{"alice", "default", "svc"}) {
		t.Fatal(seen)
	}

	// ACLs.
	acls := must(f.DescribeACLs(f.ctx, &api.DescribeACLsRequest{}))
	if len(acls.ACLs) != 1 || *acls.ACLs[0].Name != "open-access" || acls.ACLs[0].UserNames[0] != "default" {
		t.Fatal(acls)
	}

	_, err = f.CreateACL(f.ctx, &api.CreateACLRequest{ACLName: new("team"), UserNames: []string{"ghost"}})
	wantCode(t, err, "UserNotFoundFault")
	_, err = f.CreateACL(f.ctx, &api.CreateACLRequest{ACLName: new("team"), UserNames: []string{"alice", "ALICE"}})
	wantCode(t, err, "DuplicateUserNameFault")
	_, err = f.CreateACL(f.ctx, &api.CreateACLRequest{ACLName: new("team"), UserNames: []string{"default"}})
	wantCode(t, err, "InvalidParameterValueException")
	_, err = f.CreateACL(f.ctx, &api.CreateACLRequest{ACLName: new("open-access")})
	wantCode(t, err, "ACLAlreadyExistsFault")

	team := must(f.CreateACL(f.ctx, &api.CreateACLRequest{ACLName: new("team"), UserNames: []string{"alice"}})).ACL
	if *team.Name != "team" || !slices.Equal(team.UserNames, []string{"alice"}) {
		t.Fatal(team)
	}

	team = must(f.UpdateACL(f.ctx, &api.UpdateACLRequest{
		ACLName: new("team"), UserNamesToAdd: []string{"svc"}, UserNamesToRemove: []string{"alice"},
	})).ACL
	if !slices.Equal(team.UserNames, []string{"svc"}) {
		t.Fatal(team)
	}

	_, err = f.UpdateACL(f.ctx, &api.UpdateACLRequest{ACLName: new("team"), UserNamesToRemove: []string{"alice"}})
	wantCode(t, err, "InvalidParameterValueException")
	_, err = f.UpdateACL(f.ctx, &api.UpdateACLRequest{ACLName: new("open-access"), UserNamesToAdd: []string{"svc"}})
	wantCode(t, err, "InvalidACLStateFault")
	_, err = f.DeleteACL(f.ctx, &api.DeleteACLRequest{ACLName: new("open-access")})
	wantCode(t, err, "InvalidACLStateFault")

	// An ACL in use by a cluster cannot be deleted; deleting a user removes it from its ACLs.
	f.cluster(t, "guarded", func(in *api.CreateClusterRequest) { in.ACLName = new("team") })
	_, err = f.DeleteACL(f.ctx, &api.DeleteACLRequest{ACLName: new("team")})
	wantCode(t, err, "InvalidACLStateFault")
	described := must(f.DescribeACLs(f.ctx, &api.DescribeACLsRequest{ACLName: new("team")})).ACLs[0]
	if !slices.Equal(described.Clusters, []string{"guarded"}) {
		t.Fatal(described)
	}

	removed := must(f.DeleteUser(f.ctx, &api.DeleteUserRequest{UserName: new("svc")})).User
	if *removed.Status != "deleting" || !slices.Equal(removed.ACLNames, []string{"team"}) {
		t.Fatal(removed)
	}

	described = must(f.DescribeACLs(f.ctx, &api.DescribeACLsRequest{ACLName: new("team")})).ACLs[0]
	if len(described.UserNames) != 0 {
		t.Fatal(described)
	}

	must(f.DeleteCluster(f.ctx, &api.DeleteClusterRequest{ClusterName: new("guarded")}))
	gone := must(f.DeleteACL(f.ctx, &api.DeleteACLRequest{ACLName: new("team")}))
	if *gone.ACL.Status != "deleting" {
		t.Fatal(gone)
	}
}

func TestTags(t *testing.T) {
	f := newFixture(t)
	c := f.cluster(t, "tagged", func(in *api.CreateClusterRequest) {
		in.Tags = []api.Tag{{Key: new("a"), Value: new("1")}}
	})

	got := must(f.TagResource(f.ctx, &api.TagResourceRequest{
		ResourceArn: c.ARN, Tags: []api.Tag{{Key: new("b"), Value: new("2")}},
	}))
	if len(got.TagList) != 2 {
		t.Fatal(got)
	}

	listed := must(f.ListTags(f.ctx, &api.ListTagsRequest{ResourceArn: c.ARN}))
	if len(listed.TagList) != 2 || *listed.TagList[0].Key != "a" {
		t.Fatal(listed)
	}

	_, err := f.UntagResource(f.ctx, &api.UntagResourceRequest{ResourceArn: c.ARN, TagKeys: []string{"a", "missing"}})
	wantCode(t, err, "TagNotFoundFault")
	left := must(f.UntagResource(f.ctx, &api.UntagResourceRequest{ResourceArn: c.ARN, TagKeys: []string{"a"}}))
	if len(left.TagList) != 1 || *left.TagList[0].Key != "b" {
		t.Fatal(left)
	}

	_, err = f.TagResource(f.ctx, &api.TagResourceRequest{
		ResourceArn: c.ARN, Tags: []api.Tag{{Key: new("aws:reserved"), Value: new("x")}},
	})
	wantCode(t, err, "InvalidParameterValueException")

	_, err = f.ListTags(
		f.ctx,
		&api.ListTagsRequest{ResourceArn: new("arn:aws:memorydb:us-east-1:123456789012:cluster/none")},
	)
	wantCode(t, err, "ClusterNotFoundFault")
	_, err = f.ListTags(f.ctx, &api.ListTagsRequest{ResourceArn: new("not-an-arn")})
	wantCode(t, err, "InvalidARNFault")
	_, err = f.TagResource(f.ctx, &api.TagResourceRequest{
		ResourceArn: new(
			"arn:aws:memorydb:us-east-1:123456789012:acl/open-access",
		),
		Tags: []api.Tag{{Key: new("a"), Value: new("b")}},
	})
	wantCode(t, err, "InvalidParameterValueException")

	empty := must(
		f.ListTags(
			f.ctx,
			&api.ListTagsRequest{ResourceArn: new("arn:aws:memorydb:us-east-1:123456789012:user/default")},
		),
	)
	if len(empty.TagList) != 0 {
		t.Fatal(empty)
	}
}

func TestScopeIsolationAndPagination(t *testing.T) {
	f := newFixture(t)
	other := testContext("999999999999", "us-east-1")
	elsewhere := testContext("123456789012", "eu-west-1")
	for _, name := range []string{"a1", "a2", "a3"} {
		f.cluster(t, name)
	}

	for _, ctx := range []context.Context{other, elsewhere} {
		listed := must(f.DescribeClusters(ctx, &api.DescribeClustersRequest{}))
		if len(listed.Clusters) != 0 {
			t.Fatal(listed)
		}

		_, err := f.DescribeClusters(ctx, &api.DescribeClustersRequest{ClusterName: new("a1")})
		wantCode(t, err, "ClusterNotFoundFault")
	}

	// A cluster ARN of another Region does not resolve here.
	_, err := f.ListTags(
		elsewhere,
		&api.ListTagsRequest{ResourceArn: new("arn:aws:memorydb:us-east-1:123456789012:cluster/a1")},
	)
	wantCode(t, err, "ClusterNotFoundFault")

	first := must(f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{MaxResults: new(int64(2))}))
	if len(first.Clusters) != 2 || first.NextToken == nil {
		t.Fatal(first)
	}

	// The cursor survives its item being deleted.
	must(f.DeleteCluster(f.ctx, &api.DeleteClusterRequest{ClusterName: new("a2")}))
	rest := must(
		f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{MaxResults: new(int64(2)), NextToken: first.NextToken}),
	)
	if len(rest.Clusters) != 1 || *rest.Clusters[0].Name != "a3" || rest.NextToken != nil {
		t.Fatal(rest)
	}

	_, err = f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{NextToken: new("%%%")})
	wantCode(t, err, "InvalidParameterValueException")
	_, err = f.DescribeClusters(f.ctx, &api.DescribeClustersRequest{MaxResults: new(int64(0))})
	wantCode(t, err, "InvalidParameterValueException")
}

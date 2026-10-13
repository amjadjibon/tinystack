package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/memorydb"
	"github.com/aws/aws-sdk-go-v2/service/memorydb/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

const memoryDBPassword = "correct-horse-battery"

func memoryDBClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantMemoryDBError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestMemoryDBSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := memoryDBClient(s, "test", "us-east-1")
	network := ec2Client(s, "test", "us-east-1")

	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{
		VpcId: vpc.Vpc.VpcId, CidrBlock: aws.String("10.0.1.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Subnet group, user and ACL.
	group, err := c.CreateSubnetGroup(ctx, &sdk.CreateSubnetGroupInput{
		SubnetGroupName: aws.String("cache-net"), Description: aws.String("private"),
		SubnetIds: []string{aws.ToString(subnet.Subnet.SubnetId)},
		Tags:      []types.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || aws.ToString(group.SubnetGroup.VpcId) != aws.ToString(vpc.Vpc.VpcId) {
		t.Fatal(group, err)
	}

	user, err := c.CreateUser(ctx, &sdk.CreateUserInput{
		UserName: aws.String("app"), AccessString: aws.String("on ~app::* +@all"),
		AuthenticationMode: &types.AuthenticationMode{
			Type: types.InputAuthenticationTypePassword, Passwords: []string{memoryDBPassword},
		},
	})
	if err != nil || aws.ToInt32(user.User.Authentication.PasswordCount) != 1 ||
		user.User.Authentication.Type != types.AuthenticationTypePassword {
		t.Fatal(user, err)
	}

	acl, err := c.CreateACL(ctx, &sdk.CreateACLInput{ACLName: aws.String("app-acl"), UserNames: []string{"app"}})
	if err != nil || !slices.Equal(acl.ACL.UserNames, []string{"app"}) {
		t.Fatal(acl, err)
	}

	cluster, err := c.CreateCluster(ctx, &sdk.CreateClusterInput{
		ClusterName: aws.String("orders"), NodeType: aws.String("db.r6g.large"), ACLName: aws.String("app-acl"),
		SubnetGroupName: aws.String("cache-net"), NumShards: aws.Int32(2), NumReplicasPerShard: aws.Int32(1),
		Engine: aws.String("valkey"), Tags: []types.Tag{{Key: aws.String("env"), Value: aws.String("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	cl := cluster.Cluster
	if aws.ToString(cl.Status) != "available" || aws.ToInt32(cl.NumberOfShards) != 2 || !aws.ToBool(cl.TLSEnabled) ||
		cl.ClusterEndpoint.Port != 6379 ||
		!strings.HasPrefix(aws.ToString(cl.ClusterEndpoint.Address), "clustercfg.orders.") ||
		!strings.HasSuffix(aws.ToString(cl.ClusterEndpoint.Address), ".memorydb.us-east-1.amazonaws.com") ||
		aws.ToString(cl.ParameterGroupName) != "default.memorydb-valkey7" {
		t.Fatal(cl)
	}

	described, err := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{
		ClusterName: aws.String("orders"), ShowShardDetails: aws.Bool(true),
	})
	if err != nil || len(described.Clusters) != 1 || len(described.Clusters[0].Shards) != 2 ||
		len(described.Clusters[0].Shards[0].Nodes) != 2 {
		t.Fatal(described, err)
	}

	updated, err := c.UpdateCluster(ctx, &sdk.UpdateClusterInput{
		ClusterName: aws.String("orders"), Description: aws.String("hot"),
		ShardConfiguration: &types.ShardConfigurationRequest{ShardCount: 3},
	})
	if err != nil || aws.ToString(updated.Cluster.Description) != "hot" ||
		aws.ToInt32(updated.Cluster.NumberOfShards) != 3 {
		t.Fatal(updated, err)
	}

	// Modeled errors, and the unsupported final snapshot.
	_, err = c.CreateCluster(ctx, &sdk.CreateClusterInput{
		ClusterName: aws.String("orders"), NodeType: aws.String("db.r6g.large"), ACLName: aws.String("open-access"),
	})
	wantMemoryDBError(t, err, "ClusterAlreadyExistsFault")
	_, err = c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterName: aws.String("ghost")})
	wantMemoryDBError(t, err, "ClusterNotFoundFault")
	_, err = c.DeleteACL(ctx, &sdk.DeleteACLInput{ACLName: aws.String("app-acl")})
	wantMemoryDBError(t, err, "InvalidACLStateFault")
	_, err = c.DeleteSubnetGroup(ctx, &sdk.DeleteSubnetGroupInput{SubnetGroupName: aws.String("cache-net")})
	wantMemoryDBError(t, err, "SubnetGroupInUseFault")
	_, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{
		ClusterName: aws.String("orders"), FinalSnapshotName: aws.String("final"),
	})
	wantMemoryDBError(t, err, "ValidationException")

	// Built-in resources.
	users, err := c.DescribeUsers(ctx, &sdk.DescribeUsersInput{
		Filters: []types.Filter{{Name: aws.String("UserName"), Values: []string{"default"}}},
	})
	if err != nil || len(users.Users) != 1 || aws.ToString(users.Users[0].AccessString) != "on ~* &* +@all" {
		t.Fatal(users, err)
	}

	acls, err := c.DescribeACLs(ctx, &sdk.DescribeACLsInput{ACLName: aws.String("open-access")})
	if err != nil || len(acls.ACLs) != 1 || !slices.Equal(acls.ACLs[0].UserNames, []string{"default"}) {
		t.Fatal(acls, err)
	}

	_, err = c.DeleteUser(ctx, &sdk.DeleteUserInput{UserName: aws.String("default")})
	wantMemoryDBError(t, err, "InvalidUserStateFault")

	// Tags through the service and through Resource Groups Tagging API.
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: cl.ARN, Tags: []types.Tag{{Key: aws.String("owner"), Value: aws.String("me")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{
			aws.ToString(cl.ARN),
			aws.ToString(acl.ACL.ARN),
			aws.ToString(user.User.ARN),
			aws.ToString(group.SubnetGroup.ARN),
		},
		Tags: map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: cl.ARN})
	if err != nil || len(tags.TagList) != 3 {
		t.Fatal(tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: cl.ARN, TagKeys: []string{"owner", "rgta"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: cl.ARN, TagKeys: []string{"owner"}})
	wantMemoryDBError(t, err, "TagNotFoundFault")
	groupTags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: group.SubnetGroup.ARN})
	if err != nil || len(groupTags.TagList) != 2 {
		t.Fatal(groupTags, err)
	}

	// Another account sees none of it.
	other := memoryDBClient(s, "team-b", "us-east-1")
	empty, err := other.DescribeClusters(ctx, &sdk.DescribeClustersInput{})
	if err != nil || len(empty.Clusters) != 0 {
		t.Fatal(empty, err)
	}

	// Tear down in dependency order.
	deleted, err := c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterName: aws.String("orders")})
	if err != nil || aws.ToString(deleted.Cluster.Status) != "deleting" {
		t.Fatal(deleted, err)
	}

	for _, step := range []func() error{
		func() error { _, e := c.DeleteACL(ctx, &sdk.DeleteACLInput{ACLName: aws.String("app-acl")}); return e },
		func() error { _, e := c.DeleteUser(ctx, &sdk.DeleteUserInput{UserName: aws.String("app")}); return e },
		func() error {
			_, e := c.DeleteSubnetGroup(ctx, &sdk.DeleteSubnetGroupInput{SubnetGroupName: aws.String("cache-net")})
			return e
		},
	} {
		if err = step(); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DescribeUsers(ctx, &sdk.DescribeUsersInput{UserName: aws.String("app")})
	wantMemoryDBError(t, err, "UserNotFoundFault")
}

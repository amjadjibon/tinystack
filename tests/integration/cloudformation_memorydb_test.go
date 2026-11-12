package integration_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/memorydb"
)

type memoryDBStack struct {
	subnet, description, accessLevel, tag string
	members, shards                       int
	autoMinor                             bool
}

func memoryDBTemplate(s *memoryDBStack) string {
	tags := []any{map[string]any{"Key": "version", "Value": s.tag}}
	parameters := map[string]any{"maxmemory-policy": "allkeys-lru"}
	if s.tag == "one" {
		parameters["timeout"] = "30"
	}
	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"Parameters": map[string]any{"Type": "AWS::MemoryDB::ParameterGroup", "Properties": map[string]any{
				"ParameterGroupName": "cfn-parameters", "Family": "memorydb_valkey7", "Description": "custom",
				"Parameters": parameters, "Tags": tags,
			}},
			"Subnets": map[string]any{"Type": "AWS::MemoryDB::SubnetGroup", "Properties": map[string]any{
				"SubnetGroupName": "cfn-net", "Description": "private", "SubnetIds": []string{s.subnet}, "Tags": tags,
			}},
			"Reader": map[string]any{"Type": "AWS::MemoryDB::User", "Properties": map[string]any{
				"UserName": "cfn-reader", "AccessString": s.accessLevel, "Tags": tags,
				"AuthenticationMode": map[string]any{"Type": "password", "Passwords": []string{memoryDBPassword}},
			}},
			"Writer": map[string]any{"Type": "AWS::MemoryDB::User", "Properties": map[string]any{
				"UserName": "cfn-writer", "AccessString": "on ~* +@all",
				"AuthenticationMode": map[string]any{"Type": "iam"},
			}},
			"Acl": map[string]any{"Type": "AWS::MemoryDB::ACL", "Properties": map[string]any{
				"ACLName": "cfn-acl", "Tags": tags,
				"UserNames": []any{map[string]any{"Ref": "Reader"}, map[string]any{"Ref": "Writer"}}[:s.members],
			}},
			"Cache": map[string]any{"Type": "AWS::MemoryDB::Cluster", "Properties": map[string]any{
				"ParameterGroupName": map[string]any{"Ref": "Parameters"},
				"ClusterName":        "cfn-cache", "NodeType": "db.r6g.large", "Description": s.description,
				"ACLName":         map[string]any{"Ref": "Acl"},
				"SubnetGroupName": map[string]any{"Ref": "Subnets"},
				"NumShards":       s.shards, "NumReplicasPerShard": 1, "Engine": "valkey",
				"AutoMinorVersionUpgrade": s.autoMinor, "Tags": tags,
			}},
		},
		"Outputs": map[string]any{
			"ClusterRef": map[string]any{"Value": map[string]any{"Ref": "Cache"}},
			"ClusterArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Cache", "ARN"}}},
			"Address": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Cache", "ClusterEndpoint.Address"}},
			},
			"Port": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Cache", "ClusterEndpoint.Port"}},
			},
			"Status":     map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Cache", "Status"}}},
			"SubnetsRef": map[string]any{"Value": map[string]any{"Ref": "Subnets"}},
			"SubnetsArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Subnets", "ARN"}}},
			"UserRef":    map[string]any{"Value": map[string]any{"Ref": "Reader"}},
			"UserArn":    map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Reader", "Arn"}}},
			"UserStatus": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Reader", "Status"}}},
			"AclRef":     map[string]any{"Value": map[string]any{"Ref": "Acl"}},
			"AclArn":     map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Acl", "Arn"}}},
			"AclStatus":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Acl", "Status"}}},
			"ParameterGp": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"Cache", "ParameterGroupStatus"}},
			},
		},
	})
	return string(raw)
}

func TestCloudFormationMemoryDBLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
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

	stack := memoryDBStack{
		subnet: aws.ToString(subnet.Subnet.SubnetId), description: "one", accessLevel: "on ~* +@read",
		members: 2, shards: 1, autoMinor: true, tag: "one",
	}

	update := func(next *memoryDBStack) {
		t.Helper()
		_, updateErr := cf.UpdateStack(ctx, &cfn.UpdateStackInput{
			StackName: aws.String("memorydb-stack"), TemplateBody: aws.String(memoryDBTemplate(next)),
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	}

	if _, err = cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("memorydb-stack"), TemplateBody: aws.String(memoryDBTemplate(&stack)),
	}); err != nil {
		t.Fatal(err)
	}

	created := wantStatus(ctx, t, cf, "memorydb-stack", cfntypes.StackStatusCreateComplete)
	out := outputsOf(&created)
	const prefix = "arn:aws:memorydb:us-east-1:"
	if out["ClusterRef"] != "cfn-cache" || !strings.HasPrefix(out["ClusterArn"], prefix) ||
		!strings.HasSuffix(out["ClusterArn"], ":cluster/cfn-cache") ||
		!strings.HasPrefix(out["Address"], "clustercfg.cfn-cache.") || out["Port"] != "6379" ||
		out["Status"] != "available" || out["ParameterGp"] != "in-sync" ||
		out["SubnetsRef"] != "cfn-net" || !strings.HasSuffix(out["SubnetsArn"], ":subnetgroup/cfn-net") ||
		out["UserRef"] != "cfn-reader" || !strings.HasSuffix(out["UserArn"], ":user/cfn-reader") ||
		out["UserStatus"] != "active" || out["AclRef"] != "cfn-acl" || !strings.HasSuffix(out["AclArn"], ":acl/cfn-acl") ||
		out["AclStatus"] != "active" {
		t.Fatalf("unexpected outputs: %+v", out)
	}

	described, err := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterName: aws.String("cfn-cache")})
	if err != nil || len(described.Clusters) != 1 || aws.ToString(described.Clusters[0].ACLName) != "cfn-acl" ||
		aws.ToString(described.Clusters[0].SubnetGroupName) != "cfn-net" {
		t.Fatal(described, err)
	}

	// Mutable properties update in place.
	stack.description, stack.accessLevel, stack.shards, stack.tag = "two", "on ~* +@write", 2, "two"
	stack.members = 1
	update(&stack)
	updated := wantStatus(ctx, t, cf, "memorydb-stack", cfntypes.StackStatusUpdateComplete)
	if after := outputsOf(&updated); after["ClusterArn"] != out["ClusterArn"] || after["UserArn"] != out["UserArn"] {
		t.Fatalf("a resource was replaced: %+v", after)
	}

	described, err = c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterName: aws.String("cfn-cache")})
	if err != nil || aws.ToString(described.Clusters[0].Description) != "two" ||
		aws.ToInt32(described.Clusters[0].NumberOfShards) != 2 {
		t.Fatal(described, err)
	}

	users, err := c.DescribeUsers(ctx, &sdk.DescribeUsersInput{UserName: aws.String("cfn-reader")})
	if err != nil || aws.ToString(users.Users[0].AccessString) != "on ~* +@write" {
		t.Fatal(users, err)
	}

	acls, err := c.DescribeACLs(ctx, &sdk.DescribeACLsInput{ACLName: aws.String("cfn-acl")})
	if err != nil || !slices.Equal(acls.ACLs[0].UserNames, []string{"cfn-reader"}) {
		t.Fatal(acls, err)
	}

	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: aws.String(out["ClusterArn"])})
	if err != nil || len(tags.TagList) != 1 || aws.ToString(tags.TagList[0].Value) != "two" {
		t.Fatal(tags, err)
	}

	// A setting the API cannot change must roll the update back instead of being ignored.
	rejected := stack
	rejected.autoMinor = false
	update(&rejected)
	wantStatus(ctx, t, cf, "memorydb-stack", cfntypes.StackStatusUpdateRollbackComplete)
	described, err = c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterName: aws.String("cfn-cache")})
	if err != nil || !aws.ToBool(described.Clusters[0].AutoMinorVersionUpgrade) {
		t.Fatal(described, err)
	}

	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("memorydb-stack")}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *created.StackId, cfntypes.StackStatusDeleteComplete)
	left, err := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{})
	if err != nil || len(left.Clusters) != 0 {
		t.Fatal(left, err)
	}

	if _, err = c.DescribeSubnetGroups(
		ctx,
		&sdk.DescribeSubnetGroupsInput{SubnetGroupName: aws.String("cfn-net")},
	); err == nil {
		t.Fatal("the subnet group outlived the stack")
	}
}

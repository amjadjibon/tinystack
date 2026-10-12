package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/redshift"
	"github.com/aws/aws-sdk-go-v2/service/redshift/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func TestRedshiftLifecycle(t *testing.T) {
	server := server(t)
	ctx := context.Background()
	c := sdk.New(sdk.Options{Region: "us-east-1", BaseEndpoint: new(server.URL), Credentials: creds("test")})
	network := ec2sdk.New(
		ec2sdk.Options{Region: "us-east-1", BaseEndpoint: new(server.URL), Credentials: creds("test")},
	)
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2sdk.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.0.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags := []types.Tag{{Key: new("env"), Value: new("sdk")}}
	sg, err := c.CreateClusterSubnetGroup(ctx, &sdk.CreateClusterSubnetGroupInput{
		ClusterSubnetGroupName: new(
			"sdk-subnets",
		),
		Description: new("SDK subnet group"),
		SubnetIds:   []string{*subnet.Subnet.SubnetId},
		Tags:        tags,
	})
	if err != nil || len(sg.ClusterSubnetGroup.Subnets) != 1 {
		t.Fatal(sg, err)
	}

	_, err = c.ModifyClusterSubnetGroup(ctx, &sdk.ModifyClusterSubnetGroupInput{
		ClusterSubnetGroupName: new(
			"sdk-subnets",
		),
		Description: new("modified"),
		SubnetIds:   []string{*subnet.Subnet.SubnetId},
	})
	if err != nil {
		t.Fatal(err)
	}

	subnets, err := c.DescribeClusterSubnetGroups(ctx, &sdk.DescribeClusterSubnetGroupsInput{})
	if err != nil || len(subnets.ClusterSubnetGroups) != 1 ||
		*subnets.ClusterSubnetGroups[0].Description != "modified" {
		t.Fatal(subnets, err)
	}

	pg, err := c.CreateClusterParameterGroup(ctx, &sdk.CreateClusterParameterGroupInput{
		ParameterGroupName: new(
			"sdk-params",
		),
		ParameterGroupFamily: new("redshift-1.0"),
		Description:          new("SDK parameter group"),
		Tags:                 tags,
	})
	if err != nil || *pg.ClusterParameterGroup.ParameterGroupName != "sdk-params" {
		t.Fatal(pg, err)
	}

	groups, err := c.DescribeClusterParameterGroups(ctx, &sdk.DescribeClusterParameterGroupsInput{})
	if err != nil || len(groups.ParameterGroups) != 2 {
		t.Fatal(groups, err)
	}

	in := &sdk.CreateClusterInput{
		ClusterIdentifier: new("sdk-cluster"), ClusterType: new("single-node"),
		NodeType: new("dc2.large"), MasterUsername: new("admin"), MasterUserPassword: new("Password123!"),
		ClusterSubnetGroupName: new("sdk-subnets"), ClusterParameterGroupName: new("sdk-params"),
		VpcSecurityGroupIds: []string{"sg-12345678"}, Tags: tags,
	}

	created, err := c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if *created.Cluster.ClusterStatus != "available" || *created.Cluster.Endpoint.Port != 5439 ||
		created.Cluster.ClusterCreateTime == nil || !strings.Contains(*created.Cluster.ClusterNamespaceArn, ":namespace:") {
		t.Fatal(created)
	}

	_, err = c.CreateCluster(ctx, in)
	var duplicate *types.ClusterAlreadyExistsFault
	if !errors.As(err, &duplicate) {
		t.Fatalf("want modeled duplicate, got %v", err)
	}

	modified, err := c.ModifyCluster(ctx, &sdk.ModifyClusterInput{
		ClusterIdentifier: in.ClusterIdentifier,
		Port:              new(int32(5440)), PubliclyAccessible: new(true), MasterUserPassword: new("Changed123!"),
	})
	if err != nil || *modified.Cluster.Endpoint.Port != 5440 || !*modified.Cluster.PubliclyAccessible {
		t.Fatal(modified, err)
	}

	read, err := c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterIdentifier: in.ClusterIdentifier})
	if err != nil || len(read.Clusters) != 1 || *read.Clusters[0].Endpoint.Port != 5440 {
		t.Fatal(read, err)
	}

	_, err = c.DeleteClusterSubnetGroup(
		ctx,
		&sdk.DeleteClusterSubnetGroupInput{ClusterSubnetGroupName: new("sdk-subnets")},
	)
	var used *types.InvalidClusterSubnetGroupStateFault
	if !errors.As(err, &used) {
		t.Fatalf("want modeled in-use error, got %v", err)
	}

	// Use the configured test account's ARN from the generic tagging inventory.
	taggingClient := rgta.New(
		rgta.Options{Region: "us-east-1", BaseEndpoint: new(server.URL), Credentials: creds("test")},
	)
	resources, err := taggingClient.GetResources(
		ctx,
		&rgta.GetResourcesInput{ResourceTypeFilters: []string{"redshift:cluster"}},
	)
	if err != nil || len(resources.ResourceTagMappingList) != 1 {
		t.Fatal(resources, err)
	}

	arn := *resources.ResourceTagMappingList[0].ResourceARN
	prefix := strings.TrimSuffix(arn, "cluster:sdk-cluster")
	for _, resource := range []string{arn, prefix + "subnetgroup:sdk-subnets", prefix + "parametergroup:sdk-params"} {
		_, err = c.CreateTags(
			ctx,
			&sdk.CreateTagsInput{
				ResourceName: new(resource),
				Tags:         []types.Tag{{Key: new("owner"), Value: new("sdk")}},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		got, e := c.DescribeTags(ctx, &sdk.DescribeTagsInput{ResourceName: new(resource)})
		if e != nil || len(got.TaggedResources) != 2 {
			t.Fatal(got, e)
		}

		_, e = c.DeleteTags(ctx, &sdk.DeleteTagsInput{ResourceName: new(resource), TagKeys: []string{"env"}})
		if e != nil {
			t.Fatal(e)
		}
	}

	tagged, err := taggingClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{arn}, Tags: map[string]string{"rgta": "works"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	untagged, err := taggingClient.UntagResources(
		ctx,
		&rgta.UntagResourcesInput{ResourceARNList: []string{arn}, TagKeys: []string{"rgta"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal(untagged, err)
	}

	_, err = c.DeleteCluster(
		ctx,
		&sdk.DeleteClusterInput{ClusterIdentifier: in.ClusterIdentifier, FinalClusterSnapshotIdentifier: new("final")},
	)
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
		t.Fatalf("final snapshot accepted: %v", err)
	}

	_, err = c.ResizeCluster(
		ctx,
		&sdk.ResizeClusterInput{
			ClusterIdentifier: in.ClusterIdentifier,
			NodeType:          new("dc2.large"),
			NumberOfNodes:     new(int32(2)),
		},
	)
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidAction" {
		t.Fatalf("resize accepted: %v", err)
	}

	_, err = c.ModifyCluster(
		ctx,
		&sdk.ModifyClusterInput{
			ClusterIdentifier: in.ClusterIdentifier,
			NumberOfNodes:     new(int32(2)),
			NodeType:          new("dc2.large"),
		},
	)
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidParameterValue" {
		t.Fatalf("resize input accepted: %v", err)
	}

	deleted, err := c.DeleteCluster(
		ctx,
		&sdk.DeleteClusterInput{ClusterIdentifier: in.ClusterIdentifier, SkipFinalClusterSnapshot: new(true)},
	)
	if err != nil || *deleted.Cluster.ClusterStatus != "deleting" {
		t.Fatal(deleted, err)
	}

	_, err = c.DescribeClusters(ctx, &sdk.DescribeClustersInput{ClusterIdentifier: in.ClusterIdentifier})
	var missing *types.ClusterNotFoundFault
	if !errors.As(err, &missing) {
		t.Fatalf("want modeled not found, got %v", err)
	}

	_, err = c.DeleteClusterSubnetGroup(
		ctx,
		&sdk.DeleteClusterSubnetGroupInput{ClusterSubnetGroupName: new("sdk-subnets")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteClusterParameterGroup(
		ctx,
		&sdk.DeleteClusterParameterGroupInput{ParameterGroupName: new("sdk-params")},
	)
	if err != nil {
		t.Fatal(err)
	}
}

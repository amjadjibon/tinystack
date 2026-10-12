package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/docdb"
	docdbtypes "github.com/aws/aws-sdk-go-v2/service/docdb/types"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/neptune"
	neptunetypes "github.com/aws/aws-sdk-go-v2/service/neptune/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroups"
	rgtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroups/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func TestDocDBSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	r := rdsClient(s, "test", "us-east-1")
	network := ec2sdk.New(ec2sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	vpc, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(ctx, &ec2sdk.CreateSubnetInput{
		VpcId:     vpc.Vpc.VpcId,
		CidrBlock: new("10.0.1.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}

	group, err := network.CreateSecurityGroup(ctx, &ec2sdk.CreateSecurityGroupInput{
		VpcId:     vpc.Vpc.VpcId,
		GroupName: new("docdb"), Description: new("docdb security group"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        new("docdb-subnets"),
		DBSubnetGroupDescription: new("DocumentDB subnets"), SubnetIds: []string{*subnet.Subnet.SubnetId},
	})
	if err != nil {
		t.Fatal(err)
	}

	in := &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new("documents"), Engine: new("docdb"),
		MasterUsername: new("masteruser"), MasterUserPassword: new(strings.Repeat("p", 100)),
		StorageEncrypted: new(true), KmsKeyId: new("alias/test"), DBSubnetGroupName: new("DOCDB-SUBNETS"),
		VpcSecurityGroupIds: []string{*group.GroupId}, Tags: []docdbtypes.Tag{{Key: new("owner"), Value: new("tests")}},
	}

	created, err := c.CreateDBCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	cluster := created.DBCluster
	if aws.ToInt32(cluster.Port) != 27017 || aws.ToString(cluster.EngineVersion) != "5.0" ||
		aws.ToString(cluster.Status) != "available" || !strings.HasSuffix(*cluster.Endpoint, ".docdb.amazonaws.com") {
		t.Fatal(cluster)
	}

	_, err = c.CreateDBCluster(ctx, in)
	var duplicate *docdbtypes.DBClusterAlreadyExistsFault
	if !errors.As(err, &duplicate) {
		t.Fatal("expected modeled duplicate", err)
	}

	_, err = r.DeleteDBSubnetGroup(ctx, &rds.DeleteDBSubnetGroupInput{DBSubnetGroupName: new("docdb-subnets")})
	code(t, err, "InvalidDBSubnetGroupStateFault")
	_, err = c.ModifyDBCluster(ctx, &docdb.ModifyDBClusterInput{
		DBClusterIdentifier:   in.DBClusterIdentifier,
		BackupRetentionPeriod: new(int32(7)), Port: new(int32(0)), DeletionProtection: new(true),
	})
	code(t, err, "InvalidParameterValue")
	unchanged, err := c.DescribeDBClusters(
		ctx,
		&docdb.DescribeDBClustersInput{DBClusterIdentifier: in.DBClusterIdentifier},
	)
	if err != nil || aws.ToInt32(unchanged.DBClusters[0].BackupRetentionPeriod) != 1 ||
		aws.ToBool(unchanged.DBClusters[0].DeletionProtection) {
		t.Fatal(unchanged, err)
	}

	modified, err := c.ModifyDBCluster(ctx, &docdb.ModifyDBClusterInput{
		DBClusterIdentifier:   in.DBClusterIdentifier,
		BackupRetentionPeriod: new(int32(7)), Port: new(int32(27018)), MasterUserPassword: new("changed-password"),
		DeletionProtection: new(true), ApplyImmediately: new(true), VpcSecurityGroupIds: []string{*group.GroupId},
	})
	if err != nil || *modified.DBCluster.DbClusterResourceId != *cluster.DbClusterResourceId ||
		aws.ToInt32(modified.DBCluster.Port) != 27018 {
		t.Fatal(modified, err)
	}

	member, err := c.CreateDBInstance(ctx, &docdb.CreateDBInstanceInput{
		DBInstanceIdentifier: new("documents-node"),
		DBInstanceClass:      new("db.r5.large"), Engine: new("docdb"), DBClusterIdentifier: in.DBClusterIdentifier,
	})
	if err != nil || *member.DBInstance.Endpoint.Address != *cluster.Endpoint {
		t.Fatal(member, err)
	}

	_, err = c.ModifyDBInstance(ctx, &docdb.ModifyDBInstanceInput{
		DBInstanceIdentifier: new("documents-node"),
		DBInstanceClass:      new("db.r5.xlarge"), ApplyImmediately: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	members, err := c.DescribeDBInstances(
		ctx,
		&docdb.DescribeDBInstancesInput{DBInstanceIdentifier: new("documents-node")},
	)
	if err != nil || *members.DBInstances[0].DBInstanceClass != "db.r5.xlarge" {
		t.Fatal(members, err)
	}

	described, err := c.DescribeDBClusters(ctx, &docdb.DescribeDBClustersInput{Filters: []docdbtypes.Filter{
		{Name: new("engine"), Values: []string{"docdb"}},
	}})
	if err != nil || len(described.DBClusters) != 1 || len(described.DBClusters[0].DBClusterMembers) != 1 ||
		!aws.ToBool(described.DBClusters[0].DBClusterMembers[0].IsClusterWriter) {
		t.Fatal(described, err)
	}

	_, err = c.AddTagsToResource(ctx, &docdb.AddTagsToResourceInput{
		ResourceName: cluster.DBClusterArn,
		Tags:         []docdbtypes.Tag{{Key: new("extra"), Value: new("yes")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RemoveTagsFromResource(ctx, &docdb.RemoveTagsFromResourceInput{
		ResourceName: cluster.DBClusterArn,
		TagKeys:      []string{"owner"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &docdb.ListTagsForResourceInput{ResourceName: cluster.DBClusterArn})
	if err != nil || len(tags.TagList) != 1 || *tags.TagList[0].Key != "extra" {
		t.Fatal(tags, err)
	}

	tagger := resourcegroupstaggingapi.New(resourcegroupstaggingapi.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: creds("test"),
	})
	_, err = tagger.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{*cluster.DBClusterArn},
		Tags:            map[string]string{"rgta": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagged, err := tagger.GetResources(
		ctx,
		&resourcegroupstaggingapi.GetResourcesInput{ResourceTypeFilters: []string{"rds:cluster"}},
	)
	if err != nil || len(tagged.ResourceTagMappingList) != 1 {
		t.Fatal(tagged, err)
	}

	rg := resourcegroups.New(
		resourcegroups.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	selected, err := rg.SearchResources(ctx, &resourcegroups.SearchResourcesInput{ResourceQuery: docDBResourceQuery()})
	if err != nil || len(selected.ResourceIdentifiers) != 1 ||
		*selected.ResourceIdentifiers[0].ResourceType != "AWS::DocDB::DBCluster" {
		t.Fatal(selected, err)
	}

	_, err = c.DeleteDBCluster(
		ctx,
		&docdb.DeleteDBClusterInput{DBClusterIdentifier: in.DBClusterIdentifier, SkipFinalSnapshot: new(true)},
	)
	code(t, err, "InvalidDBClusterStateFault")
	deletedMember, err := c.DeleteDBInstance(
		ctx,
		&docdb.DeleteDBInstanceInput{DBInstanceIdentifier: new("documents-node")},
	)
	if err != nil || *deletedMember.DBInstance.DBInstanceStatus != "deleting" {
		t.Fatal(deletedMember, err)
	}

	_, err = c.DeleteDBCluster(
		ctx,
		&docdb.DeleteDBClusterInput{DBClusterIdentifier: in.DBClusterIdentifier, SkipFinalSnapshot: new(true)},
	)
	code(t, err, "InvalidDBClusterStateFault")
	_, err = c.ModifyDBCluster(
		ctx,
		&docdb.ModifyDBClusterInput{DBClusterIdentifier: in.DBClusterIdentifier, DeletionProtection: new(false)},
	)
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteDBCluster(
		ctx,
		&docdb.DeleteDBClusterInput{DBClusterIdentifier: in.DBClusterIdentifier, SkipFinalSnapshot: new(true)},
	)
	if err != nil || *deleted.DBCluster.Status != "deleting" {
		t.Fatal(deleted, err)
	}

	_, err = c.DescribeDBClusters(ctx, &docdb.DescribeDBClustersInput{DBClusterIdentifier: in.DBClusterIdentifier})
	var missing *docdbtypes.DBClusterNotFoundFault
	if !errors.As(err, &missing) {
		t.Fatal("expected modeled not found", err)
	}
}

func TestNeptuneSDKLifecycleAndSharedRDS(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := neptune.New(neptune.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	d := docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	r := rdsClient(s, "test", "us-east-1")
	in := &neptune.CreateDBClusterInput{
		DBClusterIdentifier: new("graph"), Engine: new("neptune"),
		Tags: []neptunetypes.Tag{{Key: new("owner"), Value: new("test")}},
	}

	created, err := c.CreateDBCluster(ctx, in)
	if err != nil || aws.ToInt32(created.DBCluster.Port) != 8182 {
		t.Fatal(created, err)
	}

	_, err = c.CreateDBCluster(ctx, in)
	var duplicate *neptunetypes.DBClusterAlreadyExistsFault
	if !errors.As(err, &duplicate) {
		t.Fatal(err)
	}

	_, err = d.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new("documents"), Engine: new("docdb"),
		MasterUsername: new("masteruser"), MasterUserPassword: new("Password123!"),
	})
	if err != nil {
		t.Fatal(err)
	}

	viaDocDB, err := d.DescribeDBClusters(ctx, &docdb.DescribeDBClustersInput{})
	if err != nil || len(viaDocDB.DBClusters) != 2 {
		t.Fatal(viaDocDB, err)
	}

	viaRDS, err := r.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{})
	if err != nil || len(viaRDS.DBClusters) != 2 {
		t.Fatal(viaRDS, err)
	}

	viaNeptune, err := c.DescribeDBClusters(ctx, &neptune.DescribeDBClustersInput{Filters: []neptunetypes.Filter{
		{Name: new("engine"), Values: []string{"neptune"}},
	}})
	if err != nil || len(viaNeptune.DBClusters) != 1 {
		t.Fatal(viaNeptune, err)
	}

	_, err = r.CreateDBCluster(
		ctx,
		&rds.CreateDBClusterInput{DBClusterIdentifier: new("aurora"), Engine: new("aurora-mysql")},
	)
	code(t, err, "InvalidParameterValue")
	_, err = c.CreateDBInstance(ctx, &neptune.CreateDBInstanceInput{
		DBInstanceIdentifier: new("wrong"),
		DBInstanceClass:      new("db.r5.large"), Engine: new("neptune"), DBClusterIdentifier: new("documents"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = r.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: new("no-cluster"),
		DBInstanceClass:      new("db.r5.large"), Engine: new("neptune"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.CreateDBInstance(ctx, &neptune.CreateDBInstanceInput{
		DBInstanceIdentifier: new("graph-node"),
		DBInstanceClass:      new("db.r5.large"), Engine: new("neptune"), DBClusterIdentifier: in.DBClusterIdentifier,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBInstance(ctx, &neptune.ModifyDBInstanceInput{
		DBInstanceIdentifier: new("graph-node"),
		DBInstanceClass:      new("db.r5.xlarge"), ApplyImmediately: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	modified, err := c.ModifyDBCluster(ctx, &neptune.ModifyDBClusterInput{
		DBClusterIdentifier: in.DBClusterIdentifier,
		Port:                new(int32(8183)), BackupRetentionPeriod: new(int32(14)), ApplyImmediately: new(false),
	})
	if err != nil || *modified.DBCluster.Endpoint != *created.DBCluster.Endpoint {
		t.Fatal(modified, err)
	}

	members, err := c.DescribeDBInstances(
		ctx,
		&neptune.DescribeDBInstancesInput{DBInstanceIdentifier: new("graph-node")},
	)
	if err != nil || *members.DBInstances[0].Endpoint.Port != 8183 ||
		*members.DBInstances[0].DBInstanceClass != "db.r5.xlarge" {
		t.Fatal(members, err)
	}

	_, err = c.ModifyDBCluster(ctx, &neptune.ModifyDBClusterInput{
		DBClusterIdentifier: in.DBClusterIdentifier,
		EngineVersion:       new("1.2.1.0"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.AddTagsToResource(ctx, &neptune.AddTagsToResourceInput{
		ResourceName: created.DBCluster.DBClusterArn,
		Tags:         []neptunetypes.Tag{{Key: new("version"), Value: new("two")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RemoveTagsFromResource(ctx, &neptune.RemoveTagsFromResourceInput{
		ResourceName: created.DBCluster.DBClusterArn,
		TagKeys:      []string{"owner"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(
		ctx,
		&neptune.ListTagsForResourceInput{ResourceName: created.DBCluster.DBClusterArn},
	)
	if err != nil || len(tags.TagList) != 1 || *tags.TagList[0].Value != "two" {
		t.Fatal(tags, err)
	}

	_, err = c.DeleteDBInstance(ctx, &neptune.DeleteDBInstanceInput{DBInstanceIdentifier: new("graph-node")})
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteDBCluster(ctx, &neptune.DeleteDBClusterInput{
		DBClusterIdentifier: in.DBClusterIdentifier,
		SkipFinalSnapshot:   new(true),
	})
	if err != nil || *deleted.DBCluster.Status != "deleting" {
		t.Fatal(deleted, err)
	}

	_, err = c.DescribeDBClusters(ctx, &neptune.DescribeDBClustersInput{DBClusterIdentifier: in.DBClusterIdentifier})
	var missing *neptunetypes.DBClusterNotFoundFault
	if !errors.As(err, &missing) {
		t.Fatal(err)
	}

	// Unsupported whitelisted RDS members must fail explicitly on the cluster path too.
	_, err = r.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: new("inherited"),
		DBInstanceClass: new(
			"db.r5.large",
		),
		Engine:              new("docdb"),
		DBClusterIdentifier: new("documents"),
		AllocatedStorage:    new(int32(20)),
	})
	code(t, err, "InvalidParameterValue")
	_, err = r.DescribeDBClusters(
		ctx,
		&rds.DescribeDBClustersInput{Filters: []rdstypes.Filter{{Name: new("unsupported"), Values: []string{"x"}}}},
	)
	code(t, err, "InvalidParameterValue")
}

func docDBResourceQuery() *rgtypes.ResourceQuery {
	return &rgtypes.ResourceQuery{
		Type:  rgtypes.QueryTypeTagFilters10,
		Query: new(`{"ResourceTypeFilters":["AWS::DocDB::DBCluster"],"TagFilters":[{"Key":"rgta","Values":["yes"]}]}`),
	}
}

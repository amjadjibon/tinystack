package integration_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

func TestRDSModifyDBSubnetGroup(t *testing.T) {
	s := server(t)
	ctx := t.Context()
	c := rdsClient(s, "test", "us-east-1")
	vpc, subnets := cacheSubnets(t, s)
	net := ec2Client(s, "test", "us-east-1")
	extra, err := net.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: new(vpc), CidrBlock: new("172.31.240.0/24")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName: new("modifiable"), DBSubnetGroupDescription: new("original"), SubnetIds: subnets,
	})
	if err != nil {
		t.Fatal(err)
	}

	in := rdsInput("attached")
	in.DBSubnetGroupName = new("modifiable")
	if _, err = c.CreateDBInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	update := &rds.ModifyDBSubnetGroupInput{
		DBSubnetGroupName: new("Modifiable"), DBSubnetGroupDescription: new("updated"),
		SubnetIds: append(subnets, aws.ToString(extra.Subnet.SubnetId)),
	}
	out, err := c.ModifyDBSubnetGroup(ctx, update)
	if err != nil || len(out.DBSubnetGroup.Subnets) != 3 || aws.ToString(out.DBSubnetGroup.VpcId) != vpc {
		t.Fatal(out, err)
	}

	update.SubnetIds = subnets
	_, err = c.ModifyDBSubnetGroup(ctx, update)
	code(t, err, "SubnetAlreadyInUse")
	update.SubnetIds = []string{subnets[0], subnets[0]}
	_, err = c.ModifyDBSubnetGroup(ctx, update)
	code(t, err, "InvalidSubnet")
	update.SubnetIds = []string{subnets[0], "subnet-missing"}
	_, err = c.ModifyDBSubnetGroup(ctx, update)
	code(t, err, "InvalidSubnet")
	db, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("attached")})
	if err != nil || len(db.DBInstances[0].DBSubnetGroup.Subnets) != 3 ||
		aws.ToString(db.DBInstances[0].DBSubnetGroup.DBSubnetGroupDescription) != "updated" {
		t.Fatal(db, err)
	}

	if _, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: new("attached"), SkipFinalSnapshot: new(true),
	}); err != nil {
		t.Fatal(err)
	}

	update.SubnetIds, update.DBSubnetGroupDescription = subnets, nil
	out, err = c.ModifyDBSubnetGroup(ctx, update)
	if err != nil || len(out.DBSubnetGroup.Subnets) != 2 ||
		aws.ToString(out.DBSubnetGroup.DBSubnetGroupDescription) != "updated" {
		t.Fatal(out, err)
	}
}

package integration_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestEC2DefaultVPCAndZones(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "eu-west-2")
	ctx := context.Background()
	zones, err := c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	if err != nil || len(zones.AvailabilityZones) != 3 ||
		aws.ToString(zones.AvailabilityZones[0].ZoneName) != "eu-west-2a" ||
		aws.ToString(zones.AvailabilityZones[0].ZoneId) != "euw2-az1" ||
		zones.AvailabilityZones[0].State != ec2types.AvailabilityZoneStateAvailable {
		t.Fatalf("got %+v, %v", zones, err)
	}

	byID, err := c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{ZoneIds: []string{"euw2-az3"}})
	if err != nil || len(byID.AvailabilityZones) != 1 ||
		aws.ToString(byID.AvailabilityZones[0].ZoneName) != "eu-west-2c" {
		t.Fatalf("got %+v, %v", byID, err)
	}

	_, err = c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{ZoneNames: []string{"eu-west-2z"}})
	code(t, err, "InvalidParameterValue")

	// Each account and Region gets a default VPC with a subnet in each zone.
	vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil || len(vpcs.Vpcs) != 1 || !aws.ToBool(vpcs.Vpcs[0].IsDefault) ||
		aws.ToString(vpcs.Vpcs[0].CidrBlock) != "172.31.0.0/16" || vpcs.Vpcs[0].State != ec2types.VpcStateAvailable ||
		aws.ToString(vpcs.Vpcs[0].OwnerId) != "000000000000" || len(vpcs.Vpcs[0].CidrBlockAssociationSet) != 1 {
		t.Fatalf("got %+v, %v", vpcs, err)
	}

	defaultVPC := aws.ToString(vpcs.Vpcs[0].VpcId)
	subnets, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if err != nil || len(subnets.Subnets) != 3 {
		t.Fatalf("got %+v, %v", subnets, err)
	}

	for _, sn := range subnets.Subnets {
		if aws.ToString(sn.VpcId) != defaultVPC || !aws.ToBool(sn.DefaultForAz) ||
			!aws.ToBool(sn.MapPublicIpOnLaunch) ||
			aws.ToInt32(sn.AvailableIpAddressCount) != 4091 {
			t.Fatalf("got %+v", sn)
		}
	}

	other, err := ec2Client(s, "team-b", "eu-west-2").DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil || len(other.Vpcs) != 1 || aws.ToString(other.Vpcs[0].VpcId) == defaultVPC {
		t.Fatalf("other account: %+v, %v", other, err)
	}

	_, err = c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{"vpc-00000000000000000"}})
	code(t, err, "InvalidVpcID.NotFound")

	// Without its subnets, the default VPC can be deleted for good.
	for _, sn := range subnets.Subnets {
		if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.SubnetId}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(defaultVPC)}); err != nil {
		t.Fatal(err)
	}

	if vpcs, err = c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}); err != nil || len(vpcs.Vpcs) != 0 {
		t.Fatalf("got %+v, %v", vpcs, err)
	}
}

func TestEC2VPCsAndSubnets(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	for _, cidr := range []string{"10.0.0.0/8", "10.0.0.0/29"} {
		_, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String(cidr)})
		code(t, err, "InvalidVpc.Range")
	}

	_, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.1/16")})
	code(t, err, "InvalidParameterValue")
	_, err = c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16"), DryRun: aws.Bool(true)})
	code(t, err, "DryRunOperation")

	created, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil || aws.ToBool(created.Vpc.IsDefault) || created.Vpc.InstanceTenancy != ec2types.TenancyDefault {
		t.Fatalf("got %+v, %v", created, err)
	}

	id := created.Vpc.VpcId
	for _, cidr := range []string{"10.1.0.0/24", "10.0.0.0/8"} {
		_, err = c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: id, CidrBlock: aws.String(cidr)})
		code(t, err, "InvalidSubnet.Range")
	}

	_, err = c.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId: id, CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZone: aws.String("us-west-2a"),
	})
	code(t, err, "InvalidParameterValue")
	subnet, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId: id, CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZoneId: aws.String("use1-az2"),
	})
	if err != nil || aws.ToString(subnet.Subnet.AvailabilityZone) != "us-east-1b" ||
		aws.ToInt32(subnet.Subnet.AvailableIpAddressCount) != 251 || aws.ToBool(subnet.Subnet.DefaultForAz) ||
		aws.ToString(subnet.Subnet.SubnetArn) !=
			"arn:aws:ec2:us-east-1:000000000000:subnet/"+aws.ToString(subnet.Subnet.SubnetId) {
		t.Fatalf("got %+v, %v", subnet, err)
	}

	_, err = c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: id, CidrBlock: aws.String("10.0.1.128/25")})
	code(t, err, "InvalidSubnet.Conflict")
	_, err = c.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId: aws.String("vpc-00000000000000000"), CidrBlock: aws.String("10.0.2.0/24"),
	})
	code(t, err, "InvalidVpcID.NotFound")

	// A VPC with subnets cannot be deleted.
	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: id})
	code(t, err, "DependencyViolation")
	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: subnet.Subnet.SubnetId}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: subnet.Subnet.SubnetId})
	code(t, err, "InvalidSubnetID.NotFound")
	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: id}); err != nil {
		t.Fatal(err)
	}

	// Five VPCs per Region, the default one included; pages of at least five.
	var ids []string
	for i := range 4 {
		out, createErr := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String(fmt.Sprintf("10.%d.0.0/16", i+1))})
		if createErr != nil {
			t.Fatal(createErr)
		}

		ids = append(ids, aws.ToString(out.Vpc.VpcId))
	}

	_, err = c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.9.0.0/16")})
	code(t, err, "VpcLimitExceeded")
	_, err = c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{MaxResults: aws.Int32(4)})
	code(t, err, "InvalidParameterValue")

	var listed []string
	paginator := ec2.NewDescribeVpcsPaginator(c, &ec2.DescribeVpcsInput{MaxResults: aws.Int32(5)})
	for paginator.HasMorePages() {
		out, pageErr := paginator.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		for _, v := range out.Vpcs {
			listed = append(listed, aws.ToString(v.VpcId))
		}
	}

	if len(listed) != 5 || !slices.IsSorted(listed) {
		t.Fatalf("listed %v", listed)
	}

	named, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: ids[:2]})
	if err != nil || len(named.Vpcs) != 2 {
		t.Fatalf("got %+v, %v", named, err)
	}
}

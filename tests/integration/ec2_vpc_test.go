package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// createVPC creates a VPC with a CIDR block and returns its ID.
func createVPC(t *testing.T, c *ec2.Client, block string) string {
	t.Helper()
	out, err := c.CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: aws.String(block)})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Vpc.VpcId)
}

// createSubnet creates a subnet in a VPC's first zone and returns its ID.
func createSubnet(t *testing.T, c *ec2.Client, vpcID, block string) string {
	t.Helper()
	out, err := c.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId: aws.String(vpcID), CidrBlock: aws.String(block),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Subnet.SubnetId)
}

func TestEC2VPCAttributes(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	vpcID := createVPC(t, c, "10.0.0.0/16")
	attr := func(name ec2types.VpcAttributeName) *ec2.DescribeVpcAttributeOutput {
		t.Helper()
		out, err := c.DescribeVpcAttribute(ctx, &ec2.DescribeVpcAttributeInput{
			VpcId: aws.String(vpcID), Attribute: name,
		})
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	if !aws.ToBool(attr(ec2types.VpcAttributeNameEnableDnsSupport).EnableDnsSupport.Value) ||
		aws.ToBool(attr(ec2types.VpcAttributeNameEnableDnsHostnames).EnableDnsHostnames.Value) {
		t.Fatal("new VPCs resolve DNS but have no host names")
	}

	if _, err := c.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcID), EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		t.Fatal(err)
	}

	if !aws.ToBool(attr(ec2types.VpcAttributeNameEnableDnsHostnames).EnableDnsHostnames.Value) {
		t.Fatal("host names were not enabled")
	}

	// Host names need DNS support, and a request changes one attribute.
	_, err := c.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcID), EnableDnsSupport: &ec2types.AttributeBooleanValue{Value: aws.Bool(false)},
	})
	code(t, err, "InvalidParameterCombination")
	_, err = c.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsSupport:   &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.1.0.0/16"), AmazonProvidedIpv6CidrBlock: aws.Bool(true),
	})
	code(t, err, "InvalidParameterValue")

	subnetID := createSubnet(t, c, vpcID, "10.0.1.0/24")
	for _, in := range []*ec2.ModifySubnetAttributeInput{
		{MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}},
		{PrivateDnsHostnameTypeOnLaunch: ec2types.HostnameTypeResourceName},
		{EnableResourceNameDnsARecordOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}},
	} {
		in.SubnetId = aws.String(subnetID)
		if _, err = c.ModifySubnetAttribute(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	subnets, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err != nil {
		t.Fatal(err)
	}

	sn := subnets.Subnets[0]
	if !aws.ToBool(sn.MapPublicIpOnLaunch) ||
		sn.PrivateDnsNameOptionsOnLaunch.HostnameType != ec2types.HostnameTypeResourceName ||
		!aws.ToBool(sn.PrivateDnsNameOptionsOnLaunch.EnableResourceNameDnsARecord) {
		t.Fatalf("got %+v", sn)
	}

	_, err = c.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
		SubnetId: aws.String(subnetID), EnableDns64: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	code(t, err, "InvalidParameterValue")
}

func TestEC2InternetGateways(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	vpcID := createVPC(t, c, "10.0.0.0/16")
	created, err := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInternetGateway,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("edge")}},
		}},
	})
	if err != nil || len(created.InternetGateway.Attachments) != 0 || len(created.InternetGateway.Tags) != 1 {
		t.Fatalf("got %+v, %v", created, err)
	}

	id := created.InternetGateway.InternetGatewayId
	if _, err = c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: id, VpcId: aws.String(vpcID),
	}); err != nil {
		t.Fatal(err)
	}

	// A gateway attaches to one VPC, and a VPC takes one gateway.
	_, err = c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: id, VpcId: aws.String(createVPC(t, c, "10.1.0.0/16")),
	})
	code(t, err, "Resource.AlreadyAssociated")
	second, err := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: second.InternetGateway.InternetGatewayId, VpcId: aws.String(vpcID),
	})
	code(t, err, "InvalidParameterValue")

	described, err := c.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("tag:Name"), Values: []string{"edge"}},
		},
	})
	if err != nil || len(described.InternetGateways) != 1 ||
		aws.ToString(described.InternetGateways[0].Attachments[0].VpcId) != vpcID ||
		described.InternetGateways[0].Attachments[0].State != "available" {
		t.Fatalf("got %+v, %v", described, err)
	}

	_, err = c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: id})
	code(t, err, "DependencyViolation")
	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	code(t, err, "DependencyViolation")
	_, err = c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
		InternetGatewayId: second.InternetGateway.InternetGatewayId, VpcId: aws.String(vpcID),
	})
	code(t, err, "Gateway.NotAttached")

	if _, err = c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
		InternetGatewayId: id, VpcId: aws.String(vpcID),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{InternetGatewayIds: []string{*id}})
	code(t, err, "InvalidInternetGatewayID.NotFound")
	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}); err != nil {
		t.Fatal(err)
	}
}

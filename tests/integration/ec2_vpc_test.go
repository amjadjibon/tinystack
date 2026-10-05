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

// mainRouteTable returns a VPC's main route table.
func mainRouteTable(t *testing.T, c *ec2.Client, vpcID string) ec2types.RouteTable {
	t.Helper()
	out, err := c.DescribeRouteTables(context.Background(), &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("association.main"), Values: []string{"true"}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil || len(out.RouteTables) != 1 {
		t.Fatalf("got %+v, %v", out, err)
	}

	return out.RouteTables[0]
}

func TestEC2RouteTables(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()

	// The default VPC's main route table sends the internet to its gateway.
	vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		t.Fatal(err)
	}

	defaultMain := mainRouteTable(t, c, aws.ToString(vpcs.Vpcs[0].VpcId))
	if len(defaultMain.Routes) != 2 || aws.ToString(defaultMain.Routes[0].GatewayId) != "local" ||
		aws.ToString(defaultMain.Routes[1].DestinationCidrBlock) != "0.0.0.0/0" ||
		defaultMain.Routes[1].State != ec2types.RouteStateActive {
		t.Fatalf("got %+v", defaultMain.Routes)
	}

	vpcID := createVPC(t, c, "10.0.0.0/16")
	main := mainRouteTable(t, c, vpcID)
	if len(main.Routes) != 1 || aws.ToString(main.Routes[0].DestinationCidrBlock) != "10.0.0.0/16" ||
		main.Routes[0].Origin != ec2types.RouteOriginCreateRouteTable ||
		len(main.Associations) != 1 || !aws.ToBool(main.Associations[0].Main) {
		t.Fatalf("got %+v", main)
	}

	created, err := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	if err != nil {
		t.Fatal(err)
	}

	tableID := created.RouteTable.RouteTableId
	gateway, err := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}

	gatewayID := gateway.InternetGateway.InternetGatewayId
	internet := &ec2.CreateRouteInput{
		RouteTableId: tableID, DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: gatewayID,
	}
	_, err = c.CreateRoute(ctx, internet)
	code(t, err, "InvalidParameterValue")
	if _, err = c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: gatewayID, VpcId: aws.String(vpcID),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateRoute(ctx, internet); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateRoute(ctx, internet)
	code(t, err, "RouteAlreadyExists")
	for _, in := range []*ec2.CreateRouteInput{
		{RouteTableId: tableID, DestinationCidrBlock: aws.String("10.9.0.1/16"), GatewayId: gatewayID},
		{RouteTableId: tableID, DestinationCidrBlock: aws.String("10.9.0.0/16")},
	} {
		_, err = c.CreateRoute(ctx, in)
		if err == nil {
			t.Fatalf("created %+v", in)
		}
	}

	_, err = c.DeleteRoute(ctx, &ec2.DeleteRouteInput{
		RouteTableId: tableID, DestinationCidrBlock: aws.String("10.0.0.0/16"),
	})
	code(t, err, "InvalidParameterValue")

	// A subnet takes one explicit association.
	subnetID := createSubnet(t, c, vpcID, "10.0.1.0/24")
	associated, err := c.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: tableID, SubnetId: aws.String(subnetID),
	})
	if err != nil || associated.AssociationState.State != ec2types.RouteTableAssociationStateCodeAssociated {
		t.Fatalf("got %+v, %v", associated, err)
	}

	_, err = c.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: main.RouteTableId, SubnetId: aws.String(subnetID),
	})
	code(t, err, "Resource.AlreadyAssociated")
	_, err = c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: tableID})
	code(t, err, "DependencyViolation")

	bySubnet, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{{Name: aws.String("association.subnet-id"), Values: []string{subnetID}}},
	})
	if err != nil || len(bySubnet.RouteTables) != 1 || *bySubnet.RouteTables[0].RouteTableId != *tableID {
		t.Fatalf("got %+v, %v", bySubnet, err)
	}

	// Detaching the gateway leaves the route a black hole.
	if _, err = c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
		InternetGatewayId: gatewayID, VpcId: aws.String(vpcID),
	}); err != nil {
		t.Fatal(err)
	}

	blackholes, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{{Name: aws.String("route.state"), Values: []string{"blackhole"}}},
	})
	if err != nil || len(blackholes.RouteTables) != 1 {
		t.Fatalf("got %+v, %v", blackholes, err)
	}

	// The main association moves to the new table and back.
	moved, err := c.ReplaceRouteTableAssociation(ctx, &ec2.ReplaceRouteTableAssociationInput{
		AssociationId: main.Associations[0].RouteTableAssociationId, RouteTableId: tableID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if now := mainRouteTable(t, c, vpcID); *now.RouteTableId != *tableID || len(now.Associations) != 2 {
		t.Fatalf("got %+v", now)
	}

	_, err = c.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: moved.NewAssociationId})
	code(t, err, "InvalidParameterValue")
	if _, err = c.ReplaceRouteTableAssociation(ctx, &ec2.ReplaceRouteTableAssociationInput{
		AssociationId: moved.NewAssociationId, RouteTableId: main.RouteTableId,
	}); err != nil {
		t.Fatal(err)
	}

	// Deleting the subnet removes its association.
	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: associated.AssociationId})
	code(t, err, "InvalidAssociationID.NotFound")
	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	code(t, err, "DependencyViolation")
	if _, err = c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: tableID}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{RouteTableIds: []string{*main.RouteTableId}})
	code(t, err, "InvalidRouteTableID.NotFound")
}

func TestEC2NetworkACLs(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	vpcID := createVPC(t, c, "10.0.0.0/16")
	subnetID := createSubnet(t, c, vpcID, "10.0.1.0/24")

	// A VPC's default ACL allows all traffic and holds its new subnets.
	defaults, err := c.DescribeNetworkAcls(ctx, &ec2.DescribeNetworkAclsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("default"), Values: []string{"true"}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil || len(defaults.NetworkAcls) != 1 {
		t.Fatalf("got %+v, %v", defaults, err)
	}

	defaultACL := defaults.NetworkAcls[0]
	if len(defaultACL.Entries) != 4 || aws.ToInt32(defaultACL.Entries[0].RuleNumber) != 100 ||
		defaultACL.Entries[0].RuleAction != ec2types.RuleActionAllow || aws.ToBool(defaultACL.Entries[0].Egress) ||
		len(defaultACL.Associations) != 1 || aws.ToString(defaultACL.Associations[0].SubnetId) != subnetID {
		t.Fatalf("got %+v", defaultACL)
	}

	created, err := c.CreateNetworkAcl(ctx, &ec2.CreateNetworkAclInput{VpcId: aws.String(vpcID)})
	if err != nil || aws.ToBool(created.NetworkAcl.IsDefault) || len(created.NetworkAcl.Entries) != 2 ||
		created.NetworkAcl.Entries[0].RuleAction != ec2types.RuleActionDeny {
		t.Fatalf("got %+v, %v", created, err)
	}

	aclID := created.NetworkAcl.NetworkAclId
	https := &ec2.CreateNetworkAclEntryInput{
		NetworkAclId: aclID, RuleNumber: aws.Int32(110), Protocol: aws.String("tcp"), Egress: aws.Bool(false),
		RuleAction: ec2types.RuleActionAllow, CidrBlock: aws.String("0.0.0.0/0"),
		PortRange: &ec2types.PortRange{From: aws.Int32(443), To: aws.Int32(443)},
	}
	if _, err = c.CreateNetworkAclEntry(ctx, https); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateNetworkAclEntry(ctx, https)
	code(t, err, "NetworkAclEntryAlreadyExists")
	for _, in := range []*ec2.CreateNetworkAclEntryInput{
		{
			NetworkAclId: aclID, RuleNumber: aws.Int32(120), Protocol: aws.String("6"), Egress: aws.Bool(false),
			RuleAction: ec2types.RuleActionAllow, CidrBlock: aws.String("0.0.0.0/0"),
		},
		{
			NetworkAclId: aclID, RuleNumber: aws.Int32(32767), Protocol: aws.String("-1"), Egress: aws.Bool(false),
			RuleAction: ec2types.RuleActionAllow, CidrBlock: aws.String("0.0.0.0/0"),
		},
	} {
		if _, err = c.CreateNetworkAclEntry(ctx, in); err == nil {
			t.Fatalf("created %+v", in)
		}
	}

	if _, err = c.CreateNetworkAclEntry(ctx, &ec2.CreateNetworkAclEntryInput{
		NetworkAclId: aclID, RuleNumber: aws.Int32(100), Protocol: aws.String("icmp"), Egress: aws.Bool(true),
		RuleAction: ec2types.RuleActionAllow, CidrBlock: aws.String("10.0.0.0/8"),
		IcmpTypeCode: &ec2types.IcmpTypeCode{Type: aws.Int32(-1), Code: aws.Int32(-1)},
	}); err != nil {
		t.Fatal(err)
	}

	https.RuleAction, https.CidrBlock = ec2types.RuleActionDeny, aws.String("192.0.2.0/24")
	if _, err = c.ReplaceNetworkAclEntry(ctx, &ec2.ReplaceNetworkAclEntryInput{
		NetworkAclId: aclID, RuleNumber: https.RuleNumber, Protocol: https.Protocol, RuleAction: https.RuleAction,
		CidrBlock: https.CidrBlock, PortRange: https.PortRange, Egress: aws.Bool(false),
	}); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeNetworkAcls(ctx, &ec2.DescribeNetworkAclsInput{NetworkAclIds: []string{*aclID}})
	if err != nil {
		t.Fatal(err)
	}

	entries := described.NetworkAcls[0].Entries
	if len(entries) != 4 || aws.ToInt32(entries[0].RuleNumber) != 110 || aws.ToString(entries[0].Protocol) != "6" ||
		entries[0].RuleAction != ec2types.RuleActionDeny || aws.ToInt32(entries[0].PortRange.From) != 443 ||
		!aws.ToBool(entries[2].Egress) || aws.ToString(entries[2].Protocol) != "1" ||
		aws.ToInt32(entries[2].IcmpTypeCode.Type) != -1 {
		t.Fatalf("got %+v", entries)
	}

	// The subnet moves to the new ACL, which then cannot be deleted.
	moved, err := c.ReplaceNetworkAclAssociation(ctx, &ec2.ReplaceNetworkAclAssociationInput{
		AssociationId: defaultACL.Associations[0].NetworkAclAssociationId, NetworkAclId: aclID,
	})
	if err != nil {
		t.Fatal(err)
	}

	bySubnet, err := c.DescribeNetworkAcls(ctx, &ec2.DescribeNetworkAclsInput{
		Filters: []ec2types.Filter{{Name: aws.String("association.subnet-id"), Values: []string{subnetID}}},
	})
	if err != nil || len(bySubnet.NetworkAcls) != 1 || *bySubnet.NetworkAcls[0].NetworkAclId != *aclID ||
		*bySubnet.NetworkAcls[0].Associations[0].NetworkAclAssociationId != *moved.NewAssociationId {
		t.Fatalf("got %+v, %v", bySubnet, err)
	}

	_, err = c.DeleteNetworkAcl(ctx, &ec2.DeleteNetworkAclInput{NetworkAclId: aclID})
	code(t, err, "DependencyViolation")
	_, err = c.DeleteNetworkAcl(ctx, &ec2.DeleteNetworkAclInput{NetworkAclId: defaultACL.NetworkAclId})
	code(t, err, "InvalidParameterValue")
	_, err = c.DeleteNetworkAclEntry(ctx, &ec2.DeleteNetworkAclEntryInput{
		NetworkAclId: aclID, RuleNumber: aws.Int32(200), Egress: aws.Bool(false),
	})
	code(t, err, "InvalidNetworkAclEntry.NotFound")
	if _, err = c.DeleteNetworkAclEntry(ctx, &ec2.DeleteNetworkAclEntryInput{
		NetworkAclId: aclID, RuleNumber: aws.Int32(110), Egress: aws.Bool(false),
	}); err != nil {
		t.Fatal(err)
	}

	// Deleting the subnet frees the ACL; a VPC with a custom ACL cannot be
	// deleted.
	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	code(t, err, "DependencyViolation")
	if _, err = c.DeleteNetworkAcl(ctx, &ec2.DeleteNetworkAclInput{NetworkAclId: aclID}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeNetworkAcls(ctx, &ec2.DescribeNetworkAclsInput{
		NetworkAclIds: []string{*defaultACL.NetworkAclId},
	})
	code(t, err, "InvalidNetworkAclID.NotFound")
}

// describeInstance returns an instance.
func describeInstance(t *testing.T, c *ec2.Client, id string) ec2types.Instance {
	t.Helper()
	out, err := c.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}

	return out.Reservations[0].Instances[0]
}

func TestEC2ElasticIPs(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	launched, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), MinCount: aws.Int32(2), MaxCount: aws.Int32(2),
	})
	if err != nil {
		t.Fatal(err)
	}

	first, second := launched.Instances[0].InstanceId, launched.Instances[1].InstanceId
	_, err = c.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeStandard})
	code(t, err, "InvalidParameterValue")
	allocated, err := c.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: ec2types.DomainTypeVpc,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeElasticIp,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("web")}},
		}},
	})
	if err != nil || allocated.Domain != ec2types.DomainTypeVpc || aws.ToString(allocated.PublicIp) == "" {
		t.Fatalf("got %+v, %v", allocated, err)
	}

	ip := aws.ToString(allocated.PublicIp)
	associated, err := c.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: allocated.AllocationId, InstanceId: first,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := describeInstance(t, c, *first); aws.ToString(got.PublicIpAddress) != ip {
		t.Fatalf("got %+v", got)
	}

	// An address in use moves only when asked to, and cannot be released.
	_, err = c.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: allocated.AllocationId, InstanceId: second,
	})
	code(t, err, "Resource.AlreadyAssociated")
	_, err = c.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: allocated.AllocationId})
	code(t, err, "InvalidIPAddress.InUse")

	// It stays with a stopped instance.
	if _, err = c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{*first}}); err != nil {
		t.Fatal(err)
	}

	if got := describeInstance(t, c, *first); aws.ToString(got.PublicIpAddress) != ip {
		t.Fatalf("got %+v", got)
	}

	moved, err := c.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: allocated.AllocationId, InstanceId: second, AllowReassociation: aws.Bool(true),
	})
	if err != nil || aws.ToString(moved.AssociationId) == aws.ToString(associated.AssociationId) {
		t.Fatalf("got %+v, %v", moved, err)
	}

	if got := describeInstance(t, c, *first); got.PublicIpAddress != nil {
		t.Fatalf("stopped instance kept %v", aws.ToString(got.PublicIpAddress))
	}

	addresses, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{*second}}},
	})
	if err != nil || len(addresses.Addresses) != 1 || aws.ToString(addresses.Addresses[0].PublicIp) != ip ||
		aws.ToString(addresses.Addresses[0].PrivateIpAddress) == "" || len(addresses.Addresses[0].Tags) != 1 {
		t.Fatalf("got %+v, %v", addresses, err)
	}

	// Terminating the instance disassociates the address.
	if _, err = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{*second}}); err != nil {
		t.Fatal(err)
	}

	addresses, err = c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{PublicIps: []string{ip}})
	if err != nil || addresses.Addresses[0].AssociationId != nil {
		t.Fatalf("got %+v, %v", addresses, err)
	}

	_, err = c.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{AssociationId: moved.AssociationId})
	code(t, err, "InvalidAssociationID.NotFound")

	// An instance in a VPC without an internet gateway cannot take one.
	vpcID := createVPC(t, c, "10.0.0.0/16")
	isolated, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		SubnetId: aws.String(createSubnet(t, c, vpcID, "10.0.8.0/24")),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: allocated.AllocationId, InstanceId: isolated.Instances[0].InstanceId,
	})
	code(t, err, "Gateway.NotAttached")
	if _, err = c.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: allocated.AllocationId}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{*allocated.AllocationId}})
	code(t, err, "InvalidAllocationID.NotFound")
}

func TestEC2NATGateways(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	vpcID := createVPC(t, c, "10.0.0.0/16")
	public := createSubnet(t, c, vpcID, "10.0.1.0/24")
	private := createSubnet(t, c, vpcID, "10.0.2.0/24")
	address, err := c.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeVpc})
	if err != nil {
		t.Fatal(err)
	}

	in := &ec2.CreateNatGatewayInput{SubnetId: aws.String(public), AllocationId: address.AllocationId}
	_, err = c.CreateNatGateway(ctx, in)
	code(t, err, "Gateway.NotAttached")
	_, err = c.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{SubnetId: aws.String(public)})
	code(t, err, "MissingParameter")

	gateway, err := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: gateway.InternetGateway.InternetGatewayId, VpcId: aws.String(vpcID),
	}); err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateNatGateway(ctx, in)
	if err != nil || created.NatGateway.State != ec2types.NatGatewayStatePending ||
		created.NatGateway.ConnectivityType != ec2types.ConnectivityTypePublic ||
		aws.ToString(created.NatGateway.NatGatewayAddresses[0].PublicIp) != aws.ToString(address.PublicIp) ||
		aws.ToString(created.NatGateway.NatGatewayAddresses[0].PrivateIp) != "10.0.1.4" {
		t.Fatalf("got %+v, %v", created, err)
	}

	natID := created.NatGateway.NatGatewayId
	described, err := c.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
		Filter: []ec2types.Filter{{Name: aws.String("subnet-id"), Values: []string{public}}},
	})
	if err != nil || len(described.NatGateways) != 1 ||
		described.NatGateways[0].State != ec2types.NatGatewayStateAvailable {
		t.Fatalf("got %+v, %v", described, err)
	}

	// The gateway holds its address, which only deleting the gateway frees.
	addresses, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		AllocationIds: []string{*address.AllocationId},
	})
	if err != nil || aws.ToString(addresses.Addresses[0].NetworkInterfaceId) == "" ||
		aws.ToString(addresses.Addresses[0].PrivateIpAddress) != "10.0.1.4" {
		t.Fatalf("got %+v, %v", addresses, err)
	}

	_, err = c.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{
		AssociationId: addresses.Addresses[0].AssociationId,
	})
	code(t, err, "InvalidIPAddress.InUse")
	_, err = c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
		InternetGatewayId: gateway.InternetGateway.InternetGatewayId, VpcId: aws.String(vpcID),
	})
	code(t, err, "DependencyViolation")
	_, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(public)})
	code(t, err, "DependencyViolation")

	// A private subnet routes the internet through it.
	table, err := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId: table.RouteTable.RouteTableId, DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId: natID,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: table.RouteTable.RouteTableId, SubnetId: aws.String(private),
	}); err != nil {
		t.Fatal(err)
	}

	routed := func(state ec2types.RouteState) {
		t.Helper()
		out, describeErr := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
			Filters: []ec2types.Filter{{Name: aws.String("route.nat-gateway-id"), Values: []string{*natID}}},
		})
		if describeErr != nil || len(out.RouteTables) != 1 || out.RouteTables[0].Routes[1].State != state {
			t.Fatalf("got %+v, %v", out, describeErr)
		}
	}
	routed(ec2types.RouteStateActive)

	privateNAT, err := c.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
		SubnetId: aws.String(private), ConnectivityType: ec2types.ConnectivityTypePrivate,
	})
	if err != nil || privateNAT.NatGateway.NatGatewayAddresses[0].PublicIp != nil {
		t.Fatalf("got %+v, %v", privateNAT, err)
	}

	for _, id := range []*string{natID, privateNAT.NatGateway.NatGatewayId} {
		if _, err = c.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: id}); err != nil {
			t.Fatal(err)
		}
	}

	// A deleted gateway stays visible, its routes are black holes, and its
	// address is free.
	routed(ec2types.RouteStateBlackhole)
	described, err = c.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{NatGatewayIds: []string{*natID}})
	if err != nil || described.NatGateways[0].State != ec2types.NatGatewayStateDeleted ||
		described.NatGateways[0].DeleteTime == nil {
		t.Fatalf("got %+v, %v", described, err)
	}

	if _, err = c.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: address.AllocationId}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(public)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{NatGatewayIds: []string{"nat-0"}})
	code(t, err, "NatGatewayNotFound")
}

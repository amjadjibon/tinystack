package integration_test

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func tier1Tags(kind types.ResourceType, name string) []types.TagSpecification {
	return []types.TagSpecification{{ResourceType: kind, Tags: []types.Tag{{Key: new("Name"), Value: new(name)}}}}
}

func TestEC2Tier1DHCPOptions(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v1, v2 := createVPC(t, c, "10.0.0.0/16"), createVPC(t, c, "10.1.0.0/16")
	vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{v1, v2}})
	if err != nil || *vpcs.Vpcs[0].DhcpOptionsId != *vpcs.Vpcs[1].DhcpOptionsId {
		t.Fatalf("Shared default DHCP: %+v %v", vpcs, err)
	}

	defaultID := *vpcs.Vpcs[0].DhcpOptionsId
	custom, err := c.CreateDhcpOptions(
		ctx,
		&ec2.CreateDhcpOptionsInput{DhcpConfigurations: []types.NewDhcpConfiguration{
			{Key: new("domain-name"), Values: []string{"tiny.internal"}},
			{Key: new("domain-name-servers"), Values: []string{"AmazonProvidedDNS"}},
			{Key: new("netbios-node-type"), Values: []string{"2"}},
		}, TagSpecifications: tier1Tags(types.ResourceTypeDhcpOptions, "custom")},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := custom.DhcpOptions.DhcpOptionsId
	if _, err = c.AssociateDhcpOptions(
		ctx,
		&ec2.AssociateDhcpOptionsInput{VpcId: new(v1), DhcpOptionsId: id},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDhcpOptions(ctx, &ec2.DeleteDhcpOptionsInput{DhcpOptionsId: id})
	code(t, err, "DependencyViolation")
	described, err := c.DescribeDhcpOptions(
		ctx,
		&ec2.DescribeDhcpOptionsInput{Filters: []types.Filter{{Name: new("tag:Name"), Values: []string{"custom"}}}},
	)
	if err != nil || len(described.DhcpOptions) != 1 || len(described.DhcpOptions[0].DhcpConfigurations) != 3 {
		t.Fatalf("%+v %v", described, err)
	}

	page, err := c.DescribeDhcpOptions(ctx, &ec2.DescribeDhcpOptionsInput{MaxResults: new(int32(1))})
	if err != nil || len(page.DhcpOptions) != 1 || page.NextToken == nil {
		t.Fatalf("%+v %v", page, err)
	}

	next, err := c.DescribeDhcpOptions(
		ctx,
		&ec2.DescribeDhcpOptionsInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(next.DhcpOptions) != 1 ||
		*next.DhcpOptions[0].DhcpOptionsId == *page.DhcpOptions[0].DhcpOptionsId {
		t.Fatalf("%+v %v", next, err)
	}

	_, err = c.CreateDhcpOptions(
		ctx,
		&ec2.CreateDhcpOptionsInput{
			DhcpConfigurations: []types.NewDhcpConfiguration{{Key: new("netbios-node-type"), Values: []string{"3"}}},
		},
	)
	code(t, err, "InvalidParameterValue")
	if _, err = c.AssociateDhcpOptions(
		ctx,
		&ec2.AssociateDhcpOptionsInput{VpcId: new(v1), DhcpOptionsId: new("default")},
	); err != nil {
		t.Fatal(err)
	}

	restored, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{v1}})
	if err != nil || *restored.Vpcs[0].DhcpOptionsId != defaultID {
		t.Fatalf("%+v %v", restored, err)
	}

	if _, err = c.DeleteDhcpOptions(ctx, &ec2.DeleteDhcpOptionsInput{DhcpOptionsId: id}); err != nil {
		t.Fatal(err)
	}
}

func TestEC2Tier1CIDRsAndEgress(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	created, err := c.CreateVpc(
		ctx,
		&ec2.CreateVpcInput{CidrBlock: new("10.0.0.0/16"), AmazonProvidedIpv6CidrBlock: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := created.Vpc.VpcId
	if *id == "" || *created.Vpc.DhcpOptionsId == "" || *created.Vpc.CidrBlockAssociationSet[0].AssociationId == "" {
		t.Fatal("Incomplete create response", created)
	}

	v6 := created.Vpc.Ipv6CidrBlockAssociationSet[0]
	prefix := netip.MustParsePrefix(*v6.Ipv6CidrBlock)
	sn, err := c.CreateSubnet(
		ctx,
		&ec2.CreateSubnetInput{
			VpcId:         id,
			CidrBlock:     new("10.0.1.0/24"),
			Ipv6CidrBlock: new(netip.PrefixFrom(prefix.Addr(), 64).String()),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.ModifySubnetAttribute(
		ctx,
		&ec2.ModifySubnetAttributeInput{
			SubnetId:                    sn.Subnet.SubnetId,
			AssignIpv6AddressOnCreation: &types.AttributeBooleanValue{Value: new(true)},
		},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.AssociateSubnetCidrBlock(
		ctx,
		&ec2.AssociateSubnetCidrBlockInput{SubnetId: sn.Subnet.SubnetId, Ipv6CidrBlock: new("2001:db8::/64")},
	)
	code(t, err, "InvalidSubnet.Range")
	secondary, err := c.AssociateVpcCidrBlock(
		ctx,
		&ec2.AssociateVpcCidrBlockInput{VpcId: id, CidrBlock: new("10.2.0.0/16")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{VpcId: id, CidrBlock: new("10.2.1.0/24")})
	code(t, err, "InvalidParameterValue")
	_, err = c.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{VpcId: id, CidrBlock: new("192.168.0.0/24")})
	code(t, err, "InvalidParameterValue")
	secondarySubnet := createSubnet(t, c, *id, "10.2.1.0/24")
	_, err = c.DisassociateVpcCidrBlock(
		ctx,
		&ec2.DisassociateVpcCidrBlockInput{AssociationId: secondary.CidrBlockAssociation.AssociationId},
	)
	code(t, err, "DependencyViolation")
	tables, err := c.DescribeRouteTables(
		ctx,
		&ec2.DescribeRouteTablesInput{Filters: []types.Filter{{Name: new("vpc-id"), Values: []string{*id}}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	rt := tables.RouteTables[0].RouteTableId
	if len(tables.RouteTables[0].Routes) != 3 {
		t.Fatal("Missing local routes", tables)
	}

	gateway, err := c.CreateEgressOnlyInternetGateway(
		ctx,
		&ec2.CreateEgressOnlyInternetGatewayInput{
			VpcId:             id,
			TagSpecifications: tier1Tags(types.ResourceTypeEgressOnlyInternetGateway, "egress"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	gID := gateway.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId
	if _, err = c.CreateRoute(
		ctx,
		&ec2.CreateRouteInput{
			RouteTableId:                rt,
			DestinationIpv6CidrBlock:    new("::/0"),
			EgressOnlyInternetGatewayId: gID,
		},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateRoute(
		ctx,
		&ec2.CreateRouteInput{
			RouteTableId:                rt,
			DestinationCidrBlock:        new("0.0.0.0/0"),
			EgressOnlyInternetGatewayId: gID,
		},
	)
	code(t, err, "InvalidParameterValue")
	filtered, err := c.DescribeEgressOnlyInternetGateways(
		ctx,
		&ec2.DescribeEgressOnlyInternetGatewaysInput{
			Filters: []types.Filter{
				{Name: new("tag-key"), Values: []string{"Name"}},
				{Name: new("vpc-id"), Values: []string{*id}},
			},
			MaxResults: new(int32(1)),
		},
	)
	if err != nil || len(filtered.EgressOnlyInternetGateways) != 1 {
		t.Fatalf("%+v %v", filtered, err)
	}

	if _, err = c.DeleteEgressOnlyInternetGateway(
		ctx,
		&ec2.DeleteEgressOnlyInternetGatewayInput{EgressOnlyInternetGatewayId: gID},
	); err != nil {
		t.Fatal(err)
	}

	routes, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{RouteTableIds: []string{*rt}})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, r := range routes.RouteTables[0].Routes {
		if r.DestinationIpv6CidrBlock != nil && *r.DestinationIpv6CidrBlock == "::/0" {
			found = r.State == types.RouteStateBlackhole
		}
	}

	if !found {
		t.Fatal("Deleted target did not blackhole route", routes)
	}

	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: new(secondarySubnet)}); err != nil {
		t.Fatal(err)
	}

	disassociated, err := c.DisassociateVpcCidrBlock(
		ctx,
		&ec2.DisassociateVpcCidrBlockInput{AssociationId: secondary.CidrBlockAssociation.AssociationId},
	)
	if err != nil ||
		disassociated.CidrBlockAssociation.CidrBlockState.State != types.VpcCidrBlockStateCodeDisassociated {
		t.Fatalf("%+v %v", disassociated, err)
	}

	_, err = c.DisassociateVpcCidrBlock(
		ctx,
		&ec2.DisassociateVpcCidrBlockInput{AssociationId: created.Vpc.CidrBlockAssociationSet[0].AssociationId},
	)
	code(t, err, "OperationNotPermitted")
	_, err = c.DisassociateVpcCidrBlock(ctx, &ec2.DisassociateVpcCidrBlockInput{AssociationId: v6.AssociationId})
	code(t, err, "DependencyViolation")
	if _, err = c.DisassociateSubnetCidrBlock(
		ctx,
		&ec2.DisassociateSubnetCidrBlockInput{AssociationId: sn.Subnet.Ipv6CidrBlockAssociationSet[0].AssociationId},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DisassociateVpcCidrBlock(
		ctx,
		&ec2.DisassociateVpcCidrBlockInput{AssociationId: v6.AssociationId},
	); err != nil {
		t.Fatal(err)
	}
}

func TestEC2Tier1NetworkInterfaces(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v := createVPC(t, c, "10.0.0.0/16")
	sn := createSubnet(t, c, v, "10.0.1.0/28")
	ni, err := c.CreateNetworkInterface(
		ctx,
		&ec2.CreateNetworkInterfaceInput{
			SubnetId:                       new(sn),
			SecondaryPrivateIpAddressCount: new(int32(2)),
			TagSpecifications:              tier1Tags(types.ResourceTypeNetworkInterface, "eni"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := ni.NetworkInterface.NetworkInterfaceId
	if len(ni.NetworkInterface.PrivateIpAddresses) != 3 || *ni.NetworkInterface.PrivateIpAddress != "10.0.1.4" {
		t.Fatal(ni)
	}

	_, err = c.CreateNetworkInterface(
		ctx,
		&ec2.CreateNetworkInterfaceInput{SubnetId: new(sn), PrivateIpAddress: ni.NetworkInterface.PrivateIpAddress},
	)
	code(t, err, "InvalidIPAddress.InUse")
	_, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: new(sn)})
	code(t, err, "DependencyViolation")
	before, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{*id}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AssignPrivateIpAddresses(
		ctx,
		&ec2.AssignPrivateIpAddressesInput{
			NetworkInterfaceId: id,
			PrivateIpAddresses: []string{"10.0.1.8", "10.0.1.0"},
		},
	)
	code(t, err, "InvalidParameterValue")
	after, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{*id}},
	)
	if err != nil || !reflect.DeepEqual(before.NetworkInterfaces, after.NetworkInterfaces) {
		t.Fatalf("Rejected assignment changed ENI: %+v %v", after, err)
	}

	assigned, err := c.AssignPrivateIpAddresses(
		ctx,
		&ec2.AssignPrivateIpAddressesInput{NetworkInterfaceId: id, SecondaryPrivateIpAddressCount: new(int32(1))},
	)
	if err != nil || len(assigned.AssignedPrivateIpAddresses) != 1 {
		t.Fatalf("%+v %v", assigned, err)
	}

	if _, err = c.UnassignPrivateIpAddresses(
		ctx,
		&ec2.UnassignPrivateIpAddressesInput{
			NetworkInterfaceId: id,
			PrivateIpAddresses: []string{*assigned.AssignedPrivateIpAddresses[0].PrivateIpAddress},
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ModifyNetworkInterfaceAttribute(
		ctx,
		&ec2.ModifyNetworkInterfaceAttributeInput{
			NetworkInterfaceId: id,
			SourceDestCheck:    &types.AttributeBooleanValue{Value: new(false)},
		},
	); err != nil {
		t.Fatal(err)
	}

	attr, err := c.DescribeNetworkInterfaceAttribute(
		ctx,
		&ec2.DescribeNetworkInterfaceAttributeInput{
			NetworkInterfaceId: id,
			Attribute:          types.NetworkInterfaceAttributeSourceDestCheck,
		},
	)
	if err != nil || *attr.SourceDestCheck.Value {
		t.Fatalf("%+v %v", attr, err)
	}

	if _, err = c.ResetNetworkInterfaceAttribute(
		ctx,
		&ec2.ResetNetworkInterfaceAttributeInput{NetworkInterfaceId: id, SourceDestCheck: new("true")},
	); err != nil {
		t.Fatal(err)
	}

	images, err := c.DescribeImages(
		ctx,
		&ec2.DescribeImagesInput{
			Owners:  []string{"amazon"},
			Filters: []types.Filter{{Name: new("architecture"), Values: []string{"x86_64"}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	launched, err := c.RunInstances(
		ctx,
		&ec2.RunInstancesInput{
			ImageId:      images.Images[0].ImageId,
			InstanceType: types.InstanceTypeT3Micro,
			MinCount:     new(int32(1)),
			MaxCount:     new(int32(1)),
			SubnetId:     new(sn),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	instanceID := launched.Instances[0].InstanceId
	if len(launched.Instances[0].NetworkInterfaces) != 1 {
		t.Fatal("No primary instance ENI", launched)
	}

	attachment, err := c.AttachNetworkInterface(
		ctx,
		&ec2.AttachNetworkInterfaceInput{NetworkInterfaceId: id, InstanceId: instanceID, DeviceIndex: new(int32(1))},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: id})
	code(t, err, "InvalidParameterValue")
	listed, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{
			Filters: []types.Filter{{Name: new("attachment.instance-id"), Values: []string{*instanceID}}},
		},
	)
	if err != nil || len(listed.NetworkInterfaces) != 2 {
		t.Fatalf("%+v %v", listed, err)
	}

	instance, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{*instanceID}})
	if err != nil || len(instance.Reservations[0].Instances[0].NetworkInterfaces) != 2 {
		t.Fatalf("%+v %v", instance, err)
	}

	if _, err = c.DetachNetworkInterface(
		ctx,
		&ec2.DetachNetworkInterfaceInput{AttachmentId: attachment.AttachmentId},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.TerminateInstances(
		ctx,
		&ec2.TerminateInstancesInput{InstanceIds: []string{*instanceID}},
	); err != nil {
		t.Fatal(err)
	}

	filtered, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{Filters: []types.Filter{{Name: new("tag:Name"), Values: []string{"eni"}}}},
	)
	if err != nil || len(filtered.NetworkInterfaces) != 1 {
		t.Fatalf("%+v %v", filtered, err)
	}

	if _, err = c.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: id}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: new(sn)}); err != nil {
		t.Fatal(err)
	}
}

func TestEC2Tier1FlowLogs(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v := createVPC(t, c, "10.0.0.0/16")
	_, err := c.CreateFlowLogs(
		ctx,
		&ec2.CreateFlowLogsInput{
			ResourceIds:  []string{v},
			ResourceType: types.FlowLogsResourceTypeVpc,
			TrafficType:  types.TrafficTypeAll,
			LogGroupName: new("flow"),
		},
	)
	code(t, err, "InvalidParameterValue")
	for _, destination := range []types.LogDestinationType{
		types.LogDestinationTypeCloudWatchLogs,
		types.LogDestinationTypeS3,
		types.LogDestinationTypeKinesisDataFirehose,
	} {
		in := &ec2.CreateFlowLogsInput{
			ResourceIds:        []string{v},
			ResourceType:       types.FlowLogsResourceTypeVpc,
			TrafficType:        types.TrafficTypeAll,
			LogDestinationType: destination,
			TagSpecifications:  tier1Tags(types.ResourceTypeVpcFlowLog, string(destination)),
		}

		switch destination {
		case types.LogDestinationTypeCloudWatchLogs:
			in.LogGroupName, in.DeliverLogsPermissionArn = new("flow"), new("arn:aws:iam::000000000000:role/flow")
		case types.LogDestinationTypeS3:
			in.LogDestination = new("arn:aws:s3:::flow-bucket")
		case types.LogDestinationTypeKinesisDataFirehose:
			in.LogDestination = new("arn:aws:firehose:us-east-1:000000000000:deliverystream/flow")
		}

		out, createFlowLogsErr := c.CreateFlowLogs(ctx, in)
		if createFlowLogsErr != nil || len(out.FlowLogIds) != 1 {
			t.Fatalf("%+v %v", out, createFlowLogsErr)
		}
	}

	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(v)})
	code(t, err, "DependencyViolation")
	page, err := c.DescribeFlowLogs(
		ctx,
		&ec2.DescribeFlowLogsInput{
			MaxResults: new(int32(1)),
			Filter:     []types.Filter{{Name: new("vpc-id"), Values: []string{v}}},
		},
	)
	if err != nil || page.NextToken == nil || len(page.FlowLogs) != 1 {
		t.Fatalf("%+v %v", page, err)
	}

	filtered, err := c.DescribeFlowLogs(
		ctx,
		&ec2.DescribeFlowLogsInput{Filter: []types.Filter{{Name: new("tag:Name"), Values: []string{"s3"}}}},
	)
	if err != nil || len(filtered.FlowLogs) != 1 || filtered.FlowLogs[0].FlowLogStatus == nil ||
		*filtered.FlowLogs[0].FlowLogStatus != "ACTIVE" {
		t.Fatalf("%+v %v", filtered, err)
	}

	all, err := c.DescribeFlowLogs(ctx, &ec2.DescribeFlowLogsInput{})
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, f := range all.FlowLogs {
		ids = append(ids, *f.FlowLogId)
	}

	if _, err = c.DeleteFlowLogs(ctx, &ec2.DeleteFlowLogsInput{FlowLogIds: ids}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(v)}); err != nil {
		t.Fatal(err)
	}
}

func TestEC2Tier1ManagedPrefixLists(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	pl, err := c.CreateManagedPrefixList(ctx, &ec2.CreateManagedPrefixListInput{
		PrefixListName: new("services"),
		AddressFamily:  new("IPv4"),
		MaxEntries:     new(int32(3)),
		Entries: []types.AddPrefixListEntry{
			{Cidr: new("10.0.0.0/24"), Description: new("original")},
		},
		TagSpecifications: tier1Tags(types.ResourceTypePrefixList, "services"),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := pl.PrefixList.PrefixListId
	g, err := c.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{GroupName: new("prefix-test"), Description: new("prefix test")},
	)
	if err != nil {
		t.Fatal(err)
	}

	rule, err := c.AuthorizeSecurityGroupIngress(
		ctx,
		&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: g.GroupId,
			IpPermissions: []types.IpPermission{
				{
					IpProtocol:    new("tcp"),
					FromPort:      new(int32(443)),
					ToPort:        new(int32(443)),
					PrefixListIds: []types.PrefixListId{{PrefixListId: id}},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	associations, err := c.GetManagedPrefixListAssociations(
		ctx,
		&ec2.GetManagedPrefixListAssociationsInput{PrefixListId: id},
	)
	if err != nil || len(associations.PrefixListAssociations) != 1 ||
		*associations.PrefixListAssociations[0].ResourceId != *g.GroupId {
		t.Fatalf("%+v %v", associations, err)
	}

	_, err = c.DeleteManagedPrefixList(ctx, &ec2.DeleteManagedPrefixListInput{PrefixListId: id})
	code(t, err, "DependencyViolation")
	updated, err := c.ModifyManagedPrefixList(
		ctx,
		&ec2.ModifyManagedPrefixListInput{
			PrefixListId:   id,
			CurrentVersion: new(int64(1)),
			AddEntries:     []types.AddPrefixListEntry{{Cidr: new("10.1.0.0/24")}},
		},
	)
	if err != nil || *updated.PrefixList.Version != 2 {
		t.Fatalf("%+v %v", updated, err)
	}

	_, err = c.ModifyManagedPrefixList(
		ctx,
		&ec2.ModifyManagedPrefixListInput{
			PrefixListId:   id,
			CurrentVersion: new(int64(1)),
			RemoveEntries:  []types.RemovePrefixListEntry{{Cidr: new("10.0.0.0/24")}},
		},
	)
	code(t, err, "PrefixListVersionMismatch")
	_, err = c.ModifyManagedPrefixList(
		ctx,
		&ec2.ModifyManagedPrefixListInput{PrefixListId: id, MaxEntries: new(int32(1))},
	)
	code(t, err, "InvalidParameterValue")
	historical, err := c.GetManagedPrefixListEntries(
		ctx,
		&ec2.GetManagedPrefixListEntriesInput{PrefixListId: id, TargetVersion: new(int64(1))},
	)
	if err != nil || len(historical.Entries) != 1 || *historical.Entries[0].Description != "original" {
		t.Fatalf("%+v %v", historical, err)
	}

	page, err := c.GetManagedPrefixListEntries(
		ctx,
		&ec2.GetManagedPrefixListEntriesInput{PrefixListId: id, MaxResults: new(int32(1))},
	)
	if err != nil || page.NextToken == nil || len(page.Entries) != 1 {
		t.Fatalf("%+v %v", page, err)
	}

	filtered, err := c.DescribeManagedPrefixLists(
		ctx,
		&ec2.DescribeManagedPrefixListsInput{
			Filters: []types.Filter{{Name: new("tag:Name"), Values: []string{"services"}}},
		},
	)
	if err != nil || len(filtered.PrefixLists) != 1 || *filtered.PrefixLists[0].Version != 2 {
		t.Fatalf("%+v %v", filtered, err)
	}

	if _, err = c.RevokeSecurityGroupIngress(
		ctx,
		&ec2.RevokeSecurityGroupIngressInput{
			GroupId:              g.GroupId,
			SecurityGroupRuleIds: []string{*rule.SecurityGroupRules[0].SecurityGroupRuleId},
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteManagedPrefixList(ctx, &ec2.DeleteManagedPrefixListInput{PrefixListId: id}); err != nil {
		t.Fatal(err)
	}
}

func TestEC2Tier1PeeringAndRuleUpdates(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v1, v2 := createVPC(t, c, "10.0.0.0/16"), createVPC(t, c, "10.1.0.0/16")
	_, err := c.CreateVpcPeeringConnection(
		ctx,
		&ec2.CreateVpcPeeringConnectionInput{VpcId: new(v1), PeerVpcId: new(v2), PeerRegion: new("eu-west-1")},
	)
	code(t, err, "NotImplemented")
	overlap := createVPC(t, c, "10.0.1.0/24")
	_, err = c.CreateVpcPeeringConnection(
		ctx,
		&ec2.CreateVpcPeeringConnectionInput{VpcId: new(v1), PeerVpcId: new(overlap)},
	)
	code(t, err, "InvalidParameterValue")
	peer, err := c.CreateVpcPeeringConnection(
		ctx,
		&ec2.CreateVpcPeeringConnectionInput{
			VpcId:             new(v1),
			PeerVpcId:         new(v2),
			TagSpecifications: tier1Tags(types.ResourceTypeVpcPeeringConnection, "peer"),
		},
	)
	if err != nil ||
		peer.VpcPeeringConnection.Status.Code != types.VpcPeeringConnectionStateReasonCodePendingAcceptance {
		t.Fatalf("%+v %v", peer, err)
	}

	id := peer.VpcPeeringConnection.VpcPeeringConnectionId
	_, err = c.AssociateVpcCidrBlock(
		ctx,
		&ec2.AssociateVpcCidrBlockInput{VpcId: new(v1), CidrBlock: new("10.3.0.0/16")},
	)
	code(t, err, "OperationNotPermitted")
	if _, err = c.AcceptVpcPeeringConnection(
		ctx,
		&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: id},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ModifyVpcPeeringConnectionOptions(
		ctx,
		&ec2.ModifyVpcPeeringConnectionOptionsInput{
			VpcPeeringConnectionId: id,
			RequesterPeeringConnectionOptions: &types.PeeringConnectionOptionsRequest{
				AllowDnsResolutionFromRemoteVpc: new(true),
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(v2)})
	code(t, err, "DependencyViolation")
	filtered, err := c.DescribeVpcPeeringConnections(
		ctx,
		&ec2.DescribeVpcPeeringConnectionsInput{
			Filters:    []types.Filter{{Name: new("tag:Name"), Values: []string{"peer"}}},
			MaxResults: new(int32(1)),
		},
	)
	if err != nil || len(filtered.VpcPeeringConnections) != 1 ||
		!*filtered.VpcPeeringConnections[0].RequesterVpcInfo.PeeringOptions.AllowDnsResolutionFromRemoteVpc {
		t.Fatalf("%+v %v", filtered, err)
	}

	g1, err := c.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{VpcId: new(v1), GroupName: new("one"), Description: new("one")},
	)
	if err != nil {
		t.Fatal(err)
	}

	g2, err := c.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{VpcId: new(v2), GroupName: new("two"), Description: new("two")},
	)
	if err != nil {
		t.Fatal(err)
	}

	granted, err := c.AuthorizeSecurityGroupIngress(
		ctx,
		&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: g1.GroupId,
			IpPermissions: []types.IpPermission{
				{
					IpProtocol:       new("tcp"),
					FromPort:         new(int32(80)),
					ToPort:           new(int32(80)),
					UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: g2.GroupId}},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ruleID := granted.SecurityGroupRules[0].SecurityGroupRuleId
	refs, err := c.DescribeSecurityGroupReferences(
		ctx,
		&ec2.DescribeSecurityGroupReferencesInput{GroupId: []string{*g2.GroupId}},
	)
	if err != nil || len(refs.SecurityGroupReferenceSet) != 1 ||
		*refs.SecurityGroupReferenceSet[0].VpcPeeringConnectionId != *id {
		t.Fatalf("%+v %v", refs, err)
	}

	if _, err = c.ModifySecurityGroupRules(
		ctx,
		&ec2.ModifySecurityGroupRulesInput{
			GroupId: g1.GroupId,
			SecurityGroupRules: []types.SecurityGroupRuleUpdate{
				{
					SecurityGroupRuleId: ruleID,
					SecurityGroupRule: &types.SecurityGroupRuleRequest{
						FromPort:    new(int32(443)),
						ToPort:      new(int32(443)),
						Description: new("TLS"),
					},
				},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	before, err := c.DescribeSecurityGroupRules(
		ctx,
		&ec2.DescribeSecurityGroupRulesInput{SecurityGroupRuleIds: []string{*ruleID}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifySecurityGroupRules(
		ctx,
		&ec2.ModifySecurityGroupRulesInput{GroupId: g1.GroupId, SecurityGroupRules: []types.SecurityGroupRuleUpdate{
			{
				SecurityGroupRuleId: ruleID,
				SecurityGroupRule:   &types.SecurityGroupRuleRequest{Description: new("changed")},
			},
			{
				SecurityGroupRuleId: new("sgr-missing"),
				SecurityGroupRule:   &types.SecurityGroupRuleRequest{Description: new("fail")},
			},
		}},
	)
	code(t, err, "InvalidSecurityGroupRuleId.NotFound")
	after, err := c.DescribeSecurityGroupRules(
		ctx,
		&ec2.DescribeSecurityGroupRulesInput{SecurityGroupRuleIds: []string{*ruleID}},
	)
	if err != nil || !reflect.DeepEqual(before.SecurityGroupRules, after.SecurityGroupRules) {
		t.Fatalf("Rejected rule update changed state: %+v %v", after, err)
	}

	if _, err = c.UpdateSecurityGroupRuleDescriptionsIngress(
		ctx,
		&ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
			GroupId: g1.GroupId,
			SecurityGroupRuleDescriptions: []types.SecurityGroupRuleDescription{
				{SecurityGroupRuleId: ruleID, Description: new("updated")},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteVpcPeeringConnection(
		ctx,
		&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: id},
	); err != nil {
		t.Fatal(err)
	}

	stale, err := c.DescribeStaleSecurityGroups(ctx, &ec2.DescribeStaleSecurityGroupsInput{VpcId: new(v1)})
	if err != nil || len(stale.StaleSecurityGroupSet) != 1 ||
		len(stale.StaleSecurityGroupSet[0].StaleIpPermissions) != 1 {
		t.Fatalf("%+v %v", stale, err)
	}
}

func TestEC2Tier1DefaultSubnetAndEndpointServices(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateDefaultVpc(ctx, &ec2.CreateDefaultVpcInput{})
	code(t, err, "DefaultVpcAlreadyExists")
	subnets, err := c.DescribeSubnets(
		ctx,
		&ec2.DescribeSubnetsInput{Filters: []types.Filter{{Name: new("default-for-az"), Values: []string{"true"}}}},
	)
	if err != nil || len(subnets.Subnets) != 3 {
		t.Fatalf("%+v %v", subnets, err)
	}

	sn := subnets.Subnets[0]
	_, err = c.CreateDefaultSubnet(ctx, &ec2.CreateDefaultSubnetInput{AvailabilityZone: sn.AvailabilityZone})
	code(t, err, "DefaultSubnetAlreadyExistsInAvailabilityZone")
	if _, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.SubnetId}); err != nil {
		t.Fatal(err)
	}

	replacement, err := c.CreateDefaultSubnet(ctx, &ec2.CreateDefaultSubnetInput{AvailabilityZone: sn.AvailabilityZone})
	if err != nil || !*replacement.Subnet.DefaultForAz {
		t.Fatalf("%+v %v", replacement, err)
	}

	services, err := c.DescribeVpcEndpointServices(
		ctx,
		&ec2.DescribeVpcEndpointServicesInput{ServiceNames: []string{"com.amazonaws.us-east-1.s3"}},
	)
	if err != nil || len(services.ServiceDetails) != 1 ||
		services.ServiceDetails[0].ServiceType[0].ServiceType != types.ServiceTypeGateway {
		t.Fatalf("%+v %v", services, err)
	}

	page, err := c.DescribeVpcEndpointServices(
		ctx,
		&ec2.DescribeVpcEndpointServicesInput{
			MaxResults: new(int32(1)),
			Filters:    []types.Filter{{Name: new("service-type"), Values: []string{"Interface"}}},
		},
	)
	if err != nil || len(page.ServiceDetails) != 1 || page.NextToken == nil {
		t.Fatalf("%+v %v", page, err)
	}

	next, err := c.DescribeVpcEndpointServices(
		ctx,
		&ec2.DescribeVpcEndpointServicesInput{
			MaxResults: new(int32(1)),
			NextToken:  page.NextToken,
			Filters:    []types.Filter{{Name: new("service-type"), Values: []string{"Interface"}}},
		},
	)
	if err != nil || *page.ServiceDetails[0].ServiceName == *next.ServiceDetails[0].ServiceName {
		t.Fatalf("%+v %v", next, err)
	}

	_, err = c.DescribeVpcEndpointServices(
		ctx,
		&ec2.DescribeVpcEndpointServicesInput{ServiceNames: []string{"com.amazonaws.us-east-1.unknown"}},
	)
	code(t, err, "InvalidParameterValue")
}

func TestEC2Tier1ListPaginationAndTagging(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v := createVPC(t, c, "10.0.0.0/16")
	sn := createSubnet(t, c, v, "10.0.0.0/24")
	for i := range 6 {
		ni, err := c.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{SubnetId: new(sn)})
		if err != nil {
			t.Fatal(err)
		}

		if _, err = c.CreateTags(
			ctx,
			&ec2.CreateTagsInput{
				Resources: []string{*ni.NetworkInterface.NetworkInterfaceId},
				Tags:      []types.Tag{{Key: new("Name"), Value: new(fmt.Sprint(i))}},
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{
			MaxResults: new(int32(5)),
			Filters: []types.Filter{
				{Name: new("tag-key"), Values: []string{"Name"}},
				{Name: new("vpc-id"), Values: []string{v}},
			},
		},
	)
	if err != nil || first.NextToken == nil || len(first.NetworkInterfaces) != 5 {
		t.Fatalf("%+v %v", first, err)
	}

	second, err := c.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{
			MaxResults: new(int32(5)),
			NextToken:  first.NextToken,
			Filters: []types.Filter{
				{Name: new("tag-key"), Values: []string{"Name"}},
				{Name: new("vpc-id"), Values: []string{v}},
			},
		},
	)
	if err != nil || len(second.NetworkInterfaces) != 1 || second.NextToken != nil {
		t.Fatalf("%+v %v", second, err)
	}

	_, err = c.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{NextToken: new("bad%%%")})
	code(t, err, "InvalidParameterValue")
	tags, err := c.DescribeTags(
		ctx,
		&ec2.DescribeTagsInput{
			Filters: []types.Filter{{Name: new("resource-type"), Values: []string{"network-interface"}}},
		},
	)
	if err != nil || len(tags.Tags) != 6 || !strings.HasPrefix(*tags.Tags[0].ResourceId, "eni-") {
		t.Fatalf("%+v %v", tags, err)
	}
}

func TestEC2Tier1CrossAccountPeering(t *testing.T) {
	s := server(t)
	a, b := ec2Client(s, "test", "us-east-1"), ec2Client(s, "team-b", "us-east-1")
	ctx := context.Background()
	v1, v2 := createVPC(t, a, "10.0.0.0/16"), createVPC(t, b, "10.1.0.0/16")
	_, err := a.CreateVpcPeeringConnection(
		ctx,
		&ec2.CreateVpcPeeringConnectionInput{VpcId: new(v1), PeerVpcId: new(v2), PeerOwnerId: new("222222222222")},
	)
	code(t, err, "NotImplemented")
	created, err := a.CreateVpcPeeringConnection(
		ctx,
		&ec2.CreateVpcPeeringConnectionInput{
			VpcId:             new(v1),
			PeerVpcId:         new(v2),
			PeerOwnerId:       new("111111111111"),
			TagSpecifications: tier1Tags(types.ResourceTypeVpcPeeringConnection, "requester"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	id := created.VpcPeeringConnection.VpcPeeringConnectionId
	peer, err := b.DescribeVpcPeeringConnections(
		ctx,
		&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: []string{*id}},
	)
	if err != nil || len(peer.VpcPeeringConnections) != 1 ||
		peer.VpcPeeringConnections[0].Status.Code != types.VpcPeeringConnectionStateReasonCodePendingAcceptance {
		t.Fatalf("%+v %v", peer, err)
	}
	_, err = a.AcceptVpcPeeringConnection(ctx, &ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: id})
	code(t, err, "OperationNotPermitted")
	if _, err = b.CreateTags(
		ctx,
		&ec2.CreateTagsInput{Resources: []string{*id}, Tags: []types.Tag{{Key: new("Name"), Value: new("accepter")}}},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = b.AcceptVpcPeeringConnection(
		ctx,
		&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: id},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ModifyVpcPeeringConnectionOptions(
		ctx,
		&ec2.ModifyVpcPeeringConnectionOptionsInput{
			VpcPeeringConnectionId: id,
			AccepterPeeringConnectionOptions: &types.PeeringConnectionOptionsRequest{
				AllowDnsResolutionFromRemoteVpc: new(true),
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	for _, participant := range []struct {
		client *ec2.Client
		name   string
	}{{a, "requester"}, {b, "accepter"}} {
		visible, readErr := participant.client.DescribeVpcPeeringConnections(
			ctx,
			&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: []string{*id}},
		)
		if readErr != nil || *visible.VpcPeeringConnections[0].Tags[0].Value != participant.name ||
			!*visible.VpcPeeringConnections[0].AccepterVpcInfo.PeeringOptions.AllowDnsResolutionFromRemoteVpc {
			t.Fatalf("%+v %v", visible, readErr)
		}
	}
	if _, err = b.DeleteVpcPeeringConnection(
		ctx,
		&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: id},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(v1)}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: new(v2)}); err != nil {
		t.Fatal(err)
	}
}

func TestEC2Tier1GatewayAndPeeringPagination(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v1, v2, v3 := createVPC(t, c, "10.0.0.0/16"), createVPC(t, c, "10.1.0.0/16"), createVPC(t, c, "10.2.0.0/16")
	for _, v := range []string{v1, v2} {
		if _, err := c.CreateEgressOnlyInternetGateway(
			ctx,
			&ec2.CreateEgressOnlyInternetGatewayInput{VpcId: new(v)},
		); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.DescribeEgressOnlyInternetGateways(
		ctx,
		&ec2.DescribeEgressOnlyInternetGatewaysInput{MaxResults: new(int32(1))},
	)
	if err != nil || first.NextToken == nil || len(first.EgressOnlyInternetGateways) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	next, err := c.DescribeEgressOnlyInternetGateways(
		ctx,
		&ec2.DescribeEgressOnlyInternetGatewaysInput{MaxResults: new(int32(1)), NextToken: first.NextToken},
	)
	if err != nil || next.NextToken != nil || len(next.EgressOnlyInternetGateways) != 1 ||
		*next.EgressOnlyInternetGateways[0].EgressOnlyInternetGatewayId ==
			*first.EgressOnlyInternetGateways[0].EgressOnlyInternetGatewayId {
		t.Fatalf("%+v %v", next, err)
	}
	for _, peer := range []string{v2, v3} {
		if _, err = c.CreateVpcPeeringConnection(
			ctx,
			&ec2.CreateVpcPeeringConnectionInput{VpcId: new(v1), PeerVpcId: new(peer)},
		); err != nil {
			t.Fatal(err)
		}
	}
	peers, err := c.DescribeVpcPeeringConnections(
		ctx,
		&ec2.DescribeVpcPeeringConnectionsInput{MaxResults: new(int32(1))},
	)
	if err != nil || peers.NextToken == nil || len(peers.VpcPeeringConnections) != 1 {
		t.Fatalf("%+v %v", peers, err)
	}
	second, err := c.DescribeVpcPeeringConnections(
		ctx,
		&ec2.DescribeVpcPeeringConnectionsInput{MaxResults: new(int32(1)), NextToken: peers.NextToken},
	)
	if err != nil || second.NextToken != nil || len(second.VpcPeeringConnections) != 1 {
		t.Fatalf("%+v %v", second, err)
	}
	if _, err = c.RejectVpcPeeringConnection(
		ctx,
		&ec2.RejectVpcPeeringConnectionInput{
			VpcPeeringConnectionId: second.VpcPeeringConnections[0].VpcPeeringConnectionId,
		},
	); err != nil {
		t.Fatal(err)
	}
	rejected, err := c.DescribeVpcPeeringConnections(
		ctx,
		&ec2.DescribeVpcPeeringConnectionsInput{
			VpcPeeringConnectionIds: []string{*second.VpcPeeringConnections[0].VpcPeeringConnectionId},
		},
	)
	if err != nil ||
		rejected.VpcPeeringConnections[0].Status.Code != types.VpcPeeringConnectionStateReasonCodeRejected {
		t.Fatalf("%+v %v", rejected, err)
	}
	_, err = c.AcceptVpcPeeringConnection(
		ctx,
		&ec2.AcceptVpcPeeringConnectionInput{
			VpcPeeringConnectionId: second.VpcPeeringConnections[0].VpcPeeringConnectionId,
		},
	)
	code(t, err, "InvalidStateTransition")
}

func TestEC2Tier1ExplicitIPv6PoolContract(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateVpc(
		ctx,
		&ec2.CreateVpcInput{CidrBlock: new("10.0.0.0/16"), Ipv6CidrBlock: new("2001:db8:1234:1a00::/56")},
	)
	code(t, err, "MissingParameter")
	created, err := c.CreateVpc(
		ctx,
		&ec2.CreateVpcInput{
			CidrBlock:     new("10.0.0.0/16"),
			Ipv6CidrBlock: new("2001:db8:1234:1a00::/56"),
			Ipv6Pool:      new("ipv6pool-external"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	a := created.Vpc.Ipv6CidrBlockAssociationSet[0]
	if *a.Ipv6Pool != "ipv6pool-external" || a.IpSource != types.IpSourceByoip {
		t.Fatal(a)
	}
	sn, err := c.CreateSubnet(
		ctx,
		&ec2.CreateSubnetInput{
			VpcId:         created.Vpc.VpcId,
			CidrBlock:     new("10.0.0.0/24"),
			Ipv6CidrBlock: new("2001:db8:1234:1a00::/64"),
		},
	)
	if err != nil || sn.Subnet.Ipv6CidrBlockAssociationSet[0].IpSource != types.IpSourceByoip {
		t.Fatalf("%+v %v", sn, err)
	}
	_, err = c.AssociateVpcCidrBlock(
		ctx,
		&ec2.AssociateVpcCidrBlockInput{VpcId: created.Vpc.VpcId, Ipv6CidrBlock: new("2001:db8:1234:1b00::/56")},
	)
	code(t, err, "MissingParameter")
	associated, err := c.AssociateVpcCidrBlock(
		ctx,
		&ec2.AssociateVpcCidrBlockInput{
			VpcId:         created.Vpc.VpcId,
			Ipv6CidrBlock: new("2001:db8:1234:1b00::/56"),
			Ipv6Pool:      new("ipv6pool-external"),
		},
	)
	if err != nil || *associated.Ipv6CidrBlockAssociation.Ipv6Pool != "ipv6pool-external" {
		t.Fatalf("%+v %v", associated, err)
	}
}

func TestEC2Tier1FlowLogValidationAndPartialFailures(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	v := createVPC(t, c, "10.0.0.0/16")
	for _, format := range []string{"", "  ", "garbage", "${unknown}"} {
		_, err := c.CreateFlowLogs(ctx, &ec2.CreateFlowLogsInput{
			ResourceIds:        []string{v},
			ResourceType:       types.FlowLogsResourceTypeVpc,
			TrafficType:        types.TrafficTypeAll,
			LogDestinationType: types.LogDestinationTypeS3,
			LogDestination:     new("arn:aws:s3:::flows"),
			LogFormat:          new(format),
		})
		code(t, err, "InvalidParameterValue")
	}
	out, err := c.CreateFlowLogs(ctx, &ec2.CreateFlowLogsInput{
		ResourceIds:        []string{v, "vpc-missing"},
		ResourceType:       types.FlowLogsResourceTypeVpc,
		TrafficType:        types.TrafficTypeAll,
		LogDestinationType: types.LogDestinationTypeS3,
		LogDestination:     new("arn:aws:s3:::flows"),
		LogFormat:          new("${version} ${vpc-id}"),
	})
	if err != nil || len(out.FlowLogIds) != 1 || len(out.Unsuccessful) != 1 ||
		*out.Unsuccessful[0].ResourceId != "vpc-missing" {
		t.Fatalf("%+v %v", out, err)
	}
	listed, err := c.DescribeFlowLogs(ctx, &ec2.DescribeFlowLogsInput{FlowLogIds: out.FlowLogIds})
	if err != nil || *listed.FlowLogs[0].LogFormat != "${version} ${vpc-id}" {
		t.Fatalf("%+v %v", listed, err)
	}
}

func TestEC2Tier1EgressRuleDescriptions(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	g, err := c.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{GroupName: new("descriptions"), Description: new("descriptions")},
	)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.DescribeSecurityGroupRules(
		ctx,
		&ec2.DescribeSecurityGroupRulesInput{
			Filters: []types.Filter{{Name: new("group-id"), Values: []string{*g.GroupId}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	id := all.SecurityGroupRules[0].SecurityGroupRuleId
	if _, err = c.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: g.GroupId,
		SecurityGroupRuleDescriptions: []types.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: id, Description: new("egress")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := c.DescribeSecurityGroupRules(
		ctx,
		&ec2.DescribeSecurityGroupRulesInput{SecurityGroupRuleIds: []string{*id}},
	)
	if err != nil || *updated.SecurityGroupRules[0].Description != "egress" {
		t.Fatalf("%+v %v", updated, err)
	}
	if _, err = c.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: g.GroupId,
		IpPermissions: []types.IpPermission{
			{IpProtocol: new("-1"), IpRanges: []types.IpRange{{CidrIp: new("0.0.0.0/0")}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cleared, err := c.DescribeSecurityGroupRules(
		ctx,
		&ec2.DescribeSecurityGroupRulesInput{SecurityGroupRuleIds: []string{*id}},
	)
	if err != nil || cleared.SecurityGroupRules[0].Description != nil {
		t.Fatalf("%+v %v", cleared, err)
	}
}

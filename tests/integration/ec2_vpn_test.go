package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestEC2VPNLifecycle(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()

	// 1. Customer Gateway
	cgwRes, err := c.CreateCustomerGateway(ctx, &ec2.CreateCustomerGatewayInput{
		BgpAsn:    new(int32(65000)),
		IpAddress: new("203.0.113.1"),
		Type:      types.GatewayTypeIpsec1,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeCustomerGateway,
				Tags: []types.Tag{
					{Key: new("Name"), Value: new("my-cgw")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateCustomerGateway failed: %v", err)
	}

	cgwID := *cgwRes.CustomerGateway.CustomerGatewayId
	descCgw, err := c.DescribeCustomerGateways(ctx, &ec2.DescribeCustomerGatewaysInput{
		CustomerGatewayIds: []string{cgwID},
		Filters: []types.Filter{
			{Name: new("tag:Name"), Values: []string{"my-cgw"}},
			{Name: new("ip-address"), Values: []string{"203.0.113.1"}},
		},
	})
	if err != nil || len(descCgw.CustomerGateways) != 1 {
		t.Fatalf("DescribeCustomerGateways failed: %v, count: %d", err, len(descCgw.CustomerGateways))
	}

	// 2. VPN Gateway
	vgwRes, err := c.CreateVpnGateway(ctx, &ec2.CreateVpnGatewayInput{
		AmazonSideAsn: new(int64(64512)),
		Type:          types.GatewayTypeIpsec1,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVpnGateway,
				Tags: []types.Tag{
					{Key: new("Name"), Value: new("my-vgw")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVpnGateway failed: %v", err)
	}

	vgwID := *vgwRes.VpnGateway.VpnGatewayId
	descVgw, err := c.DescribeVpnGateways(ctx, &ec2.DescribeVpnGatewaysInput{
		VpnGatewayIds: []string{vgwID},
		Filters: []types.Filter{
			{Name: new("tag:Name"), Values: []string{"my-vgw"}},
		},
	})
	if err != nil || len(descVgw.VpnGateways) != 1 {
		t.Fatalf("DescribeVpnGateways failed: %v", err)
	}

	// 3. Attach VPN Gateway to VPC
	vpcID := createVPC(t, c, "10.0.0.0/16")
	attachRes, err := c.AttachVpnGateway(ctx, &ec2.AttachVpnGatewayInput{
		VpnGatewayId: new(vgwID),
		VpcId:        new(vpcID),
	})
	if err != nil {
		t.Fatalf("AttachVpnGateway failed: %v", err)
	}

	if *attachRes.VpcAttachment.VpcId != vpcID || attachRes.VpcAttachment.State != types.AttachmentStatusAttached {
		t.Fatalf("Unexpected VpcAttachment: %+v", attachRes.VpcAttachment)
	}

	// 4. Route Propagation on Route Table
	rtbRes, err := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: new(vpcID),
	})
	if err != nil {
		t.Fatalf("CreateRouteTable failed: %v", err)
	}

	rtbID := *rtbRes.RouteTable.RouteTableId
	_, err = c.EnableVgwRoutePropagation(ctx, &ec2.EnableVgwRoutePropagationInput{
		RouteTableId: new(rtbID),
		GatewayId:    new(vgwID),
	})
	if err != nil {
		t.Fatalf("EnableVgwRoutePropagation failed: %v", err)
	}

	descRtbs, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		RouteTableIds: []string{rtbID},
	})
	if err != nil || len(descRtbs.RouteTables) != 1 || len(descRtbs.RouteTables[0].PropagatingVgws) != 1 {
		t.Fatalf("DescribeRouteTables propagating VGWs failed: %v", err)
	}

	_, err = c.DisableVgwRoutePropagation(ctx, &ec2.DisableVgwRoutePropagationInput{
		RouteTableId: new(rtbID),
		GatewayId:    new(vgwID),
	})
	if err != nil {
		t.Fatalf("DisableVgwRoutePropagation failed: %v", err)
	}

	// 5. VPN Connection
	vpnRes, err := c.CreateVpnConnection(ctx, &ec2.CreateVpnConnectionInput{
		CustomerGatewayId: new(cgwID),
		VpnGatewayId:      new(vgwID),
		Type:              new("ipsec.1"),
		Options: &types.VpnConnectionOptionsSpecification{
			StaticRoutesOnly: new(true),
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeVpnConnection,
				Tags: []types.Tag{
					{Key: new("Name"), Value: new("my-vpn")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVpnConnection failed: %v", err)
	}

	vpnID := *vpnRes.VpnConnection.VpnConnectionId

	// Describe VPN Connection
	descVpn, err := c.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{
		VpnConnectionIds: []string{vpnID},
		Filters: []types.Filter{
			{Name: new("tag:Name"), Values: []string{"my-vpn"}},
		},
	})
	if err != nil || len(descVpn.VpnConnections) != 1 {
		t.Fatalf("DescribeVpnConnections failed: %v", err)
	}

	// VPN Connection Route
	_, err = c.CreateVpnConnectionRoute(ctx, &ec2.CreateVpnConnectionRouteInput{
		VpnConnectionId:      new(vpnID),
		DestinationCidrBlock: new("192.168.10.0/24"),
	})
	if err != nil {
		t.Fatalf("CreateVpnConnectionRoute failed: %v", err)
	}

	_, err = c.DeleteVpnConnectionRoute(ctx, &ec2.DeleteVpnConnectionRouteInput{
		VpnConnectionId:      new(vpnID),
		DestinationCidrBlock: new("192.168.10.0/24"),
	})
	if err != nil {
		t.Fatalf("DeleteVpnConnectionRoute failed: %v", err)
	}

	// 6. Delete VPN Connection
	_, err = c.DeleteVpnConnection(ctx, &ec2.DeleteVpnConnectionInput{
		VpnConnectionId: new(vpnID),
	})
	if err != nil {
		t.Fatalf("DeleteVpnConnection failed: %v", err)
	}

	// 7. Detach & Delete VPN Gateway
	_, err = c.DetachVpnGateway(ctx, &ec2.DetachVpnGatewayInput{
		VpnGatewayId: new(vgwID),
		VpcId:        new(vpcID),
	})
	if err != nil {
		t.Fatalf("DetachVpnGateway failed: %v", err)
	}

	_, err = c.DeleteVpnGateway(ctx, &ec2.DeleteVpnGatewayInput{
		VpnGatewayId: new(vgwID),
	})
	if err != nil {
		t.Fatalf("DeleteVpnGateway failed: %v", err)
	}

	// 8. Delete Customer Gateway
	_, err = c.DeleteCustomerGateway(ctx, &ec2.DeleteCustomerGatewayInput{
		CustomerGatewayId: new(cgwID),
	})
	if err != nil {
		t.Fatalf("DeleteCustomerGateway failed: %v", err)
	}
}

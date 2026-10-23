package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

func requireEC2Code(t *testing.T, err error, code string) {
	t.Helper()
	var apiError smithy.APIError
	if !errors.As(err, &apiError) || apiError.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestEC2TransitGatewayConsistency(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	create := func() *types.TransitGateway {
		t.Helper()
		out, err := c.CreateTransitGateway(
			ctx,
			&ec2.CreateTransitGatewayInput{Options: &types.TransitGatewayRequestOptions{
				DefaultRouteTableAssociation: types.DefaultRouteTableAssociationValueDisable,
				DefaultRouteTablePropagation: types.DefaultRouteTablePropagationValueDisable,
			}},
		)
		if err != nil {
			t.Fatal(err)
		}

		return out.TransitGateway
	}
	a, b := create(), create()
	table, err := c.CreateTransitGatewayRouteTable(
		ctx,
		&ec2.CreateTransitGatewayRouteTableInput{TransitGatewayId: b.TransitGatewayId},
	)
	if err != nil {
		t.Fatal(err)
	}

	tableID := table.TransitGatewayRouteTable.TransitGatewayRouteTableId
	vpc := createVPC(t, c, "10.30.0.0/16")
	subnet := createSubnet(t, c, vpc, "10.30.1.0/24")
	attachment, err := c.CreateTransitGatewayVpcAttachment(ctx, &ec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: a.TransitGatewayId, VpcId: new(vpc), SubnetIds: []string{subnet},
	})
	if err != nil {
		t.Fatal(err)
	}

	attachmentID := attachment.TransitGatewayVpcAttachment.TransitGatewayAttachmentId
	_, err = c.AssociateTransitGatewayRouteTable(ctx, &ec2.AssociateTransitGatewayRouteTableInput{
		TransitGatewayAttachmentId: attachmentID, TransitGatewayRouteTableId: tableID,
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	_, err = c.EnableTransitGatewayRouteTablePropagation(ctx, &ec2.EnableTransitGatewayRouteTablePropagationInput{
		TransitGatewayAttachmentId: attachmentID, TransitGatewayRouteTableId: tableID,
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	_, err = c.CreateTransitGatewayRoute(ctx, &ec2.CreateTransitGatewayRouteInput{
		TransitGatewayAttachmentId: attachmentID,
		TransitGatewayRouteTableId: tableID,
		DestinationCidrBlock:       new("192.0.2.0/24"),
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	for _, options := range []*types.ModifyTransitGatewayOptions{
		{AssociationDefaultRouteTableId: tableID}, {PropagationDefaultRouteTableId: tableID},
	} {
		_, err = c.ModifyTransitGateway(
			ctx,
			&ec2.ModifyTransitGatewayInput{TransitGatewayId: a.TransitGatewayId, Options: options},
		)
		requireEC2Code(t, err, "InvalidParameterValue")
	}

	_, err = c.ModifyTransitGateway(ctx, &ec2.ModifyTransitGatewayInput{
		TransitGatewayId: a.TransitGatewayId,
		Options:          &types.ModifyTransitGatewayOptions{AssociationDefaultRouteTableId: new("tgw-rtb-missing")},
	})
	requireEC2Code(t, err, "InvalidRouteTableID.NotFound")
	_, err = c.DeleteTransitGateway(ctx, &ec2.DeleteTransitGatewayInput{TransitGatewayId: b.TransitGatewayId})
	requireEC2Code(t, err, "IncorrectState")
	_, err = c.DeleteTransitGateway(ctx, &ec2.DeleteTransitGatewayInput{TransitGatewayId: a.TransitGatewayId})
	requireEC2Code(t, err, "IncorrectState")
	modified, err := c.ModifyTransitGateway(ctx, &ec2.ModifyTransitGatewayInput{
		TransitGatewayId: a.TransitGatewayId,
		Options: &types.ModifyTransitGatewayOptions{
			DefaultRouteTableAssociation: types.DefaultRouteTableAssociationValueEnable,
			DefaultRouteTablePropagation: types.DefaultRouteTablePropagationValueEnable,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	defaultID := modified.TransitGateway.Options.AssociationDefaultRouteTableId
	if defaultID == nil || modified.TransitGateway.Options.PropagationDefaultRouteTableId == nil {
		t.Fatal("default tables missing")
	}

	_, err = c.AssociateTransitGatewayRouteTable(ctx, &ec2.AssociateTransitGatewayRouteTableInput{
		TransitGatewayAttachmentId: attachmentID, TransitGatewayRouteTableId: defaultID,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.EnableTransitGatewayRouteTablePropagation(ctx, &ec2.EnableTransitGatewayRouteTablePropagationInput{
		TransitGatewayAttachmentId: attachmentID, TransitGatewayRouteTableId: defaultID,
	})
	if err != nil {
		t.Fatal(err)
	}

	routes, err := c.SearchTransitGatewayRoutes(ctx, &ec2.SearchTransitGatewayRoutesInput{
		TransitGatewayRouteTableId: defaultID,
		Filters:                    []types.Filter{{Name: new("type"), Values: []string{"propagated"}}},
	})
	if err != nil || len(routes.Routes) != 1 || *routes.Routes[0].DestinationCidrBlock != "10.30.0.0/16" {
		t.Fatalf("propagated routes: %v %v", routes, err)
	}

	_, err = c.CreateTransitGatewayRoute(ctx, &ec2.CreateTransitGatewayRouteInput{
		TransitGatewayRouteTableId: defaultID,
		TransitGatewayAttachmentId: attachmentID, Blackhole: new(true), DestinationCidrBlock: new("192.0.2.0/24"),
	})
	requireEC2Code(t, err, "InvalidParameterCombination")
	_, err = c.CreateTransitGatewayRoute(ctx, &ec2.CreateTransitGatewayRouteInput{
		TransitGatewayRouteTableId: defaultID,
		TransitGatewayAttachmentId: attachmentID, DestinationCidrBlock: new("192.0.2.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteTransitGatewayVpcAttachment(
		ctx,
		&ec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: attachmentID},
	)
	if err != nil {
		t.Fatal(err)
	}

	routes, err = c.SearchTransitGatewayRoutes(
		ctx,
		&ec2.SearchTransitGatewayRoutesInput{TransitGatewayRouteTableId: defaultID, Filters: []types.Filter{}},
	)
	if err != nil || len(routes.Routes) != 1 || routes.Routes[0].State != types.TransitGatewayRouteStateBlackhole {
		t.Fatalf("blackhole routes: %v %v", routes, err)
	}

	associations, err := c.GetTransitGatewayRouteTableAssociations(
		ctx,
		&ec2.GetTransitGatewayRouteTableAssociationsInput{
			TransitGatewayRouteTableId: defaultID,
			Filters:                    []types.Filter{},
		},
	)
	if err != nil || len(associations.Associations) != 0 {
		t.Fatalf("associations: %v %v", associations, err)
	}

	props, err := c.GetTransitGatewayRouteTablePropagations(
		ctx,
		&ec2.GetTransitGatewayRouteTablePropagationsInput{
			TransitGatewayRouteTableId: defaultID,
			Filters:                    []types.Filter{},
		},
	)
	if err != nil || len(props.TransitGatewayRouteTablePropagations) != 0 {
		t.Fatalf("propagations: %v %v", props, err)
	}

	_, err = c.DeleteTransitGateway(ctx, &ec2.DeleteTransitGatewayInput{TransitGatewayId: a.TransitGatewayId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.SearchTransitGatewayRoutes(
		ctx,
		&ec2.SearchTransitGatewayRoutesInput{TransitGatewayRouteTableId: defaultID, Filters: []types.Filter{}},
	)
	requireEC2Code(t, err, "InvalidRouteTableID.NotFound")
}

func TestEC2TransitGatewayValidation(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	for _, options := range []*types.TransitGatewayRequestOptions{
		{DnsSupport: types.DnsSupportValue("invalid")},
		{DefaultRouteTableAssociation: types.DefaultRouteTableAssociationValue("invalid")},
		{DefaultRouteTablePropagation: types.DefaultRouteTablePropagationValue("invalid")},
		{AutoAcceptSharedAttachments: types.AutoAcceptSharedAttachmentsValue("invalid")},
		{VpnEcmpSupport: types.VpnEcmpSupportValue("invalid")},
		{SecurityGroupReferencingSupport: types.SecurityGroupReferencingSupportValue("invalid")},
		{TransitGatewayCidrBlocks: []string{"invalid"}},
		{AmazonSideAsn: new(int64(1))},
	} {
		_, err := c.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{Options: options})
		requireEC2Code(t, err, "InvalidParameterValue")
	}

	_, err := c.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{Options: &types.TransitGatewayRequestOptions{
		MulticastSupport: types.MulticastSupportValueEnable,
	}})
	requireEC2Code(t, err, "NotImplemented")
	gateway, err := c.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyTransitGateway(ctx, &ec2.ModifyTransitGatewayInput{
		TransitGatewayId: gateway.TransitGateway.TransitGatewayId,
		Options:          &types.ModifyTransitGatewayOptions{DnsSupport: types.DnsSupportValue("bad")},
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	vpc := createVPC(t, c, "10.31.0.0/16")
	subnet := createSubnet(t, c, vpc, "10.31.0.0/24")
	_, err = c.CreateTransitGatewayVpcAttachment(ctx, &ec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: gateway.TransitGateway.TransitGatewayId, VpcId: new(vpc), SubnetIds: []string{subnet},
		Options: &types.CreateTransitGatewayVpcAttachmentRequestOptions{DnsSupport: types.DnsSupportValue("bad")},
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	attachment, err := c.CreateTransitGatewayVpcAttachment(ctx, &ec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: gateway.TransitGateway.TransitGatewayId, VpcId: new(vpc), SubnetIds: []string{subnet},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyTransitGatewayVpcAttachment(ctx, &ec2.ModifyTransitGatewayVpcAttachmentInput{
		TransitGatewayAttachmentId: attachment.TransitGatewayVpcAttachment.TransitGatewayAttachmentId,
		Options: &types.ModifyTransitGatewayVpcAttachmentRequestOptions{
			DnsSupport: types.DnsSupportValue("bad"),
		},
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	_, err = c.ModifyTransitGatewayVpcAttachment(ctx, &ec2.ModifyTransitGatewayVpcAttachmentInput{
		TransitGatewayAttachmentId: attachment.TransitGatewayVpcAttachment.TransitGatewayAttachmentId,
		AddSubnetIds:               []string{"subnet-missing"},
	})
	requireEC2Code(t, err, "InvalidSubnetID.NotFound")
}

func TestEC2VPNRoutingCleanup(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	customer, err := c.CreateCustomerGateway(ctx, &ec2.CreateCustomerGatewayInput{
		BgpAsn: new(int32(65000)), IpAddress: new("203.0.113.2"), Type: types.GatewayTypeIpsec1,
	})
	if err != nil {
		t.Fatal(err)
	}

	gateway, err := c.CreateVpnGateway(ctx, &ec2.CreateVpnGatewayInput{Type: types.GatewayTypeIpsec1})
	if err != nil {
		t.Fatal(err)
	}

	vpc := createVPC(t, c, "10.32.0.0/16")
	_, err = c.AttachVpnGateway(
		ctx,
		&ec2.AttachVpnGatewayInput{VpnGatewayId: gateway.VpnGateway.VpnGatewayId, VpcId: new(vpc)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteVpnGateway(ctx, &ec2.DeleteVpnGatewayInput{VpnGatewayId: gateway.VpnGateway.VpnGatewayId})
	requireEC2Code(t, err, "DependencyViolation")
	table, err := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: new(vpc)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.EnableVgwRoutePropagation(ctx, &ec2.EnableVgwRoutePropagationInput{
		GatewayId: gateway.VpnGateway.VpnGatewayId, RouteTableId: table.RouteTable.RouteTableId,
	})
	if err != nil {
		t.Fatal(err)
	}

	vpn, err := c.CreateVpnConnection(ctx, &ec2.CreateVpnConnectionInput{
		CustomerGatewayId: customer.CustomerGateway.CustomerGatewayId,
		VpnGatewayId:      gateway.VpnGateway.VpnGatewayId,
		Type:              new("ipsec.1"),
		Options:           &types.VpnConnectionOptionsSpecification{StaticRoutesOnly: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCustomerGateway(
		ctx,
		&ec2.DeleteCustomerGatewayInput{CustomerGatewayId: customer.CustomerGateway.CustomerGatewayId},
	)
	requireEC2Code(t, err, "DependencyViolation")
	_, err = c.CreateVpnConnectionRoute(ctx, &ec2.CreateVpnConnectionRouteInput{
		VpnConnectionId:      vpn.VpnConnection.VpnConnectionId,
		DestinationCidrBlock: new("192.168.1.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}

	tables, err := c.DescribeRouteTables(
		ctx,
		&ec2.DescribeRouteTablesInput{RouteTableIds: []string{*table.RouteTable.RouteTableId}},
	)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, route := range tables.RouteTables[0].Routes {
		if route.DestinationCidrBlock != nil && *route.DestinationCidrBlock == "192.168.1.0/24" {
			found = route.Origin == types.RouteOriginEnableVgwRoutePropagation
		}
	}

	if !found {
		t.Fatal("VPN static route was not propagated")
	}

	_, err = c.DetachVpnGateway(
		ctx,
		&ec2.DetachVpnGatewayInput{VpnGatewayId: gateway.VpnGateway.VpnGatewayId, VpcId: new(vpc)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteVpnGateway(ctx, &ec2.DeleteVpnGatewayInput{VpnGatewayId: gateway.VpnGateway.VpnGatewayId})
	requireEC2Code(t, err, "DependencyViolation")
	_, err = c.DeleteVpnConnection(
		ctx,
		&ec2.DeleteVpnConnectionInput{VpnConnectionId: vpn.VpnConnection.VpnConnectionId},
	)
	if err != nil {
		t.Fatal(err)
	}

	tgw, err := c.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}

	vpn, err = c.CreateVpnConnection(ctx, &ec2.CreateVpnConnectionInput{
		CustomerGatewayId: customer.CustomerGateway.CustomerGatewayId,
		TransitGatewayId:  tgw.TransitGateway.TransitGatewayId,
		Type:              new("ipsec.1"),
		Options:           &types.VpnConnectionOptionsSpecification{StaticRoutesOnly: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}

	attachments, err := c.DescribeTransitGatewayAttachments(ctx, &ec2.DescribeTransitGatewayAttachmentsInput{
		Filters: []types.Filter{{Name: new("resource-id"), Values: []string{*vpn.VpnConnection.VpnConnectionId}}},
	})
	if err != nil || len(attachments.TransitGatewayAttachments) != 1 {
		t.Fatalf("VPN attachment: %v %v", attachments, err)
	}

	attachmentID := attachments.TransitGatewayAttachments[0].TransitGatewayAttachmentId
	tableID := tgw.TransitGateway.Options.AssociationDefaultRouteTableId
	_, err = c.CreateVpnConnectionRoute(ctx, &ec2.CreateVpnConnectionRouteInput{
		VpnConnectionId:      vpn.VpnConnection.VpnConnectionId,
		DestinationCidrBlock: new("192.168.2.0/24"),
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	routes, err := c.SearchTransitGatewayRoutes(ctx, &ec2.SearchTransitGatewayRoutesInput{
		TransitGatewayRouteTableId: tableID, Filters: []types.Filter{},
	})
	if err != nil || len(routes.Routes) != 0 {
		t.Fatalf("Unexpected VPN propagated routes: %v %v", routes, err)
	}

	_, err = c.CreateTransitGatewayRoute(ctx, &ec2.CreateTransitGatewayRouteInput{
		TransitGatewayRouteTableId: tableID,
		TransitGatewayAttachmentId: attachmentID, DestinationCidrBlock: new("192.168.3.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []*string{
		tgw.TransitGateway.TransitGatewayId, tableID, attachmentID,
		customer.CustomerGateway.CustomerGatewayId, vpn.VpnConnection.VpnConnectionId,
	} {
		_, err = c.CreateTags(
			ctx,
			&ec2.CreateTagsInput{Resources: []string{*id}, Tags: []types.Tag{{Key: new("review"), Value: new("ok")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, tagErr := c.DescribeTags(
			ctx,
			&ec2.DescribeTagsInput{
				Filters: []types.Filter{
					{Name: new("resource-id"), Values: []string{*id}},
					{Name: new("key"), Values: []string{"review"}},
				},
			},
		)
		if tagErr != nil || len(tags.Tags) != 1 {
			t.Fatalf("tags: %v %v", tags, tagErr)
		}

		_, err = c.DeleteTags(
			ctx,
			&ec2.DeleteTagsInput{Resources: []string{*id}, Tags: []types.Tag{{Key: new("review")}}},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteVpnConnection(
		ctx,
		&ec2.DeleteVpnConnectionInput{VpnConnectionId: vpn.VpnConnection.VpnConnectionId},
	)
	if err != nil {
		t.Fatal(err)
	}

	routes, err = c.SearchTransitGatewayRoutes(
		ctx,
		&ec2.SearchTransitGatewayRoutesInput{TransitGatewayRouteTableId: tableID, Filters: []types.Filter{}},
	)
	if err != nil || len(routes.Routes) != 1 || routes.Routes[0].State != types.TransitGatewayRouteStateBlackhole {
		t.Fatalf("VPN cleanup: %v %v", routes, err)
	}

	associations, err := c.GetTransitGatewayRouteTableAssociations(
		ctx,
		&ec2.GetTransitGatewayRouteTableAssociationsInput{TransitGatewayRouteTableId: tableID},
	)
	if err != nil || len(associations.Associations) != 0 {
		t.Fatalf("VPN associations: %v %v", associations, err)
	}

	_, err = c.DeleteTransitGateway(
		ctx,
		&ec2.DeleteTransitGatewayInput{TransitGatewayId: tgw.TransitGateway.TransitGatewayId},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCustomerGateway(
		ctx,
		&ec2.DeleteCustomerGatewayInput{CustomerGatewayId: customer.CustomerGateway.CustomerGatewayId},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestEC2TransitGatewaySeparateDefaultTables(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	gateway, err := c.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}

	table, err := c.CreateTransitGatewayRouteTable(ctx, &ec2.CreateTransitGatewayRouteTableInput{
		TransitGatewayId: gateway.TransitGateway.TransitGatewayId,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyTransitGateway(ctx, &ec2.ModifyTransitGatewayInput{
		TransitGatewayId: gateway.TransitGateway.TransitGatewayId,
		Options: &types.ModifyTransitGatewayOptions{
			PropagationDefaultRouteTableId: table.TransitGatewayRouteTable.TransitGatewayRouteTableId,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteTransitGateway(
		ctx,
		&ec2.DeleteTransitGatewayInput{TransitGatewayId: gateway.TransitGateway.TransitGatewayId},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []*string{
		gateway.TransitGateway.Options.AssociationDefaultRouteTableId,
		table.TransitGatewayRouteTable.TransitGatewayRouteTableId,
	} {
		_, err = c.GetTransitGatewayRouteTablePropagations(ctx, &ec2.GetTransitGatewayRouteTablePropagationsInput{
			TransitGatewayRouteTableId: id,
		})
		requireEC2Code(t, err, "InvalidRouteTableID.NotFound")
	}
}

func TestEC2VPNUnsupportedOptions(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateVpnConnection(ctx, &ec2.CreateVpnConnectionInput{
		CustomerGatewayId: new("cgw-missing"), TransitGatewayId: new("tgw-missing"), Type: new("ipsec.1"),
		Options: &types.VpnConnectionOptionsSpecification{LocalIpv4NetworkCidr: new("10.0.0.0/16")},
	})
	requireEC2Code(t, err, "InvalidParameterValue")
	_, err = c.CreateVpnConnection(ctx, &ec2.CreateVpnConnectionInput{
		CustomerGatewayId: new("cgw-missing"), TransitGatewayId: new("tgw-missing"), Type: new("ipsec.1"),
		Options: &types.VpnConnectionOptionsSpecification{TunnelOptions: []types.VpnTunnelOptionsSpecification{
			{Phase1LifetimeSeconds: new(int32(3600))},
		}},
	})
	requireEC2Code(t, err, "InvalidParameterValue")
}

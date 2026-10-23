package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestEC2TransitGatewayLifecycle(t *testing.T) {
	c := ec2Client(server(t), "test", "us-east-1")
	ctx := context.Background()

	// 1. Create Transit Gateway
	tgwRes, err := c.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{
		Description: new("Test TGW"),
		Options: &types.TransitGatewayRequestOptions{
			AmazonSideAsn:                new(int64(64512)),
			AutoAcceptSharedAttachments:  types.AutoAcceptSharedAttachmentsValueDisable,
			DefaultRouteTableAssociation: types.DefaultRouteTableAssociationValueDisable,
			DefaultRouteTablePropagation: types.DefaultRouteTablePropagationValueDisable,
			DnsSupport:                   types.DnsSupportValueEnable,
			VpnEcmpSupport:               types.VpnEcmpSupportValueEnable,
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeTransitGateway,
				Tags: []types.Tag{
					{Key: new("Name"), Value: new("my-tgw")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTransitGateway failed: %v", err)
	}

	tgwID := *tgwRes.TransitGateway.TransitGatewayId
	if *tgwRes.TransitGateway.Options.AmazonSideAsn != 64512 {
		t.Fatalf("Expected AmazonSideAsn 64512, got %d", *tgwRes.TransitGateway.Options.AmazonSideAsn)
	}

	// 2. Describe Transit Gateways with Filter
	descTgw, err := c.DescribeTransitGateways(ctx, &ec2.DescribeTransitGatewaysInput{
		TransitGatewayIds: []string{tgwID},
		Filters: []types.Filter{
			{Name: new("tag:Name"), Values: []string{"my-tgw"}},
		},
	})
	if err != nil || len(descTgw.TransitGateways) != 1 {
		t.Fatalf("DescribeTransitGateways failed: %v, count: %d", err, len(descTgw.TransitGateways))
	}

	// 3. Modify Transit Gateway
	modTgw, err := c.ModifyTransitGateway(ctx, &ec2.ModifyTransitGatewayInput{
		TransitGatewayId: new(tgwID),
		Description:      new("Updated TGW"),
	})
	if err != nil || *modTgw.TransitGateway.Description != "Updated TGW" {
		t.Fatalf("ModifyTransitGateway failed: %v", err)
	}

	// 4. Create VPC and Subnets for VPC attachment
	vpcID := createVPC(t, c, "10.0.0.0/16")
	sn1 := createSubnet(t, c, vpcID, "10.0.1.0/24")

	// 5. Create VPC Attachment
	attachRes, err := c.CreateTransitGatewayVpcAttachment(ctx, &ec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: new(tgwID),
		VpcId:            new(vpcID),
		SubnetIds:        []string{sn1},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeTransitGatewayAttachment,
				Tags: []types.Tag{
					{Key: new("Name"), Value: new("tgw-vpc-attach")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTransitGatewayVpcAttachment failed: %v", err)
	}

	attachID := *attachRes.TransitGatewayVpcAttachment.TransitGatewayAttachmentId

	// 6. Describe VPC Attachments and Attachments
	descAttach, err := c.DescribeTransitGatewayVpcAttachments(ctx, &ec2.DescribeTransitGatewayVpcAttachmentsInput{
		TransitGatewayAttachmentIds: []string{attachID},
		Filters: []types.Filter{
			{Name: new("transit-gateway-id"), Values: []string{tgwID}},
		},
	})
	if err != nil || len(descAttach.TransitGatewayVpcAttachments) != 1 {
		t.Fatalf("DescribeTransitGatewayVpcAttachments failed: %v", err)
	}

	descAllAttach, err := c.DescribeTransitGatewayAttachments(ctx, &ec2.DescribeTransitGatewayAttachmentsInput{
		TransitGatewayAttachmentIds: []string{attachID},
	})
	if err != nil || len(descAllAttach.TransitGatewayAttachments) != 1 {
		t.Fatalf("DescribeTransitGatewayAttachments failed: %v", err)
	}

	// 7. Transit Gateway Route Table Lifecycle
	rtbRes, err := c.CreateTransitGatewayRouteTable(ctx, &ec2.CreateTransitGatewayRouteTableInput{
		TransitGatewayId: new(tgwID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeTransitGatewayRouteTable,
				Tags: []types.Tag{
					{Key: new("Name"), Value: new("tgw-rtb")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTransitGatewayRouteTable failed: %v", err)
	}

	tgwRtbID := *rtbRes.TransitGatewayRouteTable.TransitGatewayRouteTableId

	// Associate Route Table
	assocRes, err := c.AssociateTransitGatewayRouteTable(ctx, &ec2.AssociateTransitGatewayRouteTableInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		TransitGatewayAttachmentId: new(attachID),
	})
	if err != nil {
		t.Fatalf("AssociateTransitGatewayRouteTable failed: %v", err)
	}

	if assocRes.Association.State != types.TransitGatewayAssociationStateAssociated {
		t.Fatalf("Unexpected association state: %v", assocRes.Association.State)
	}

	getAssoc, err := c.GetTransitGatewayRouteTableAssociations(ctx, &ec2.GetTransitGatewayRouteTableAssociationsInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
	})
	if err != nil || len(getAssoc.Associations) != 1 {
		t.Fatalf("GetTransitGatewayRouteTableAssociations failed: %v", err)
	}

	// Enable Propagation
	propRes, err := c.EnableTransitGatewayRouteTablePropagation(
		ctx,
		&ec2.EnableTransitGatewayRouteTablePropagationInput{
			TransitGatewayRouteTableId: new(tgwRtbID),
			TransitGatewayAttachmentId: new(attachID),
		},
	)
	if err != nil {
		t.Fatalf("EnableTransitGatewayRouteTablePropagation failed: %v", err)
	}

	if propRes.Propagation.State != types.TransitGatewayPropagationStateEnabled {
		t.Fatalf("Unexpected propagation state: %v", propRes.Propagation.State)
	}

	getProp, err := c.GetTransitGatewayRouteTablePropagations(ctx, &ec2.GetTransitGatewayRouteTablePropagationsInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
	})
	if err != nil || len(getProp.TransitGatewayRouteTablePropagations) != 1 {
		t.Fatalf("GetTransitGatewayRouteTablePropagations failed: %v", err)
	}

	// Create Static and Blackhole Routes in TGW Route Table
	_, err = c.CreateTransitGatewayRoute(ctx, &ec2.CreateTransitGatewayRouteInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		DestinationCidrBlock:       new("192.168.0.0/16"),
		TransitGatewayAttachmentId: new(attachID),
	})
	if err != nil {
		t.Fatalf("CreateTransitGatewayRoute static failed: %v", err)
	}

	_, err = c.CreateTransitGatewayRoute(ctx, &ec2.CreateTransitGatewayRouteInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		DestinationCidrBlock:       new("172.16.0.0/12"),
		Blackhole:                  new(true),
	})
	if err != nil {
		t.Fatalf("CreateTransitGatewayRoute blackhole failed: %v", err)
	}

	// Search Routes
	searchRes, err := c.SearchTransitGatewayRoutes(ctx, &ec2.SearchTransitGatewayRoutesInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		Filters: []types.Filter{
			{Name: new("type"), Values: []string{"static", "propagated"}},
		},
	})
	if err != nil || len(searchRes.Routes) < 2 {
		t.Fatalf("SearchTransitGatewayRoutes failed: %v, count: %d", err, len(searchRes.Routes))
	}

	// Delete Route
	_, err = c.DeleteTransitGatewayRoute(ctx, &ec2.DeleteTransitGatewayRouteInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		DestinationCidrBlock:       new("172.16.0.0/12"),
	})
	if err != nil {
		t.Fatalf("DeleteTransitGatewayRoute failed: %v", err)
	}

	// 8. VPC Route pointing to TGW
	vpcRtbs, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []types.Filter{
			{Name: new("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil || len(vpcRtbs.RouteTables) == 0 {
		t.Fatalf("DescribeRouteTables failed: %v", err)
	}

	vpcRtbID := *vpcRtbs.RouteTables[0].RouteTableId
	_, err = c.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         new(vpcRtbID),
		DestinationCidrBlock: new("10.50.0.0/16"),
		TransitGatewayId:     new(tgwID),
	})
	if err != nil {
		t.Fatalf("CreateRoute with TransitGatewayId failed: %v", err)
	}

	// Verify route.transit-gateway-id filter
	filteredRtb, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []types.Filter{
			{Name: new("route.transit-gateway-id"), Values: []string{tgwID}},
		},
	})
	if err != nil || len(filteredRtb.RouteTables) != 1 {
		t.Fatalf("Filter route.transit-gateway-id failed: %v", err)
	}

	// Replace Route
	_, err = c.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{
		RouteTableId:         new(vpcRtbID),
		DestinationCidrBlock: new("10.50.0.0/16"),
		TransitGatewayId:     new(tgwID),
	})
	if err != nil {
		t.Fatalf("ReplaceRoute failed: %v", err)
	}

	// Delete Route
	_, err = c.DeleteRoute(ctx, &ec2.DeleteRouteInput{
		RouteTableId:         new(vpcRtbID),
		DestinationCidrBlock: new("10.50.0.0/16"),
	})
	if err != nil {
		t.Fatalf("DeleteRoute failed: %v", err)
	}

	// 9. Dependency violation when deleting TGW with active attachments
	_, err = c.DeleteTransitGateway(ctx, &ec2.DeleteTransitGatewayInput{
		TransitGatewayId: new(tgwID),
	})
	code(t, err, "IncorrectState")

	// Disassociate and Disable Propagation
	_, err = c.DisableTransitGatewayRouteTablePropagation(ctx, &ec2.DisableTransitGatewayRouteTablePropagationInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		TransitGatewayAttachmentId: new(attachID),
	})
	if err != nil {
		t.Fatalf("DisableTransitGatewayRouteTablePropagation failed: %v", err)
	}

	_, err = c.DisassociateTransitGatewayRouteTable(ctx, &ec2.DisassociateTransitGatewayRouteTableInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
		TransitGatewayAttachmentId: new(attachID),
	})
	if err != nil {
		t.Fatalf("DisassociateTransitGatewayRouteTable failed: %v", err)
	}

	// Delete TGW Attachment
	_, err = c.DeleteTransitGatewayVpcAttachment(ctx, &ec2.DeleteTransitGatewayVpcAttachmentInput{
		TransitGatewayAttachmentId: new(attachID),
	})
	if err != nil {
		t.Fatalf("DeleteTransitGatewayVpcAttachment failed: %v", err)
	}

	// Delete TGW Route Table
	_, err = c.DeleteTransitGatewayRouteTable(ctx, &ec2.DeleteTransitGatewayRouteTableInput{
		TransitGatewayRouteTableId: new(tgwRtbID),
	})
	if err != nil {
		t.Fatalf("DeleteTransitGatewayRouteTable failed: %v", err)
	}

	// Delete TGW
	_, err = c.DeleteTransitGateway(ctx, &ec2.DeleteTransitGatewayInput{
		TransitGatewayId: new(tgwID),
	})
	if err != nil {
		t.Fatalf("DeleteTransitGateway failed: %v", err)
	}
}

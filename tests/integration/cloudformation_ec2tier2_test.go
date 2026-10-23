package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestCloudFormationEC2Tier2StackLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c, cf := ec2Client(s, "test", "us-east-1"), cfnClient(s)

	vpcID := createVPC(t, c, "10.0.0.0/16")
	subnetID := createSubnet(t, c, vpcID, "10.0.1.0/24")

	ref := func(logical string) map[string]any { return map[string]any{"Ref": logical} }
	template := map[string]any{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Resources": map[string]any{
			"TGW": map[string]any{
				"Type": "AWS::EC2::TransitGateway",
				"Properties": map[string]any{
					"Description":   "CFN TGW",
					"AmazonSideAsn": 64512,
					"Tags": []any{
						map[string]any{"Key": "Environment", "Value": "Test"},
					},
				},
			},
			"TGWAttachment": map[string]any{
				"Type": "AWS::EC2::TransitGatewayAttachment",
				"Properties": map[string]any{
					"TransitGatewayId": ref("TGW"),
					"VpcId":            vpcID,
					"SubnetIds":        []any{subnetID},
				},
			},
			"TGWRtb": map[string]any{
				"Type": "AWS::EC2::TransitGatewayRouteTable",
				"Properties": map[string]any{
					"TransitGatewayId": ref("TGW"),
				},
			},
			"TGWRoute": map[string]any{
				"Type": "AWS::EC2::TransitGatewayRoute",
				"Properties": map[string]any{
					"TransitGatewayRouteTableId": ref("TGWRtb"),
					"DestinationCidrBlock":       "192.168.0.0/16",
					"TransitGatewayAttachmentId": ref("TGWAttachment"),
				},
			},
			"CGW": map[string]any{
				"Type": "AWS::EC2::CustomerGateway",
				"Properties": map[string]any{
					"BgpAsn":    65000,
					"IpAddress": "203.0.113.10",
					"Type":      "ipsec.1",
				},
			},
			"VGW": map[string]any{
				"Type": "AWS::EC2::VPNGateway",
				"Properties": map[string]any{
					"Type":          "ipsec.1",
					"AmazonSideAsn": 64512,
				},
			},
			"VPCGatewayAttach": map[string]any{
				"Type": "AWS::EC2::VPCGatewayAttachment",
				"Properties": map[string]any{
					"VpcId":        vpcID,
					"VpnGatewayId": ref("VGW"),
				},
			},
			"VPNConn": map[string]any{
				"Type":      "AWS::EC2::VPNConnection",
				"DependsOn": []any{"VPCGatewayAttach"},
				"Properties": map[string]any{
					"CustomerGatewayId": ref("CGW"),
					"VpnGatewayId":      ref("VGW"),
					"Type":              "ipsec.1",
					"StaticRoutesOnly":  true,
				},
			},
			"VPNRoute": map[string]any{
				"Type": "AWS::EC2::VPNConnectionRoute",
				"Properties": map[string]any{
					"VpnConnectionId":      ref("VPNConn"),
					"DestinationCidrBlock": "10.100.0.0/16",
				},
			},
		},
	}

	body, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}

	stackName := "test-ec2tier2-stack"
	_, err = cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    new(stackName),
		TemplateBody: new(string(body)),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	st := wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusCreateComplete)

	// Verify resources created in EC2
	tgws, err := c.DescribeTransitGateways(ctx, &ec2.DescribeTransitGatewaysInput{
		Filters: []ec2types.Filter{
			{Name: new("tag:Environment"), Values: []string{"Test"}},
		},
	})
	if err != nil || len(tgws.TransitGateways) != 1 {
		t.Fatalf("DescribeTransitGateways after stack create failed: %v, count: %d", err, len(tgws.TransitGateways))
	}

	cgws, err := c.DescribeCustomerGateways(ctx, &ec2.DescribeCustomerGatewaysInput{
		Filters: []ec2types.Filter{
			{Name: new("ip-address"), Values: []string{"203.0.113.10"}},
		},
	})
	if err != nil || len(cgws.CustomerGateways) != 1 {
		t.Fatalf("DescribeCustomerGateways after stack create failed: %v, count: %d", err, len(cgws.CustomerGateways))
	}

	vpns, err := c.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{})
	if err != nil || len(vpns.VpnConnections) != 1 {
		t.Fatalf("DescribeVpnConnections after stack create failed: %v, count: %d", err, len(vpns.VpnConnections))
	}

	resources := template["Resources"].(map[string]any)
	resources["TGW"].(map[string]any)["Properties"].(map[string]any)["TransitGatewayCidrBlocks"] = []any{
		"172.20.0.0/24",
	}
	replacementSubnet := createSubnet(t, c, vpcID, "10.0.2.0/24")
	resources["TGWAttachment"].(map[string]any)["Properties"].(map[string]any)["SubnetIds"] = []any{replacementSubnet}
	body, err = json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(stackName), TemplateBody: new(string(body))})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusUpdateComplete)
	updatedGateways, err := c.DescribeTransitGateways(ctx, &ec2.DescribeTransitGatewaysInput{
		TransitGatewayIds: []string{*tgws.TransitGateways[0].TransitGatewayId},
	})
	if err != nil {
		t.Fatal(err)
	}

	blocks := updatedGateways.TransitGateways[0].Options.TransitGatewayCidrBlocks
	if len(blocks) != 1 || blocks[0] != "172.20.0.0/24" {
		t.Fatalf("CIDR update ignored: %v", blocks)
	}

	updatedAttachments, err := c.DescribeTransitGatewayVpcAttachments(
		ctx,
		&ec2.DescribeTransitGatewayVpcAttachmentsInput{
			Filters: []ec2types.Filter{
				{Name: new("transit-gateway-id"), Values: []string{*tgws.TransitGateways[0].TransitGatewayId}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	subnets := updatedAttachments.TransitGatewayVpcAttachments[0].SubnetIds
	if len(subnets) != 1 || subnets[0] != replacementSubnet {
		t.Fatalf("Subnet update ignored: %v", subnets)
	}

	// Delete Stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: new(stackName),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify resources removed
	descDeletedTgw, err := c.DescribeTransitGateways(ctx, &ec2.DescribeTransitGatewaysInput{
		TransitGatewayIds: []string{*tgws.TransitGateways[0].TransitGatewayId},
	})
	if err != nil || len(descDeletedTgw.TransitGateways) != 1 ||
		descDeletedTgw.TransitGateways[0].State != ec2types.TransitGatewayStateDeleted {
		t.Fatalf("Expected TGW in deleted state: %+v, %v", descDeletedTgw, err)
	}

	descDeletedVpn, err := c.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{
		VpnConnectionIds: []string{*vpns.VpnConnections[0].VpnConnectionId},
	})
	if err != nil || len(descDeletedVpn.VpnConnections) != 1 ||
		descDeletedVpn.VpnConnections[0].State != ec2types.VpnStateDeleted {
		t.Fatalf("Expected VPN in deleted state: %+v, %v", descDeletedVpn, err)
	}
}

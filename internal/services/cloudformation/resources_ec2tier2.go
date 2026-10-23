package cloudformation

import (
	"cmp"
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

const (
	memberMulticastSupport             = "MulticastSupport"
	memberAmazonSideAsn                = "AmazonSideAsn"
	memberAutoAcceptSharedAttachments  = "AutoAcceptSharedAttachments"
	memberDefaultRouteTableAssociation = "DefaultRouteTableAssociation"
	memberDefaultRouteTablePropagation = "DefaultRouteTablePropagation"
	memberDnsSupport                   = "DnsSupport"
	memberVpnEcmpSupport               = "VpnEcmpSupport"
	memberTransitGatewayID             = "TransitGatewayId"
	memberTransitGatewayAttachmentID   = "TransitGatewayAttachmentId"
	memberTransitGatewayRouteTableID   = "TransitGatewayRouteTableId"
	memberDestinationCidrBlock         = "DestinationCidrBlock"
	memberCustomerGatewayID            = "CustomerGatewayId"
	memberInternetGatewayID            = "InternetGatewayId"
	memberVpnGatewayID                 = "VpnGatewayId"
	memberVpnConnectionID              = "VpnConnectionId"
)

func init() {
	register("AWS::EC2::TransitGateway", &resourceType{
		properties: []string{
			memberDescription, memberAmazonSideAsn, memberAutoAcceptSharedAttachments,
			memberDefaultRouteTableAssociation, memberDefaultRouteTablePropagation,
			memberDnsSupport, memberVpnEcmpSupport, memberMulticastSupport,
			"TransitGatewayCidrBlocks", memberTags,
		},
		replace:    []string{memberAmazonSideAsn, memberMulticastSupport},
		attributes: []string{"Id", "TransitGatewayArn"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{}
			if p.has(memberDescription) {
				params[memberDescription] = p.str(memberDescription)
			}

			options := map[string]any{}
			if p.has(memberAmazonSideAsn) {
				options[memberAmazonSideAsn] = p[memberAmazonSideAsn]
			}

			for _, opt := range []string{
				memberAutoAcceptSharedAttachments, memberDefaultRouteTableAssociation,
				memberDefaultRouteTablePropagation, memberDnsSupport, memberVpnEcmpSupport, memberMulticastSupport,
			} {
				if p.has(opt) {
					options[opt] = p.str(opt)
				}
			}

			if p.has("TransitGatewayCidrBlocks") {
				options["TransitGatewayCidrBlocks"] = p.list("TransitGatewayCidrBlocks")
			}

			if len(options) > 0 {
				params["Options"] = options
			}

			params[memberTagSpecifications] = tagSpecifications("transit-gateway", p)
			out, err := c.call(ctx, "ec2", "CreateTransitGateway", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "TransitGateway", memberTransitGatewayID)
			arn := outText(out, "TransitGateway", "TransitGatewayArn")
			return id, map[string]any{"Id": id, "TransitGatewayArn": arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{memberTransitGatewayID: r.Physical}
			if p.has(memberDescription) {
				params[memberDescription] = p.str(memberDescription)
			}

			options := map[string]any{}
			for _, opt := range []string{
				memberAutoAcceptSharedAttachments, memberDefaultRouteTableAssociation,
				memberDefaultRouteTablePropagation, memberDnsSupport, memberVpnEcmpSupport,
			} {
				if p.has(opt) {
					options[opt] = p.str(opt)
				}
			}

			if p.changed(r.Properties, "TransitGatewayCidrBlocks") {
				old := props(r.Properties).strings("TransitGatewayCidrBlocks")
				next := p.strings("TransitGatewayCidrBlocks")
				add, remove := diffStrings(old, next)
				options["AddTransitGatewayCidrBlocks"] = anySlice(add)
				options["RemoveTransitGatewayCidrBlocks"] = anySlice(remove)
			}

			if len(options) > 0 {
				params["Options"] = options
			}

			if len(params) > 1 {
				if _, err := c.call(ctx, "ec2", "ModifyTransitGateway", params); err != nil {
					return nil, err
				}
			}

			return updateEC2Tags(ctx, c, r, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteTransitGateway",
				memberTransitGatewayID,
				r.Physical,
				"InvalidTransitGatewayID.NotFound",
			)
		},
	})

	tgwAttachmentHandler := &resourceType{
		properties: []string{
			memberTransitGatewayID, memberVpcID, memberSubnetIDs, "Options", memberTags,
		},
		replace:    []string{memberTransitGatewayID, memberVpcID},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberTransitGatewayID: p.str(memberTransitGatewayID),
				memberVpcID:            p.str(memberVpcID),
				memberSubnetIDs:        p.list(memberSubnetIDs),
			}
			if p.has("Options") {
				params["Options"] = p["Options"]
			}

			params[memberTagSpecifications] = tagSpecifications("transit-gateway-attachment", p)
			out, err := c.call(ctx, "ec2", "CreateTransitGatewayVpcAttachment", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "TransitGatewayVpcAttachment", memberTransitGatewayAttachmentID)
			return id, map[string]any{"Id": id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{memberTransitGatewayAttachmentID: r.Physical}
			if p.has("Options") {
				params["Options"] = p["Options"]
			}

			if p.changed(r.Properties, memberSubnetIDs) {
				old := props(r.Properties).strings(memberSubnetIDs)
				next := p.strings(memberSubnetIDs)
				add, remove := diffStrings(old, next)
				params["AddSubnetIds"] = anySlice(add)
				params["RemoveSubnetIds"] = anySlice(remove)
			}

			if len(params) > 1 {
				if _, err := c.call(ctx, "ec2", "ModifyTransitGatewayVpcAttachment", params); err != nil {
					return nil, err
				}
			}

			return updateEC2Tags(ctx, c, r, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteTransitGatewayVpcAttachment",
				memberTransitGatewayAttachmentID,
				r.Physical,
				"InvalidTransitGatewayAttachmentID.NotFound",
			)
		},
	}
	register("AWS::EC2::TransitGatewayAttachment", tgwAttachmentHandler)
	register("AWS::EC2::TransitGatewayVpcAttachment", tgwAttachmentHandler)

	register("AWS::EC2::TransitGatewayRouteTable", &resourceType{
		properties: []string{memberTransitGatewayID, memberTags},
		replace:    []string{memberTransitGatewayID},
		attributes: []string{memberTransitGatewayRouteTableID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberTransitGatewayID:  p.str(memberTransitGatewayID),
				memberTagSpecifications: tagSpecifications("transit-gateway-route-table", p),
			}
			out, err := c.call(ctx, "ec2", "CreateTransitGatewayRouteTable", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "TransitGatewayRouteTable", memberTransitGatewayRouteTableID)
			return id, map[string]any{memberTransitGatewayRouteTableID: id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteTransitGatewayRouteTable",
				memberTransitGatewayRouteTableID,
				r.Physical,
				"InvalidRouteTableID.NotFound",
			)
		},
	})

	register("AWS::EC2::TransitGatewayRouteTableAssociation", &resourceType{
		properties: []string{memberTransitGatewayRouteTableID, memberTransitGatewayAttachmentID},
		replace:    []string{memberTransitGatewayRouteTableID, memberTransitGatewayAttachmentID},
		attributes: []string{memberTransitGatewayRouteTableID, memberTransitGatewayAttachmentID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			rtbID := p.str(memberTransitGatewayRouteTableID)
			attachID := p.str(memberTransitGatewayAttachmentID)
			params := map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberTransitGatewayAttachmentID: attachID,
			}
			_, err := c.call(ctx, "ec2", "AssociateTransitGatewayRouteTable", params)
			if err != nil {
				return "", nil, err
			}

			id := rtbID + "_" + attachID
			return id, map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberTransitGatewayAttachmentID: attachID,
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			rtbID := props(r.Properties).str(memberTransitGatewayRouteTableID)
			attachID := props(r.Properties).str(memberTransitGatewayAttachmentID)
			_, err := c.call(ctx, "ec2", "DisassociateTransitGatewayRouteTable", map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberTransitGatewayAttachmentID: attachID,
			})
			if gone(err, "InvalidRouteTableID.NotFound", "InvalidTransitGatewayAttachmentID.NotFound",
				"Resource.NotFound", "InvalidAssociation.NotFound") {
				return nil
			}

			return err
		},
	})

	register("AWS::EC2::TransitGatewayRouteTablePropagation", &resourceType{
		properties: []string{memberTransitGatewayRouteTableID, memberTransitGatewayAttachmentID},
		replace:    []string{memberTransitGatewayRouteTableID, memberTransitGatewayAttachmentID},
		attributes: []string{memberTransitGatewayRouteTableID, memberTransitGatewayAttachmentID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			rtbID := p.str(memberTransitGatewayRouteTableID)
			attachID := p.str(memberTransitGatewayAttachmentID)
			params := map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberTransitGatewayAttachmentID: attachID,
			}
			_, err := c.call(ctx, "ec2", "EnableTransitGatewayRouteTablePropagation", params)
			if err != nil {
				return "", nil, err
			}

			id := rtbID + "_" + attachID
			return id, map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberTransitGatewayAttachmentID: attachID,
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			rtbID := props(r.Properties).str(memberTransitGatewayRouteTableID)
			attachID := props(r.Properties).str(memberTransitGatewayAttachmentID)
			_, err := c.call(ctx, "ec2", "DisableTransitGatewayRouteTablePropagation", map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberTransitGatewayAttachmentID: attachID,
			})
			if gone(err, "InvalidRouteTableID.NotFound", "InvalidTransitGatewayAttachmentID.NotFound",
				"Resource.NotFound", "InvalidPropagation.NotFound") {
				return nil
			}

			return err
		},
	})

	register("AWS::EC2::TransitGatewayRoute", &resourceType{
		properties: []string{
			memberTransitGatewayRouteTableID, memberDestinationCidrBlock, memberTransitGatewayAttachmentID, "Blackhole",
		},
		replace: []string{
			memberTransitGatewayRouteTableID,
			memberDestinationCidrBlock,
			memberTransitGatewayAttachmentID,
			"Blackhole",
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			rtbID := p.str(memberTransitGatewayRouteTableID)
			cidr := p.str(memberDestinationCidrBlock)
			params := map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberDestinationCidrBlock:       cidr,
			}
			if p.has(memberTransitGatewayAttachmentID) {
				params[memberTransitGatewayAttachmentID] = p.str(memberTransitGatewayAttachmentID)
			}

			if p.has("Blackhole") {
				params["Blackhole"] = p.truth("Blackhole")
			}

			_, err := c.call(ctx, "ec2", "CreateTransitGatewayRoute", params)
			if err != nil {
				return "", nil, err
			}

			return rtbID + "_" + cidr, map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			rtbID := props(r.Properties).str(memberTransitGatewayRouteTableID)
			cidr := props(r.Properties).str(memberDestinationCidrBlock)
			_, err := c.call(ctx, "ec2", "DeleteTransitGatewayRoute", map[string]any{
				memberTransitGatewayRouteTableID: rtbID,
				memberDestinationCidrBlock:       cidr,
			})
			if gone(err, "InvalidRouteTableID.NotFound", "InvalidRoute.NotFound") {
				return nil
			}

			return err
		},
	})

	register("AWS::EC2::CustomerGateway", &resourceType{
		properties: []string{
			"BgpAsn", "BgpAsnExtended", propertyEFSIP, memberType, "DeviceName", "CertificateArn", memberTags,
		},
		replace:    []string{"BgpAsn", "BgpAsnExtended", propertyEFSIP, memberType, "DeviceName", "CertificateArn"},
		attributes: []string{memberCustomerGatewayID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberType:              p.str(memberType),
				propertyEFSIP:           p.str(propertyEFSIP),
				memberTagSpecifications: tagSpecifications("customer-gateway", p),
			}
			if p.has("BgpAsn") {
				params["BgpAsn"] = p["BgpAsn"]
			}

			if p.has("BgpAsnExtended") {
				params["BgpAsnExtended"] = p["BgpAsnExtended"]
			}

			if p.has("DeviceName") {
				params["DeviceName"] = p.str("DeviceName")
			}

			if p.has("CertificateArn") {
				params["CertificateArn"] = p.str("CertificateArn")
			}

			out, err := c.call(ctx, "ec2", "CreateCustomerGateway", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "CustomerGateway", memberCustomerGatewayID)
			return id, map[string]any{memberCustomerGatewayID: id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteCustomerGateway",
				memberCustomerGatewayID,
				r.Physical,
				"InvalidCustomerGatewayID.NotFound",
			)
		},
	})

	register("AWS::EC2::VPNGateway", &resourceType{
		properties: []string{memberType, memberAmazonSideAsn, memberTags},
		replace:    []string{memberType, memberAmazonSideAsn},
		attributes: []string{"VPNGatewayId"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberType:              p.str(memberType),
				memberTagSpecifications: tagSpecifications("vpn-gateway", p),
			}
			if p.has(memberAmazonSideAsn) {
				params[memberAmazonSideAsn] = p[memberAmazonSideAsn]
			}

			out, err := c.call(ctx, "ec2", "CreateVpnGateway", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "VpnGateway", memberVpnGatewayID)
			return id, map[string]any{"VPNGatewayId": id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteVpnGateway",
				memberVpnGatewayID,
				r.Physical,
				"InvalidVpnGatewayID.NotFound",
			)
		},
	})

	register("AWS::EC2::VPCGatewayAttachment", &resourceType{
		properties: []string{memberVpcID, memberInternetGatewayID, memberVpnGatewayID},
		replace:    []string{memberVpcID, memberInternetGatewayID, memberVpnGatewayID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			vpcID := p.str(memberVpcID)
			igwID := p.str(memberInternetGatewayID)
			vgwID := p.str(memberVpnGatewayID)
			if (igwID == "") == (vgwID == "") {
				return "", nil, awserror.New(
					"ValidationError",
					"Exactly one of InternetGatewayId or VpnGatewayId is required",
				)
			}

			if igwID != "" {
				_, err := c.call(ctx, "ec2", "AttachInternetGateway", map[string]any{
					memberVpcID:             vpcID,
					memberInternetGatewayID: igwID,
				})
				if err != nil {
					return "", nil, err
				}
			}

			if vgwID != "" {
				_, err := c.call(ctx, "ec2", "AttachVpnGateway", map[string]any{
					memberVpcID:        vpcID,
					memberVpnGatewayID: vgwID,
				})
				if err != nil {
					return "", nil, err
				}
			}

			return vpcID + "_" + cmp.Or(igwID, vgwID), map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			vpcID := props(r.Properties).str(memberVpcID)
			igwID := props(r.Properties).str(memberInternetGatewayID)
			vgwID := props(r.Properties).str(memberVpnGatewayID)

			if igwID != "" {
				_, err := c.call(ctx, "ec2", "DetachInternetGateway", map[string]any{
					memberVpcID:             vpcID,
					memberInternetGatewayID: igwID,
				})
				if err != nil && !gone(err, "InvalidInternetGatewayID.NotFound", "Gateway.NotAttached") {
					return err
				}
			}

			if vgwID != "" {
				_, err := c.call(ctx, "ec2", "DetachVpnGateway", map[string]any{
					memberVpcID:        vpcID,
					memberVpnGatewayID: vgwID,
				})
				if err != nil && !gone(err, "InvalidVpnGatewayID.NotFound", "Gateway.NotAttached") {
					return err
				}
			}

			return nil
		},
	})

	register("AWS::EC2::VPNConnection", &resourceType{
		properties: []string{
			memberCustomerGatewayID, memberType, memberVpnGatewayID, memberTransitGatewayID,
			"StaticRoutesOnly", "VpnTunnelOptionsSpecifications", memberTags,
		},
		replace: []string{
			memberCustomerGatewayID, memberType, memberVpnGatewayID, memberTransitGatewayID,
			"StaticRoutesOnly", "VpnTunnelOptionsSpecifications",
		},
		attributes: []string{memberVpnConnectionID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberCustomerGatewayID: p.str(memberCustomerGatewayID),
				memberType:              p.str(memberType),
				memberTagSpecifications: tagSpecifications("vpn-connection", p),
			}
			if p.has(memberVpnGatewayID) {
				params[memberVpnGatewayID] = p.str(memberVpnGatewayID)
			}

			if p.has(memberTransitGatewayID) {
				params[memberTransitGatewayID] = p.str(memberTransitGatewayID)
			}

			options := map[string]any{}
			if p.has("StaticRoutesOnly") {
				options["StaticRoutesOnly"] = p.truth("StaticRoutesOnly")
			}

			if p.has("VpnTunnelOptionsSpecifications") {
				options["TunnelOptions"] = p["VpnTunnelOptionsSpecifications"]
			}

			if len(options) > 0 {
				params["Options"] = options
			}

			out, err := c.call(ctx, "ec2", "CreateVpnConnection", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "VpnConnection", memberVpnConnectionID)
			return id, map[string]any{memberVpnConnectionID: id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteVpnConnection",
				memberVpnConnectionID,
				r.Physical,
				"InvalidVpnConnectionID.NotFound",
			)
		},
	})

	register("AWS::EC2::VPNConnectionRoute", &resourceType{
		properties: []string{memberVpnConnectionID, memberDestinationCidrBlock},
		replace:    []string{memberVpnConnectionID, memberDestinationCidrBlock},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			vpnID := p.str(memberVpnConnectionID)
			cidr := p.str(memberDestinationCidrBlock)
			params := map[string]any{
				memberVpnConnectionID:      vpnID,
				memberDestinationCidrBlock: cidr,
			}
			_, err := c.call(ctx, "ec2", "CreateVpnConnectionRoute", params)
			if err != nil {
				return "", nil, err
			}

			return vpnID + "_" + cidr, map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			vpnID := props(r.Properties).str(memberVpnConnectionID)
			cidr := props(r.Properties).str(memberDestinationCidrBlock)
			_, err := c.call(ctx, "ec2", "DeleteVpnConnectionRoute", map[string]any{
				memberVpnConnectionID:      vpnID,
				memberDestinationCidrBlock: cidr,
			})
			if gone(err, "InvalidVpnConnectionID.NotFound", "InvalidRoute.NotFound") {
				return nil
			}

			return err
		},
	})

	register("AWS::EC2::VPNGatewayRoutePropagation", &resourceType{
		properties: []string{memberVpnGatewayID, "RouteTableIds"},
		replace:    []string{memberVpnGatewayID, "RouteTableIds"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			vgwID := p.str(memberVpnGatewayID)
			tableIDs := p.strings("RouteTableIds")
			for _, rtbID := range tableIDs {
				_, err := c.call(ctx, "ec2", "EnableVgwRoutePropagation", map[string]any{
					"GatewayId":    vgwID,
					"RouteTableId": rtbID,
				})
				if err != nil {
					return "", nil, err
				}
			}

			return vgwID + "_" + strings.Join(tableIDs, ","), map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			vgwID := props(r.Properties).str(memberVpnGatewayID)
			tableIDs := props(r.Properties).strings("RouteTableIds")
			for _, rtbID := range tableIDs {
				_, err := c.call(ctx, "ec2", "DisableVgwRoutePropagation", map[string]any{
					"GatewayId":    vgwID,
					"RouteTableId": rtbID,
				})
				if err != nil && !gone(err, "InvalidRouteTableID.NotFound", "InvalidVpnGatewayID.NotFound") {
					return err
				}
			}

			return nil
		},
	})

	register("AWS::EC2::InternetGateway", &resourceType{
		properties: []string{memberTags},
		attributes: []string{memberInternetGatewayID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberTagSpecifications: tagSpecifications("internet-gateway", p),
			}
			out, err := c.call(ctx, "ec2", "CreateInternetGateway", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "InternetGateway", memberInternetGatewayID)
			return id, map[string]any{memberInternetGatewayID: id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteInternetGateway",
				memberInternetGatewayID,
				r.Physical,
				"InvalidInternetGatewayID.NotFound",
			)
		},
	})
}

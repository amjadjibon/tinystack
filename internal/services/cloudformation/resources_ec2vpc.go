package cloudformation

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

func init() {
	register("AWS::EC2::DHCPOptions", &resourceType{
		properties: []string{
			memberDomainName,
			wireDomainNameServers,
			wireNtpServers,
			wireNetbiosNameServers,
			wireNetbiosNodeType,
			wireIpv6AddressPreferredLeaseTime,
			memberTags,
		},
		replace: []string{
			memberDomainName,
			wireDomainNameServers,
			wireNtpServers,
			wireNetbiosNameServers,
			wireNetbiosNodeType,
			wireIpv6AddressPreferredLeaseTime,
		},
		attributes: []string{wireDhcpOptionsId},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			var configurations []any
			for property, key := range map[string]string{
				memberDomainName: "domain-name", wireDomainNameServers: "domain-name-servers",
				wireNtpServers:                    "ntp-servers",
				wireNetbiosNameServers:            "netbios-name-servers",
				wireNetbiosNodeType:               "netbios-node-type",
				wireIpv6AddressPreferredLeaseTime: "ipv6-address-preferred-lease-time",
			} {
				if !p.has(property) {
					continue
				}

				values := p.list(property)
				if values == nil {
					values = []any{fmt.Sprint(p[property])}
				}

				configurations = append(configurations, map[string]any{"Key": key, wireValues: values})
			}

			out, err := c.call(ctx, "ec2", "CreateDhcpOptions", map[string]any{
				"DhcpConfigurations": configurations,
				"TagSpecifications":  tagSpecifications("dhcp-options", p),
			})
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "DhcpOptions", wireDhcpOptionsId)
			return id, map[string]any{wireDhcpOptionsId: id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteDhcpOptions",
				wireDhcpOptionsId,
				r.Physical,
				"InvalidDhcpOptionID.NotFound",
			)
		},
	})
	register("AWS::EC2::VPCDHCPOptionsAssociation", &resourceType{
		properties: []string{memberVpcID, wireDhcpOptionsId},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			_, err := c.call(
				ctx,
				"ec2",
				"AssociateDhcpOptions",
				pick(map[string]any{}, p, memberVpcID, wireDhcpOptionsId),
			)
			return p.str(memberVpcID) + "/" + p.str(wireDhcpOptionsId), map[string]any{}, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				"ec2",
				"AssociateDhcpOptions",
				map[string]any{memberVpcID: props(r.Properties).str(memberVpcID), wireDhcpOptionsId: "default"},
			)
			if gone(err, "InvalidVpcID.NotFound") {
				return nil
			}

			return err
		},
	})
	register("AWS::EC2::EgressOnlyInternetGateway", &resourceType{
		properties: []string{memberVpcID, memberTags}, replace: []string{memberVpcID}, attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "ec2", "CreateEgressOnlyInternetGateway", map[string]any{
				memberVpcID:         p.str(memberVpcID),
				"TagSpecifications": tagSpecifications("egress-only-internet-gateway", p),
			})
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "EgressOnlyInternetGateway", "EgressOnlyInternetGatewayId")
			return id, map[string]any{"Id": id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteEgressOnlyInternetGateway",
				"EgressOnlyInternetGatewayId",
				r.Physical,
				"InvalidEgressOnlyInternetGatewayId.NotFound",
			)
		},
	})
	register("AWS::EC2::VPCCidrBlock", &resourceType{
		properties: []string{
			memberVpcID,
			memberCidrBlock,
			"AmazonProvidedIpv6CidrBlock",
			"Ipv6CidrBlock",
			"Ipv6CidrBlockNetworkBorderGroup",
			"Ipv6Pool",
		},
		attributes: []string{"Id", wireIpv6AddressAttribute, wireIpSource},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "ec2", "AssociateVpcCidrBlock", pick(
				map[string]any{},
				p,
				memberVpcID,
				memberCidrBlock,
				"AmazonProvidedIpv6CidrBlock",
				"Ipv6CidrBlock",
				"Ipv6CidrBlockNetworkBorderGroup",
				"Ipv6Pool",
			))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "CidrBlockAssociation", "AssociationId")
			if id == "" {
				id = outText(out, "Ipv6CidrBlockAssociation", "AssociationId")
			}

			return id, map[string]any{
				"Id": id, wireIpv6AddressAttribute: outText(out, "Ipv6CidrBlockAssociation", wireIpv6AddressAttribute),
				wireIpSource: outText(out, "Ipv6CidrBlockAssociation", wireIpSource),
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DisassociateVpcCidrBlock",
				"AssociationId",
				r.Physical,
				"InvalidAssociationID.NotFound",
			)
		},
	})
	register("AWS::EC2::SubnetCidrBlock", &resourceType{
		properties: []string{
			memberSubnetID,
			"Ipv6CidrBlock",
		},
		attributes: []string{"Id", wireIpv6AddressAttribute, wireIpSource},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				"ec2",
				"AssociateSubnetCidrBlock",
				pick(map[string]any{}, p, memberSubnetID, "Ipv6CidrBlock"),
			)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Ipv6CidrBlockAssociation", "AssociationId")
			return id, map[string]any{
				"Id": id, wireIpv6AddressAttribute: outText(out, "Ipv6CidrBlockAssociation", wireIpv6AddressAttribute),
				wireIpSource: outText(out, "Ipv6CidrBlockAssociation", wireIpSource),
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DisassociateSubnetCidrBlock",
				"AssociationId",
				r.Physical,
				"InvalidAssociationID.NotFound",
			)
		},
	})
	register("AWS::EC2::NetworkInterface", &resourceType{
		properties: []string{
			memberSubnetID,
			memberDescription,
			"PrivateIpAddress",
			"PrivateIpAddresses",
			"SecondaryPrivateIpAddressCount",
			wireGroupSet,
			wireSourceDestCheck,
			"InterfaceType",
			"Ipv6Addresses",
			"Ipv6AddressCount",
			memberTags,
		},
		replace: []string{memberSubnetID, "PrivateIpAddress", "InterfaceType"},
		attributes: []string{
			"Id",
			memberVpcID,
			"PrimaryPrivateIpAddress",
			"SecondaryPrivateIpAddresses",
			"PrimaryIpv6Address",
		},
		create: createEC2Interface,
		update: updateEC2Interface,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteNetworkInterface",
				wireNetworkInterfaceId,
				r.Physical,
				"InvalidNetworkInterfaceID.NotFound",
			)
		},
	})
	register("AWS::EC2::NetworkInterfaceAttachment", &resourceType{
		properties: []string{wireNetworkInterfaceId, "InstanceId", "DeviceIndex", wireDeleteOnTermination},
		replace: []string{
			wireNetworkInterfaceId,
			"InstanceId",
			"DeviceIndex",
		},
		attributes: []string{wireAttachmentId},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				"ec2",
				"AttachNetworkInterface",
				pick(map[string]any{}, p, wireNetworkInterfaceId, "InstanceId", "DeviceIndex"),
			)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, wireAttachmentId)
			if p.has(wireDeleteOnTermination) {
				_, err = c.call(ctx, "ec2", "ModifyNetworkInterfaceAttribute", map[string]any{
					wireNetworkInterfaceId: p.str(wireNetworkInterfaceId),
					"Attachment": map[string]any{
						wireAttachmentId:        id,
						wireDeleteOnTermination: p[wireDeleteOnTermination],
					},
				})
				if err != nil {
					_, _ = c.call(ctx, "ec2", "DetachNetworkInterface", map[string]any{wireAttachmentId: id})
					return "", nil, err
				}
			}

			return id, map[string]any{wireAttachmentId: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, wireDeleteOnTermination) {
				value := any(false)
				if p.has(wireDeleteOnTermination) {
					value = p[wireDeleteOnTermination]
				}

				_, err := c.call(ctx, "ec2", "ModifyNetworkInterfaceAttribute", map[string]any{
					wireNetworkInterfaceId: p.str(wireNetworkInterfaceId),
					"Attachment": map[string]any{
						wireAttachmentId:        r.Physical,
						wireDeleteOnTermination: value,
					},
				})
				if err != nil {
					return nil, err
				}
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DetachNetworkInterface",
				wireAttachmentId,
				r.Physical,
				"InvalidAttachmentID.NotFound",
			)
		},
	})
	register("AWS::EC2::FlowLog", &resourceType{
		properties: []string{
			applicationResource,
			wireResourceType,
			"TrafficType",
			"DeliverLogsPermissionArn",
			"DeliverCrossAccountRole",
			"LogGroupName",
			"LogDestinationType",
			"LogDestination",
			"LogFormat",
			"MaxAggregationInterval",
			"DestinationOptions",
			memberTags,
		},
		replace: []string{
			applicationResource,
			wireResourceType,
			"TrafficType",
			"DeliverLogsPermissionArn",
			"DeliverCrossAccountRole",
			"LogGroupName",
			"LogDestinationType",
			"LogDestination",
			"LogFormat",
			"MaxAggregationInterval",
			"DestinationOptions",
		},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{"ResourceIds": []any{p.str(applicationResource)}},
				p,
				wireResourceType,
				"TrafficType",
				"DeliverLogsPermissionArn",
				"DeliverCrossAccountRole",
				"LogGroupName",
				"LogDestinationType",
				"LogDestination",
				"LogFormat",
				"MaxAggregationInterval",
				"DestinationOptions",
			)
			params["TagSpecifications"] = tagSpecifications("vpc-flow-log", p)
			out, err := c.call(ctx, "ec2", "CreateFlowLogs", params)
			if err != nil {
				return "", nil, err
			}

			ids, _ := out["FlowLogIds"].([]any)
			if len(ids) != 1 {
				return "", nil, awserror.Validation("CreateFlowLogs did not create one flow log")
			}

			id, _ := ids[0].(string)
			return id, map[string]any{"Id": id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ec2", "DeleteFlowLogs", map[string]any{"FlowLogIds": []any{r.Physical}})
			return err
		},
	})
	register("AWS::EC2::PrefixList", &resourceType{
		properties: []string{"PrefixListName", "AddressFamily", "MaxEntries", "Entries", memberTags},
		attributes: []string{wirePrefixListId, memberArn, memberVersion, "OwnerId"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, "PrefixListName", "AddressFamily", "MaxEntries", "Entries")
			params["TagSpecifications"] = tagSpecifications("prefix-list", p)
			out, err := c.call(ctx, "ec2", "CreateManagedPrefixList", params)
			if err != nil {
				return "", nil, err
			}

			return outText(out, "PrefixList", wirePrefixListId), ec2PrefixAttributes(out), nil
		},
		update: updateEC2Prefix,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteManagedPrefixList",
				wirePrefixListId,
				r.Physical,
				"InvalidPrefixListID.NotFound",
			)
		},
	})
	register("AWS::EC2::VPCPeeringConnection", &resourceType{
		properties: []string{memberVpcID, "PeerVpcId", "PeerOwnerId", "PeerRegion", memberTags},
		replace:    []string{memberVpcID, "PeerVpcId", "PeerOwnerId", "PeerRegion"}, attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberVpcID, "PeerVpcId", "PeerOwnerId", "PeerRegion")
			params["TagSpecifications"] = tagSpecifications("vpc-peering-connection", p)
			out, err := c.call(ctx, "ec2", "CreateVpcPeeringConnection", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "VpcPeeringConnection", "VpcPeeringConnectionId")
			if p.str("PeerOwnerId") == "" || p.str("PeerOwnerId") == c.scope.AccountID {
				_, err = c.call(ctx, "ec2", "AcceptVpcPeeringConnection", map[string]any{"VpcPeeringConnectionId": id})
				if err != nil {
					_, _ = c.call(
						ctx,
						"ec2",
						"DeleteVpcPeeringConnection",
						map[string]any{"VpcPeeringConnectionId": id},
					)
					return "", nil, err
				}
			}

			return id, map[string]any{"Id": id}, nil
		},
		update: updateEC2Tags,
		remove: func(ctx context.Context, c *client, r *resource) error {
			return removeEC2Record(
				ctx,
				c,
				"DeleteVpcPeeringConnection",
				"VpcPeeringConnectionId",
				r.Physical,
				"InvalidVpcPeeringConnectionID.NotFound",
			)
		},
	})
}

func updateEC2Tags(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	return r.Attributes, retagEC2(ctx, c, r.Physical, r.Properties, p)
}

// removeEC2Record treats already-absent records as successful deletion for stack cleanup.
func removeEC2Record(ctx context.Context, c *client, operation, member, id, missing string) error {
	_, err := c.call(ctx, "ec2", operation, map[string]any{member: id})
	if gone(err, missing) {
		return nil
	}

	return err
}

func createEC2Interface(
	ctx context.Context,
	c *client,
	p props,
) (physical string, attributes map[string]any, err error) {
	params := pick(map[string]any{}, p, memberSubnetID, memberDescription, "PrivateIpAddress", "PrivateIpAddresses",
		"SecondaryPrivateIpAddressCount", "InterfaceType", "Ipv6Addresses", "Ipv6AddressCount")
	if p.has(wireGroupSet) {
		params["Groups"] = p[wireGroupSet]
	}

	params["TagSpecifications"] = tagSpecifications("network-interface", p)
	out, err := c.call(ctx, "ec2", "CreateNetworkInterface", params)
	if err != nil {
		return "", nil, err
	}

	id := outText(out, "NetworkInterface", wireNetworkInterfaceId)
	if p.has(wireSourceDestCheck) {
		_, err = c.call(ctx, "ec2", "ModifyNetworkInterfaceAttribute", map[string]any{
			wireNetworkInterfaceId: id,
			wireSourceDestCheck:    map[string]any{memberValue: p[wireSourceDestCheck]},
		})
		if err != nil {
			_ = removeEC2Record(
				ctx,
				c,
				"DeleteNetworkInterface",
				wireNetworkInterfaceId,
				id,
				"InvalidNetworkInterfaceID.NotFound",
			)
			return "", nil, err
		}
	}

	return id, ec2InterfaceAttributes(out), nil
}

func ec2InterfaceAttributes(out map[string]any) map[string]any {
	ni, _ := out["NetworkInterface"].(map[string]any)
	var secondary []any
	ips, _ := ni["PrivateIpAddresses"].([]any)
	for _, value := range ips {
		ip, _ := value.(map[string]any)
		if ip["Primary"] != true {
			secondary = append(secondary, ip["PrivateIpAddress"])
		}
	}

	return map[string]any{
		"Id": ni[wireNetworkInterfaceId], memberVpcID: ni[memberVpcID],
		"PrimaryPrivateIpAddress": ni["PrivateIpAddress"], "SecondaryPrivateIpAddresses": secondary,
	}
}

func updateEC2Interface(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	// Address updates require assignment APIs beyond Tier 1's IPv4 API; fail before changing other attributes.
	for _, key := range []string{
		"PrivateIpAddresses",
		"SecondaryPrivateIpAddressCount",
		"Ipv6Addresses",
		"Ipv6AddressCount",
	} {
		if p.changed(r.Properties, key) {
			return nil, awserror.Validation("TinyStack does not support updating NetworkInterface.%s", key)
		}
	}

	for _, key := range []string{memberDescription, wireSourceDestCheck, wireGroupSet} {
		if !p.changed(r.Properties, key) {
			continue
		}

		params := map[string]any{wireNetworkInterfaceId: r.Physical}
		switch key {
		case memberDescription:
			params[key] = map[string]any{memberValue: p.str(key)}
		case wireSourceDestCheck:
			value := any(true)
			if p.has(key) {
				value = p[key]
			}

			params[key] = map[string]any{memberValue: value}
		case wireGroupSet:
			if len(p.list(key)) == 0 {
				return nil, awserror.Validation("GroupSet must contain at least one security group")
			}

			params["Groups"] = p[key]
		}

		if _, err := c.call(ctx, "ec2", "ModifyNetworkInterfaceAttribute", params); err != nil {
			return nil, err
		}
	}

	return r.Attributes, retagEC2(ctx, c, r.Physical, r.Properties, p)
}

func ec2PrefixAttributes(out map[string]any) map[string]any {
	pl, _ := out["PrefixList"].(map[string]any)
	return map[string]any{
		wirePrefixListId: pl[wirePrefixListId],
		memberArn:        pl["PrefixListArn"],
		memberVersion:    pl[memberVersion],
		"OwnerId":        pl["OwnerId"],
	}
}

func updateEC2Prefix(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	if p.changed(r.Properties, "AddressFamily") {
		return nil, awserror.Validation("TinyStack does not support updating PrefixList.AddressFamily")
	}

	if p.changed(r.Properties, "MaxEntries") {
		if _, err := c.call(
			ctx,
			"ec2",
			"ModifyManagedPrefixList",
			map[string]any{wirePrefixListId: r.Physical, "MaxEntries": p["MaxEntries"]},
		); err != nil {
			return nil, err
		}
	}

	params := map[string]any{wirePrefixListId: r.Physical}
	if p.changed(r.Properties, "PrefixListName") {
		params["PrefixListName"] = p["PrefixListName"]
	}

	if p.changed(r.Properties, "Entries") {
		params["CurrentVersion"] = r.Attributes[memberVersion]
		var added, removed []any
		for _, value := range props(r.Properties).list("Entries") {
			if slices.ContainsFunc(
				p.list("Entries"),
				func(candidate any) bool { return reflect.DeepEqual(value, candidate) },
			) {
				continue
			}

			entry, _ := value.(map[string]any)
			removed = append(removed, map[string]any{"Cidr": entry["Cidr"]})
		}

		for _, value := range p.list("Entries") {
			if !slices.ContainsFunc(
				props(r.Properties).list("Entries"),
				func(candidate any) bool { return reflect.DeepEqual(value, candidate) },
			) {
				added = append(added, value)
			}
		}

		params["AddEntries"], params["RemoveEntries"] = added, removed
	}

	out, err := c.call(ctx, "ec2", "ModifyManagedPrefixList", params)
	if err != nil {
		return nil, err
	}

	if err := retagEC2(ctx, c, r.Physical, r.Properties, p); err != nil {
		return nil, err
	}

	return ec2PrefixAttributes(out), nil
}

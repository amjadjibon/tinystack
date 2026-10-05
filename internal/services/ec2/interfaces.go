package ec2

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// managedInterface is a network interface AWS manages for a NAT gateway or
// an interface endpoint. TinyStack keeps no records of its own for them; they
// are derived from their owners'.
type managedInterface struct {
	id          string
	subnetID    string
	vpcID       string
	zone        string
	privateIP   string
	kind        string
	description string
	groups      []string
}

// managedInterfaces lists the interfaces of NAT gateways and interface
// endpoints in scope, ordered by ID.
func managedInterfaces(tx storage.ReadTx, scope request.Scope) ([]*managedInterface, error) {
	gateways, err := list[natGateway](tx, scope, kindNATGateway)
	if err != nil {
		return nil, err
	}

	endpoints, err := list[vpcEndpoint](tx, scope, kindVPCEndpoint)
	if err != nil {
		return nil, err
	}

	var out []*managedInterface
	for _, g := range gateways {
		if !g.active() {
			continue
		}

		sn, _, readErr := storage.Read[subnet](tx, key(scope, kindSubnet, g.SubnetID))
		if readErr != nil {
			return nil, readErr
		}

		ni := &managedInterface{
			id: g.InterfaceID, subnetID: g.SubnetID, vpcID: g.VPCID, privateIP: g.PrivateIP, kind: "nat_gateway",
			description: "Interface for NAT Gateway " + g.ID,
		}
		if sn != nil {
			ni.zone = sn.Zone
		}

		out = append(out, ni)
	}

	for _, ep := range endpoints {
		for _, sn := range ep.Subnets {
			out = append(out, &managedInterface{
				id: sn.InterfaceID, subnetID: sn.ID, vpcID: ep.VPCID, zone: sn.Zone, privateIP: sn.PrivateIP,
				kind: "vpc_endpoint", description: "VPC Endpoint Interface " + ep.ID, groups: ep.GroupIDs,
			})
		}
	}

	slices.SortFunc(out, func(a, b *managedInterface) int { return strings.Compare(a.id, b.id) })
	return out, nil
}

// macAddress derives an interface's MAC address from its ID.
func macAddress(id string) string {
	hexID := strings.TrimPrefix(id, "eni-")
	parts := []string{"02"}
	for i := 0; i+2 <= len(hexID) && len(parts) < 6; i += 2 {
		parts = append(parts, hexID[i:i+2])
	}

	return strings.Join(parts, ":")
}

func interfaceOutput(tx storage.ReadTx, scope request.Scope, ni *managedInterface) (api.NetworkInterface, error) {
	out := api.NetworkInterface{
		NetworkInterfaceId: new(ni.id), SubnetId: new(ni.subnetID), VpcId: new(ni.vpcID),
		AvailabilityZone: new(ni.zone), AvailabilityZoneId: new(zoneID(scope.Region, strings.TrimPrefix(ni.zone,
			scope.Region))), Description: new(ni.description), InterfaceType: new(ni.kind),
		MacAddress: new(macAddress(ni.id)), OwnerId: new(scope.AccountID), PrivateIpAddress: new(ni.privateIP),
		RequesterManaged: new(true), SourceDestCheck: new(true), Status: new("in-use"),
		Groups: []api.GroupIdentifier{},
	}
	for _, id := range ni.groups {
		g, ok, err := storage.Read[securityGroup](tx, key(scope, kindSecurityGroup, id))
		if err != nil {
			return out, err
		}

		if ok {
			out.Groups = append(out.Groups, api.GroupIdentifier{GroupId: new(g.ID), GroupName: new(g.Name)})
		}
	}

	return out, nil
}

func interfaceValues(scope request.Scope, ni *managedInterface) values {
	return values{
		"network-interface-id": {ni.id}, filterGroupID: slices.Clone(ni.groups), filterSubnetID: {ni.subnetID},
		filterVPCID: {ni.vpcID}, "status": {"in-use"}, filterDescription: {ni.description}, filterZone: {ni.zone},
		"attachment.instance-id": {}, filterOwnerID: {scope.AccountID}, filterPrivateIP: {ni.privateIP},
		"interface-type": {ni.kind}, "requester-managed": {strconv.FormatBool(true)},
	}
}

// DescribeNetworkInterfaces lists the network interfaces of NAT gateways and
// interface endpoints. Instances have none.
func (s *Service) DescribeNetworkInterfaces(
	ctx context.Context,
	in *api.DescribeNetworkInterfacesRequest,
) (*api.DescribeNetworkInterfacesResult, error) {
	match, err := compileFilters(in.Filters, interfaceValues(request.Scope{}, &managedInterface{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeNetworkInterfacesResult{NetworkInterfaces: []api.NetworkInterface{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := managedInterfaces(tx, scope)
		if listErr != nil {
			return listErr
		}

		id := func(ni *managedInterface) string { return ni.id }
		selected, selectErr := selectIDs(all, id, in.NetworkInterfaceIds, func(id string) error {
			return notFound("InvalidNetworkInterfaceID.NotFound", "network interface", id)
		})
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(ni *managedInterface) bool {
			return !match.match(interfaceValues(scope, ni))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		if pageErr != nil {
			return pageErr
		}

		for _, ni := range selected {
			item, outErr := interfaceOutput(tx, scope, ni)
			if outErr != nil {
				return fmt.Errorf("describe %s: %w", ni.id, outErr)
			}

			out.NetworkInterfaces = append(out.NetworkInterfaces, item)
		}

		out.NextToken = next
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

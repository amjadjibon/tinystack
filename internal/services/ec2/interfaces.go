package ec2

import (
	"context"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
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

		sn, _, readErr := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, g.SubnetID))
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
		RequesterManaged: new(true), SourceDestCheck: new(true), Status: new(ec2InUse),
		Groups: []api.GroupIdentifier{},
	}

	for _, id := range ni.groups {
		g, ok, err := storage.Read[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, id))
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
		ec2NetworkInterfaceId: {ni.id}, filterGroupID: slices.Clone(ni.groups), filterSubnetID: {ni.subnetID},
		filterVPCID: {ni.vpcID}, "status": {ec2InUse}, filterDescription: {ni.description}, filterZone: {ni.zone},
		"attachment.instance-id": {}, filterOwnerID: {scope.AccountID}, filterPrivateIP: {ni.privateIP},
		"interface-type": {ni.kind}, "requester-managed": {strconv.FormatBool(true)},
	}
}

// DescribeNetworkInterfaces combines persisted customer/instance ENIs with service-managed interfaces.
func (s *Service) DescribeNetworkInterfaces(ctx context.Context, in *api.DescribeNetworkInterfacesRequest) (
	*api.DescribeNetworkInterfacesResult, error,
) {
	match, err := compileFilters(in.Filters, eniValues(request.Scope{}, &networkInterface{}).names()...)
	if err != nil {
		return nil, err
	}

	if len(in.NetworkInterfaceIds) > 0 && in.MaxResults != nil {
		return nil, invalidParameter("MaxResults cannot be combined with IDs")
	}

	out := &api.DescribeNetworkInterfacesResult{NetworkInterfaces: []api.NetworkInterface{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		records, recordsErr := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
		if recordsErr != nil {
			return recordsErr
		}

		managed, recordsErr := managedInterfaces(tx, scope)
		if recordsErr != nil {
			return recordsErr
		}

		var all []api.NetworkInterface
		existing := map[string]bool{}
		for _, ni := range records {
			existing[ni.ID] = true
			if !match.match(eniValues(scope, ni)) {
				continue
			}

			item, eniOutputErr := eniOutput(tx, scope, ni)
			if eniOutputErr != nil {
				return eniOutputErr
			}

			all = append(all, item)
		}

		for _, ni := range managed {
			existing[ni.id] = true
			if !match.match(interfaceValues(scope, ni)) {
				continue
			}

			item, interfaceOutputErr := interfaceOutput(tx, scope, ni)
			if interfaceOutputErr != nil {
				return interfaceOutputErr
			}

			all = append(all, item)
		}

		for _, id := range in.NetworkInterfaceIds {
			if !existing[id] {
				return eniNotFound(id)
			}
		}

		id := func(ni api.NetworkInterface) string { return common.Value(ni.NetworkInterfaceId) }
		all = slices.DeleteFunc(all, func(ni api.NetworkInterface) bool {
			return len(in.NetworkInterfaceIds) > 0 && !slices.Contains(in.NetworkInterfaceIds, id(ni))
		})
		slices.SortFunc(all, func(a, b api.NetworkInterface) int { return strings.Compare(id(a), id(b)) })
		limit := len(all) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 5 || *in.MaxResults > maxDescribe {
				return invalidParameter("MaxResults must be 5..1000")
			}

			limit = int(*in.MaxResults)
		}

		out.NetworkInterfaces, out.NextToken, recordsErr = common.Page(
			all,
			id,
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		return recordsErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

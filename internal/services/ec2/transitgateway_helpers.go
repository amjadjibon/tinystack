package ec2

import (
	"net/netip"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func validateTransitGatewayEnums(options ...*string) error {
	for _, option := range options {
		if option != nil && *option != stateEnable && *option != stateDisable {
			return invalidParameter("Transit gateway options must be enable or disable")
		}
	}

	return nil
}

func validAmazonASN(asn int64) bool {
	return asn >= 64512 && asn <= 65534 || asn >= 4200000000 && asn <= 4294967294
}

func validateTransitGatewayCIDRs(cidrs []string) error {
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Addr().Is4() && prefix.Bits() > 24 || prefix.Addr().Is6() && prefix.Bits() > 64 {
			return invalidParameter(
				"Transit gateway CIDR must be a valid IPv4 /24 or larger or IPv6 /64 or larger block",
			)
		}
	}

	return nil
}

func validateTransitGatewaySubnets(tx storage.ReadTx, scope request.Scope, vpcID string, ids []string) error {
	if len(ids) == 0 {
		return invalidParameter("At least one attachment subnet is required")
	}

	zones := map[string]bool{}
	for _, id := range ids {
		sn, err := loadSubnet(tx, scope, id)
		if err != nil {
			return err
		}

		if sn.VPCID != vpcID || zones[sn.Zone] {
			return invalidParameter("Attachment subnets must belong to the VPC and occupy distinct Availability Zones")
		}

		zones[sn.Zone] = true
	}

	return nil
}

// syncTransitGatewayDefaultTables validates references and updates flags in the same transaction as the gateway.
func (s *Service) syncTransitGatewayDefaultTables(
	tx storage.WriteTx, scope request.Scope, gateway *transitGateway,
) error {
	opts := &gateway.Options
	for _, id := range []string{opts.AssociationDefaultRouteTableID, opts.PropagationDefaultRouteTableID} {
		if id == "" {
			continue
		}

		table, found, err := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, id),
		)
		if err != nil {
			return err
		}

		if !found || table.State == stateDeleted {
			return tgwRouteTableNotFound(id)
		}

		if table.TransitGatewayID != gateway.ID {
			return invalidParameter("Default route table belongs to another transit gateway")
		}
	}

	if opts.DefaultRouteTableAssociation == stateEnable && opts.AssociationDefaultRouteTableID == "" ||
		opts.DefaultRouteTablePropagation == stateEnable && opts.PropagationDefaultRouteTableID == "" {
		id := opts.AssociationDefaultRouteTableID
		if id == "" {
			id = opts.PropagationDefaultRouteTableID
		}

		if id == "" {
			id = newID("tgw-rtb")
			table := &transitGatewayRouteTable{
				ID:               id,
				TransitGatewayID: gateway.ID,
				State:            stateAvailable,
				Created:          s.now().UTC(),
			}
			if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayRouteTable, id), table); err != nil {
				return err
			}
		}

		if opts.DefaultRouteTableAssociation == stateEnable && opts.AssociationDefaultRouteTableID == "" {
			opts.AssociationDefaultRouteTableID = id
		}

		if opts.DefaultRouteTablePropagation == stateEnable && opts.PropagationDefaultRouteTableID == "" {
			opts.PropagationDefaultRouteTableID = id
		}
	}

	tables, err := list[transitGatewayRouteTable](tx, scope, kindTransitGatewayRouteTable)
	if err != nil {
		return err
	}

	for _, table := range tables {
		if table.TransitGatewayID != gateway.ID || table.State == stateDeleted {
			continue
		}

		table.DefaultAssociationRouteTable = table.ID == opts.AssociationDefaultRouteTableID
		table.DefaultPropagationRouteTable = table.ID == opts.PropagationDefaultRouteTableID
		if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayRouteTable, table.ID), table); err != nil {
			return err
		}
	}

	return nil
}

// deleteTransitGatewayAttachment removes routing relationships atomically for both VPC and VPN attachments.
func deleteTransitGatewayAttachment(
	tx storage.WriteTx,
	scope request.Scope,
	attachment *transitGatewayAttachment,
) error {
	attachment.State = stateDeleted
	attachment.AssociationTableID = ""
	props, err := list[transitGatewayPropagationRecord](tx, scope, kindTransitGatewayPropagation)
	if err != nil {
		return err
	}

	for _, prop := range props {
		if prop.AttachmentID == attachment.ID {
			tx.Delete(scope.Key("ec2", kindTransitGatewayPropagation, prop.RouteTableID+"_"+attachment.ID))
		}
	}

	routes, err := list[transitGatewayRouteRecord](tx, scope, kindTransitGatewayRoute)
	if err != nil {
		return err
	}

	for _, route := range routes {
		if route.AttachmentID != attachment.ID {
			continue
		}

		route.State = routeBlackhole
		route.Blackhole = true
		route.AttachmentID = ""
		route.ResourceID = ""
		route.ResourceType = ""
		if err := storage.Write(
			tx,
			scope.Key("ec2", kindTransitGatewayRoute, route.RouteTableID+"_"+route.DestinationCIDR),
			route,
		); err != nil {
			return err
		}
	}

	return storage.Write(tx, scope.Key("ec2", kindTransitGatewayAttachment, attachment.ID), attachment)
}

func invalidTransitGatewayFilter() error {
	return awserror.New("InvalidParameterValue", "Unsupported transit gateway route search filter")
}

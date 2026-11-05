package ec2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindNATGateway      = "nat-gateway"
	connectivityPublic  = "public"
	connectivityPrivate = "private"
	stateDeleted        = "deleted"
	// deletedFor is how long a deleted NAT gateway stays visible.
	deletedFor = time.Hour
)

// natGateway is a NAT gateway in a subnet, available at once. A public one
// holds an Elastic IP address. A deleted one stays visible for an hour.
type natGateway struct {
	Created      time.Time
	Deleted      time.Time         `json:",omitzero"`
	Tags         map[string]string `json:",omitempty"`
	ID           string
	SubnetID     string
	VPCID        string
	Connectivity string
	PrivateIP    string
	InterfaceID  string
	AddressID    string `json:",omitempty"`
	PublicIP     string `json:",omitempty"`
	Association  string `json:",omitempty"`
	State        string
}

func natGatewayNotFound(id string) error {
	return awserror.New("NatGatewayNotFound", fmt.Sprintf("NAT gateway %s was not found", id))
}

func (g *natGateway) active() bool { return g.State != stateDeleted }

func (g *natGateway) visible(now time.Time) bool {
	return g.active() || now.Sub(g.Deleted) < deletedFor
}

func natGatewayOutput(g *natGateway, state string) api.NatGateway {
	out := api.NatGateway{
		NatGatewayId: new(g.ID), SubnetId: new(g.SubnetID), VpcId: new(g.VPCID), State: new(state),
		ConnectivityType: new(g.Connectivity), CreateTime: new(g.Created), Tags: tagList(g.Tags),
		NatGatewayAddresses: []api.NatGatewayAddress{{
			NetworkInterfaceId: new(g.InterfaceID), PrivateIp: new(g.PrivateIP), IsPrimary: new(true),
			AllocationId: optional(g.AddressID), PublicIp: optional(g.PublicIP), AssociationId: optional(g.Association),
			Status: new("succeeded"),
		}},
	}
	if !g.active() {
		out.DeleteTime = new(g.Deleted)
	}

	return out
}

func natGatewayValues(g *natGateway) values {
	return values{
		"nat-gateway-id": {g.ID}, filterState: {g.State}, filterSubnetID: {g.SubnetID}, filterVPCID: {g.VPCID},
	}.withTags(g.Tags)
}

// gatewayAddress claims an unassociated Elastic IP address for a public NAT
// gateway.
func gatewayAddress(tx storage.WriteTx, scope request.Scope, g *natGateway, allocationID string) error {
	a, err := loadAddress(tx, scope, allocationID)
	if err != nil {
		return err
	}

	if a.AssociationID != "" {
		return awserror.New("Resource.AlreadyAssociated",
			fmt.Sprintf("Elastic IP address [%s] is already associated", a.ID))
	}

	g.AddressID, g.PublicIP, g.Association = a.ID, a.PublicIP, newID("eipassoc")
	a.AssociationID, a.NATGatewayID, a.InterfaceID, a.PrivateIP = g.Association, g.ID, g.InterfaceID, g.PrivateIP
	return storage.Write(tx, scope.Key("ec2", kindAddress, a.ID), a)
}

// CreateNatGateway creates a NAT gateway in a subnet, with an address from
// it. A public gateway needs an Elastic IP address and an internet gateway
// attached to the subnet's VPC. A ClientToken already used returns its
// gateway.
func (s *Service) CreateNatGateway(
	ctx context.Context,
	in *api.CreateNatGatewayRequest,
) (*api.CreateNatGatewayResult, error) {
	connectivity := common.Value(in.ConnectivityType)
	switch {
	case connectivity == "":
		connectivity = connectivityPublic
	case connectivity != connectivityPublic && connectivity != connectivityPrivate:
		return nil, invalidParameter(fmt.Sprintf("Value (%s) for parameter connectivityType is invalid.",
			connectivity))
	}

	allocationID := common.Value(in.AllocationId)
	switch {
	case connectivity == connectivityPublic && allocationID == "":
		return nil, awserror.New("MissingParameter", "The request must contain the parameter AllocationId")
	case connectivity == connectivityPrivate && allocationID != "":
		return nil, invalidParameter("A private NAT gateway cannot have an Elastic IP address")
	}

	tags, err := specTags(in.TagSpecifications, "natgateway")
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	var out api.NatGateway
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		tokenKey := idempotency.Key(scope, "ec2", "CreateNatGateway", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[api.NatGateway](tx, tokenKey, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ"))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		all, listErr := list[natGateway](tx, scope, kindNATGateway)
		if listErr != nil {
			return listErr
		}

		for _, g := range all {
			switch {
			case !g.visible(now):
				tx.Delete(scope.Key("ec2", kindNATGateway, g.ID))
			}
		}

		id := common.Value(in.SubnetId)
		sn, ok, readErr := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, id))
		if readErr != nil || !ok {
			return common.ErrorsOr(readErr, subnetNotFound(id))
		}

		instances, listErr := list[instance](tx, scope, kindInstance)
		if listErr != nil {
			return listErr
		}

		ips, ipsErr := subnetAddresses(tx, scope, sn, instances)
		if ipsErr != nil {
			return ipsErr
		}

		ip, ipErr := ips.take(common.Value(in.PrivateIpAddress))
		if ipErr != nil {
			return ipErr
		}

		g := &natGateway{
			ID: newID("nat"), SubnetID: sn.ID, VPCID: sn.VPCID, Connectivity: connectivity, PrivateIP: ip,
			InterfaceID: newID("eni"), State: stateAvailable, Created: now, Tags: tags,
		}
		if connectivity == connectivityPublic {
			gateway, gatewayErr := vpcGateway(tx, scope, sn.VPCID)
			if gatewayErr != nil {
				return gatewayErr
			}

			if gateway == "" {
				return awserror.New("Gateway.NotAttached",
					fmt.Sprintf("Network %s is not attached to any internet gateway", sn.VPCID))
			}

			if addressErr := gatewayAddress(tx, scope, g, allocationID); addressErr != nil {
				return addressErr
			}
		}

		out = natGatewayOutput(g, statePending)
		if e := storage.Write(tx, scope.Key("ec2", kindNATGateway, g.ID), g); e != nil {
			return e
		}

		return idempotency.Remember(tx, tokenKey, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateNatGatewayResult{NatGateway: &out, ClientToken: in.ClientToken}, nil
}

// DeleteNatGateway deletes a NAT gateway, which gives up its Elastic IP
// address at once.
func (s *Service) DeleteNatGateway(
	ctx context.Context,
	in *api.DeleteNatGatewayRequest,
) (*api.DeleteNatGatewayResult, error) {
	now := s.now().UTC()
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.NatGatewayId)
		g, ok, err := storage.Read[natGateway](tx, scope.Key("ec2", kindNATGateway, id))
		if err != nil || !ok || !g.visible(now) {
			return common.ErrorsOr(err, natGatewayNotFound(id))
		}

		if !g.active() {
			return nil
		}

		if g.AddressID != "" {
			a, found, readErr := storage.Read[address](tx, scope.Key("ec2", kindAddress, g.AddressID))
			if readErr != nil {
				return readErr
			}

			if found {
				a.AssociationID, a.NATGatewayID, a.InterfaceID, a.PrivateIP = "", "", "", ""
				if writeErr := storage.Write(tx, scope.Key("ec2", kindAddress, a.ID), a); writeErr != nil {
					return writeErr
				}
			}
		}

		g.State, g.Deleted = stateDeleted, now
		return storage.Write(tx, scope.Key("ec2", kindNATGateway, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteNatGatewayResult{NatGatewayId: in.NatGatewayId}, nil
}

func (s *Service) DescribeNatGateways(
	ctx context.Context,
	in *api.DescribeNatGatewaysRequest,
) (*api.DescribeNatGatewaysResult, error) {
	match, err := compileFilters(in.Filter, natGatewayValues(&natGateway{}).names()...)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	out := &api.DescribeNatGatewaysResult{NatGateways: []api.NatGateway{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[natGateway](tx, scope, kindNATGateway)
		if listErr != nil {
			return listErr
		}

		all = slices.DeleteFunc(all, func(g *natGateway) bool { return !g.visible(now) })
		id := func(g *natGateway) string { return g.ID }
		selected, selectErr := selectIDs(all, id, in.NatGatewayIds, natGatewayNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(g *natGateway) bool { return !match.match(natGatewayValues(g)) })
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, g := range selected {
			out.NatGateways = append(out.NatGateways, natGatewayOutput(g, g.State))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

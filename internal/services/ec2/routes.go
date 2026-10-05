package ec2

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindRouteTable   = "route-table"
	targetLocal      = "local"
	originTable      = "CreateRouteTable"
	originRoute      = "CreateRoute"
	routeActive      = "active"
	routeBlackhole   = "blackhole"
	stateAssociated  = "associated"
	filterMain       = "association.main"
	dependencyFormat = "The %s '%s' has dependencies and cannot be deleted."
)

// routeTable is a VPC's route table with its associations: the VPC's main
// table holds the main association, and others hold subnets'.
type routeTable struct {
	Tags         map[string]string `json:",omitempty"`
	ID           string
	VPCID        string
	ClientToken  string             `json:",omitempty"`
	Associations []routeAssociation `json:",omitempty"`
	Routes       []route
}

type routeAssociation struct {
	ID       string
	SubnetID string `json:",omitempty"`
	Main     bool   `json:",omitempty"`
}

// route sends a destination, an IPv4 block or a prefix list, to one target:
// the VPC itself (GatewayID "local"), an internet gateway, a NAT gateway, an
// instance, or a gateway VPC endpoint, whose routes go to its service's
// prefix list.
type route struct {
	CIDR         string `json:",omitempty"`
	PrefixList   string `json:",omitempty"`
	GatewayID    string `json:",omitempty"`
	NATGatewayID string `json:",omitempty"`
	InstanceID   string `json:",omitempty"`
	Origin       string
}

func routeTableNotFound(id string) error {
	return notFound("InvalidRouteTableID.NotFound", "routeTable", id)
}

func associationNotFound(id string) error {
	return notFound("InvalidAssociationID.NotFound", "association", id)
}

func loadRouteTable(tx storage.ReadTx, scope request.Scope, id string) (*routeTable, error) {
	rt, ok, err := storage.Read[routeTable](tx, key(scope, kindRouteTable, id))
	if err == nil && !ok {
		err = routeTableNotFound(id)
	}

	return rt, err
}

// newRouteTable returns a VPC's route table with its local route.
func newRouteTable(v *vpc, tags map[string]string) *routeTable {
	return &routeTable{
		ID: newID("rtb"), VPCID: v.ID, Tags: tags,
		Routes: []route{{CIDR: v.CIDR, GatewayID: targetLocal, Origin: originTable}},
	}
}

// createMainRouteTable stores a VPC's main route table, with routes after
// the local one.
func createMainRouteTable(tx storage.WriteTx, scope request.Scope, v *vpc, routes ...route) error {
	rt := newRouteTable(v, nil)
	rt.Routes = append(rt.Routes, routes...)
	rt.Associations = []routeAssociation{{ID: newID("rtbassoc"), Main: true}}
	return storage.Write(tx, key(scope, kindRouteTable, rt.ID), rt)
}

// vpcRouteTables returns a VPC's route tables, ordered by ID.
func vpcRouteTables(tx storage.ReadTx, scope request.Scope, vpcID string) ([]*routeTable, error) {
	all, err := list[routeTable](tx, scope, kindRouteTable)
	return slices.DeleteFunc(all, func(rt *routeTable) bool { return rt.VPCID != vpcID }), err
}

func (rt *routeTable) main() bool {
	return slices.ContainsFunc(rt.Associations, func(a routeAssociation) bool { return a.Main })
}

// findRouteAssociation returns the route table holding an association and the
// association's index in it.
func findRouteAssociation(tx storage.ReadTx, scope request.Scope, id string) (*routeTable, int, error) {
	all, err := list[routeTable](tx, scope, kindRouteTable)
	if err != nil {
		return nil, 0, err
	}

	for _, rt := range all {
		if at := slices.IndexFunc(rt.Associations, func(a routeAssociation) bool { return a.ID == id }); at >= 0 {
			return rt, at, nil
		}
	}

	return nil, 0, associationNotFound(id)
}

// dissociateRouteTable removes a deleted subnet's explicit association.
func dissociateRouteTable(tx storage.WriteTx, scope request.Scope, subnetID string) error {
	all, err := list[routeTable](tx, scope, kindRouteTable)
	if err != nil {
		return err
	}

	for _, rt := range all {
		kept := slices.DeleteFunc(slices.Clone(rt.Associations), func(a routeAssociation) bool {
			return a.SubnetID == subnetID
		})
		if len(kept) == len(rt.Associations) {
			continue
		}

		rt.Associations = kept
		if writeErr := storage.Write(tx, key(scope, kindRouteTable, rt.ID), rt); writeErr != nil {
			return writeErr
		}
	}

	return nil
}

// routeState reports whether a route's target can carry traffic: an internet
// gateway must still be attached to the table's VPC, a NAT gateway not be
// deleted, and an instance be running.
func routeState(tx storage.ReadTx, scope request.Scope, rt *routeTable, r *route) (string, error) {
	ok := true
	switch {
	case strings.HasPrefix(r.GatewayID, "vpce-"):
		_, found, err := storage.Read[vpcEndpoint](tx, key(scope, kindVPCEndpoint, r.GatewayID))
		if err != nil {
			return "", err
		}

		ok = found
	case r.NATGatewayID != "":
		g, found, err := storage.Read[natGateway](tx, key(scope, kindNATGateway, r.NATGatewayID))
		if err != nil {
			return "", err
		}

		ok = found && g.active()
	case strings.HasPrefix(r.GatewayID, "igw-"):
		g, found, err := storage.Read[internetGateway](tx, key(scope, kindInternetGateway, r.GatewayID))
		if err != nil {
			return "", err
		}

		ok = found && g.VPCID == rt.VPCID
	case r.InstanceID != "":
		i, found, err := storage.Read[instance](tx, key(scope, kindInstance, r.InstanceID))
		if err != nil {
			return "", err
		}

		ok = found && i.State == stateRunning
	}

	if !ok {
		return routeBlackhole, nil
	}

	return routeActive, nil
}

func associationOutput(rt *routeTable, a *routeAssociation) api.RouteTableAssociation {
	return api.RouteTableAssociation{
		Main: new(a.Main), RouteTableAssociationId: new(a.ID), RouteTableId: new(rt.ID),
		SubnetId: optional(a.SubnetID), AssociationState: &api.RouteTableAssociationState{State: new(stateAssociated)},
	}
}

func routeTableOutput(tx storage.ReadTx, scope request.Scope, rt *routeTable) (api.RouteTable, error) {
	out := api.RouteTable{
		RouteTableId: new(rt.ID), VpcId: new(rt.VPCID), OwnerId: new(scope.AccountID), Tags: tagList(rt.Tags),
		Associations: []api.RouteTableAssociation{}, PropagatingVgws: []api.PropagatingVgw{}, Routes: []api.Route{},
	}
	for i := range rt.Associations {
		out.Associations = append(out.Associations, associationOutput(rt, &rt.Associations[i]))
	}

	for i := range rt.Routes {
		r := &rt.Routes[i]
		state, err := routeState(tx, scope, rt, r)
		if err != nil {
			return out, err
		}

		item := api.Route{
			DestinationCidrBlock: optional(r.CIDR), DestinationPrefixListId: optional(r.PrefixList),
			GatewayId: optional(r.GatewayID), InstanceId: optional(r.InstanceID),
			NatGatewayId: optional(r.NATGatewayID), Origin: new(r.Origin), State: new(state),
		}
		if r.InstanceID != "" {
			item.InstanceOwnerId = new(scope.AccountID)
		}

		out.Routes = append(out.Routes, item)
	}

	return out, nil
}

func routeTableValues(scope request.Scope, rt *routeTable) values {
	v := values{
		"route-table-id": {rt.ID}, filterVPCID: {rt.VPCID}, filterOwnerID: {scope.AccountID},
		filterMain: {}, "association.subnet-id": {}, "association.route-table-association-id": {},
		"association.route-table-id": {}, "route.destination-cidr-block": {}, "route.gateway-id": {},
		"route.instance-id": {}, "route.nat-gateway-id": {}, "route.origin": {},
		"route.destination-prefix-list-id": {},
	}
	for _, a := range rt.Associations {
		v[filterMain] = append(v[filterMain], strconv.FormatBool(a.Main))
		v["association.route-table-association-id"] = append(v["association.route-table-association-id"], a.ID)
		v["association.route-table-id"] = append(v["association.route-table-id"], rt.ID)
		if a.SubnetID != "" {
			v["association.subnet-id"] = append(v["association.subnet-id"], a.SubnetID)
		}
	}

	for _, r := range rt.Routes {
		v["route.origin"] = append(v["route.origin"], r.Origin)
		if r.CIDR != "" {
			v["route.destination-cidr-block"] = append(v["route.destination-cidr-block"], r.CIDR)
		}

		if r.PrefixList != "" {
			v["route.destination-prefix-list-id"] = append(v["route.destination-prefix-list-id"], r.PrefixList)
		}

		if r.GatewayID != "" {
			v["route.gateway-id"] = append(v["route.gateway-id"], r.GatewayID)
		}

		if r.InstanceID != "" {
			v["route.instance-id"] = append(v["route.instance-id"], r.InstanceID)
		}

		if r.NATGatewayID != "" {
			v["route.nat-gateway-id"] = append(v["route.nat-gateway-id"], r.NATGatewayID)
		}
	}

	return v.withTags(rt.Tags)
}

func (s *Service) CreateRouteTable(
	ctx context.Context,
	in *api.CreateRouteTableRequest,
) (*api.CreateRouteTableResult, error) {
	tags, err := specTags(in.TagSpecifications, kindRouteTable)
	if err != nil {
		return nil, err
	}

	var out api.RouteTable
	token := common.Value(in.ClientToken)
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, loadErr := loadVPC(tx, scope, common.Value(in.VpcId))
		if loadErr != nil {
			return loadErr
		}

		all, listErr := list[routeTable](tx, scope, kindRouteTable)
		if listErr != nil {
			return listErr
		}

		// A ClientToken already used returns the table it created.
		at := slices.IndexFunc(all, func(rt *routeTable) bool { return token != "" && rt.ClientToken == token })
		if at >= 0 {
			out, err = routeTableOutput(tx, scope, all[at])
			return err
		}

		rt := newRouteTable(v, tags)
		rt.ClientToken = token
		if writeErr := storage.Write(tx, key(scope, kindRouteTable, rt.ID), rt); writeErr != nil {
			return writeErr
		}

		out, err = routeTableOutput(tx, scope, rt)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRouteTableResult{RouteTable: &out, ClientToken: in.ClientToken}, nil
}

// DeleteRouteTable deletes a route table that is neither main nor associated
// with a subnet. Gateway endpoints that routed through it no longer do.
func (s *Service) DeleteRouteTable(
	ctx context.Context,
	in *api.DeleteRouteTableRequest,
) (*api.DeleteRouteTableOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rt, err := loadRouteTable(tx, scope, common.Value(in.RouteTableId))
		if err != nil {
			return err
		}

		if len(rt.Associations) > 0 {
			return awserror.New("DependencyViolation", fmt.Sprintf(dependencyFormat, "routeTable", rt.ID))
		}

		tx.Delete(key(scope, kindRouteTable, rt.ID))
		return forgetRouteTable(tx, scope, rt.ID)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRouteTableOutput{}, nil
}

func (s *Service) DescribeRouteTables(
	ctx context.Context,
	in *api.DescribeRouteTablesRequest,
) (*api.DescribeRouteTablesResult, error) {
	known := routeTableValues(request.Scope{}, &routeTable{}).names()
	match, err := compileFilters(in.Filters, append(known, "route.state")...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeRouteTablesResult{RouteTables: []api.RouteTable{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[routeTable](tx, scope, kindRouteTable)
		if listErr != nil {
			return listErr
		}

		id := func(rt *routeTable) string { return rt.ID }
		selected, selectErr := selectIDs(all, id, in.RouteTableIds, routeTableNotFound)
		if selectErr != nil {
			return selectErr
		}

		var matched []*api.RouteTable
		for _, rt := range selected {
			item, outErr := routeTableOutput(tx, scope, rt)
			if outErr != nil {
				return outErr
			}

			v := routeTableValues(scope, rt)
			v["route.state"] = []string{}
			for _, r := range item.Routes {
				v["route.state"] = append(v["route.state"], *r.State)
			}

			if match.match(v) {
				matched = append(matched, &item)
			}
		}

		shown, next, pageErr := page(matched, func(rt *api.RouteTable) string { return *rt.RouteTableId },
			in.NextToken, in.MaxResults, maxDescribe)
		for _, rt := range shown {
			out.RouteTables = append(out.RouteTables, *rt)
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// routeDestination checks a route's IPv4 destination.
func routeDestination(text string) (string, error) {
	p, err := netip.ParsePrefix(text)
	if err != nil || !p.Addr().Is4() || p.Masked() != p {
		return "", invalidParameter(fmt.Sprintf("Value (%s) for parameter destinationCidrBlock is invalid. "+
			"This is not a valid CIDR block.", text))
	}

	return p.String(), nil
}

// routeTarget checks the target a CreateRoute or ReplaceRoute request names
// in want: exactly one of an internet gateway attached to the table's VPC, a
// NAT gateway in it, and an instance in it.
func routeTarget(tx storage.ReadTx, scope request.Scope, rt *routeTable, want *route) error {
	want.Origin = originRoute
	set := 0
	for _, id := range []string{want.GatewayID, want.NATGatewayID, want.InstanceID} {
		if id != "" {
			set++
		}
	}

	var vpcID string
	switch {
	case set != 1:
		return awserror.New("InvalidParameterCombination", "The parameter combination is not valid: "+
			"a route needs exactly one of gatewayId, natGatewayId, and instanceId")
	case want.GatewayID != "":
		if !strings.HasPrefix(want.GatewayID, "igw-") {
			return notFound("InvalidGatewayID.NotFound", "gateway", want.GatewayID)
		}

		g, err := loadGateway(tx, scope, want.GatewayID)
		if err != nil {
			return err
		}

		vpcID = g.VPCID
	case want.NATGatewayID != "":
		g, found, err := storage.Read[natGateway](tx, key(scope, kindNATGateway, want.NATGatewayID))
		if err != nil || !found || !g.active() {
			return errorsOr(err, natGatewayNotFound(want.NATGatewayID))
		}

		vpcID = g.VPCID
	default:
		i, found, err := storage.Read[instance](tx, key(scope, kindInstance, want.InstanceID))
		if err != nil || !found || !i.active() {
			return errorsOr(err, instanceNotFound(want.InstanceID))
		}

		vpcID = i.VPCID
	}

	if vpcID != rt.VPCID {
		return invalidParameter(fmt.Sprintf("route table %s and the route's target belong to different "+
			"networks", rt.ID))
	}

	return nil
}

// changeRoute loads a route table, runs fn on it with the request's
// destination, and stores it.
func (s *Service) changeRoute(
	ctx context.Context,
	dry *bool,
	tableID, destination string,
	fn func(tx storage.ReadTx, scope request.Scope, rt *routeTable, cidr string, at int) error,
) error {
	cidr, err := routeDestination(destination)
	if err != nil {
		return err
	}

	return s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		rt, loadErr := loadRouteTable(tx, scope, tableID)
		if loadErr != nil {
			return loadErr
		}

		at := slices.IndexFunc(rt.Routes, func(r route) bool { return r.CIDR == cidr })
		if fnErr := fn(tx, scope, rt, cidr, at); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, key(scope, kindRouteTable, rt.ID), rt)
	})
}

func routeNotFound(cidr string) error {
	return awserror.New("InvalidRoute.NotFound", fmt.Sprintf("There is no route defined for '%s' in the route "+
		"table. Use CreateRoute instead.", cidr))
}

func localRoute(rt *routeTable, cidr string) error {
	return invalidParameter(fmt.Sprintf("cannot change local route %s in route table %s", cidr, rt.ID))
}

func (s *Service) CreateRoute(ctx context.Context, in *api.CreateRouteRequest) (*api.CreateRouteResult, error) {
	err := s.changeRoute(ctx, in.DryRun, common.Value(in.RouteTableId), common.Value(in.DestinationCidrBlock),
		func(tx storage.ReadTx, scope request.Scope, rt *routeTable, cidr string, at int) error {
			if at >= 0 {
				return awserror.New("RouteAlreadyExists", fmt.Sprintf("The route identified by %s already exists.",
					cidr))
			}

			r := route{
				CIDR: cidr, GatewayID: common.Value(in.GatewayId), NATGatewayID: common.Value(in.NatGatewayId),
				InstanceID: common.Value(in.InstanceId),
			}
			err := routeTarget(tx, scope, rt, &r)
			rt.Routes = append(rt.Routes, r)
			return err
		})
	if err != nil {
		return nil, err
	}

	return &api.CreateRouteResult{Return: new(true)}, nil
}

func (s *Service) ReplaceRoute(ctx context.Context, in *api.ReplaceRouteRequest) (*api.ReplaceRouteOutput, error) {
	err := s.changeRoute(ctx, in.DryRun, common.Value(in.RouteTableId), common.Value(in.DestinationCidrBlock),
		func(tx storage.ReadTx, scope request.Scope, rt *routeTable, cidr string, at int) error {
			switch {
			case at < 0:
				return routeNotFound(cidr)
			case rt.Routes[at].GatewayID == targetLocal:
				return localRoute(rt, cidr)
			}

			r := route{
				CIDR: cidr, GatewayID: common.Value(in.GatewayId), NATGatewayID: common.Value(in.NatGatewayId),
				InstanceID: common.Value(in.InstanceId),
			}
			err := routeTarget(tx, scope, rt, &r)
			rt.Routes[at] = r
			return err
		})
	if err != nil {
		return nil, err
	}

	return &api.ReplaceRouteOutput{}, nil
}

func (s *Service) DeleteRoute(ctx context.Context, in *api.DeleteRouteRequest) (*api.DeleteRouteOutput, error) {
	err := s.changeRoute(ctx, in.DryRun, common.Value(in.RouteTableId), common.Value(in.DestinationCidrBlock),
		func(_ storage.ReadTx, _ request.Scope, rt *routeTable, cidr string, at int) error {
			switch {
			case at < 0:
				return routeNotFound(cidr)
			case rt.Routes[at].GatewayID == targetLocal:
				return localRoute(rt, cidr)
			}

			rt.Routes = slices.Delete(rt.Routes, at, at+1)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRouteOutput{}, nil
}

// AssociateRouteTable associates a route table with a subnet of its VPC that
// has no explicit association yet.
func (s *Service) AssociateRouteTable(
	ctx context.Context,
	in *api.AssociateRouteTableRequest,
) (*api.AssociateRouteTableResult, error) {
	a := routeAssociation{ID: newID("rtbassoc"), SubnetID: common.Value(in.SubnetId)}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rt, err := loadRouteTable(tx, scope, common.Value(in.RouteTableId))
		if err != nil {
			return err
		}

		sn, ok, err := storage.Read[subnet](tx, key(scope, kindSubnet, a.SubnetID))
		if err != nil || !ok {
			return errorsOr(err, subnetNotFound(a.SubnetID))
		}

		if sn.VPCID != rt.VPCID {
			return invalidParameter(fmt.Sprintf("Route table %s and subnet %s belong to different networks",
				rt.ID, sn.ID))
		}

		tables, err := vpcRouteTables(tx, scope, rt.VPCID)
		if err != nil {
			return err
		}

		for _, other := range tables {
			if slices.ContainsFunc(other.Associations, func(o routeAssociation) bool { return o.SubnetID == sn.ID }) {
				return awserror.New("Resource.AlreadyAssociated", fmt.Sprintf("the specified association for "+
					"route table %s conflicts with an existing association", rt.ID))
			}
		}

		rt.Associations = append(rt.Associations, a)
		return storage.Write(tx, key(scope, kindRouteTable, rt.ID), rt)
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateRouteTableResult{
		AssociationId: new(a.ID), AssociationState: &api.RouteTableAssociationState{State: new(stateAssociated)},
	}, nil
}

// DisassociateRouteTable removes a subnet's explicit association, so the
// subnet uses its VPC's main route table again.
func (s *Service) DisassociateRouteTable(
	ctx context.Context,
	in *api.DisassociateRouteTableRequest,
) (*api.DisassociateRouteTableOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rt, at, err := findRouteAssociation(tx, scope, common.Value(in.AssociationId))
		if err != nil {
			return err
		}

		if rt.Associations[at].Main {
			return invalidParameter("cannot disassociate the main route table association " +
				rt.Associations[at].ID)
		}

		rt.Associations = slices.Delete(rt.Associations, at, at+1)
		return storage.Write(tx, key(scope, kindRouteTable, rt.ID), rt)
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateRouteTableOutput{}, nil
}

// ReplaceRouteTableAssociation moves an association, a subnet's or the VPC's
// main one, to another route table of the VPC under a new ID.
func (s *Service) ReplaceRouteTableAssociation(
	ctx context.Context,
	in *api.ReplaceRouteTableAssociationRequest,
) (*api.ReplaceRouteTableAssociationResult, error) {
	newAssociation := newID("rtbassoc")
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		from, at, err := findRouteAssociation(tx, scope, common.Value(in.AssociationId))
		if err != nil {
			return err
		}

		to, err := loadRouteTable(tx, scope, common.Value(in.RouteTableId))
		if err != nil {
			return err
		}

		if to.VPCID != from.VPCID {
			return invalidParameter(fmt.Sprintf("Route table %s is not in network %s", to.ID, from.VPCID))
		}

		a := from.Associations[at]
		from.Associations = slices.Delete(from.Associations, at, at+1)
		if writeErr := storage.Write(tx, key(scope, kindRouteTable, from.ID), from); writeErr != nil {
			return writeErr
		}

		// Moving an association to the table that holds it replaces its ID.
		if to.ID == from.ID {
			to = from
		}

		// The main association comes first.
		a.ID = newAssociation
		if a.Main {
			to.Associations = slices.Insert(to.Associations, 0, a)
		} else {
			to.Associations = append(to.Associations, a)
		}

		return storage.Write(tx, key(scope, kindRouteTable, to.ID), to)
	})
	if err != nil {
		return nil, err
	}

	return &api.ReplaceRouteTableAssociationResult{
		NewAssociationId: new(newAssociation),
		AssociationState: &api.RouteTableAssociationState{State: new(stateAssociated)},
	}, nil
}

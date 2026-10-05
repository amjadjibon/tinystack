package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindVPCEndpoint   = "vpc-endpoint"
	serviceS3         = "s3"
	serviceDynamoDB   = "dynamodb"
	filterPrefixList  = "prefix-list-id"
	endpointGateway   = "Gateway"
	endpointInterface = "Interface"
	// EC2 reports endpoint states in lowercase, although the model's enum
	// capitalizes them.
	endpointAvailable = stateAvailable
	endpointPending   = statePending
	ipv4              = "ipv4"
	// endpointZone is the hosted zone of interface endpoints' DNS names.
	endpointZone = "Z7HUB22UULQXV"
	// defaultEndpointPolicy allows everything, as an endpoint's policy does
	// until one is set.
	defaultEndpointPolicy = `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Principal":"*",` +
		`"Action":"*","Resource":"*"}]}`
)

// gatewayServices are the services gateway endpoints reach, through their
// AWS-managed prefix lists.
var gatewayServices = map[string][]string{
	serviceS3:       {"3.5.0.0/19", "52.216.0.0/15", "54.231.0.0/16"},
	serviceDynamoDB: {"3.218.180.0/22", "52.94.0.0/22", "52.119.224.0/20"},
}

// interfaceServices are the services interface endpoints reach, by the
// first labels of their DNS names where those differ.
var interfaceServices = map[string]string{
	"acm": "", "autoscaling": "", "cloudformation": "", "cognito-idp": "", serviceDynamoDB: "", "ec2": "",
	"ecr.api": "api.ecr", "ecr.dkr": "dkr.ecr", "ecs": "", "eks": "", "elasticloadbalancing": "", "events": "",
	"execute-api": "", "firehose": "", "kinesis-streams": "kinesis", "kms": "", "lambda": "", "logs": "",
	"monitoring": "", serviceS3: "", "scheduler": "", "secretsmanager": "", "sns": "", "sqs": "", "ssm": "",
	"states": "", "sts": "",
}

// vpcEndpoint is a VPC endpoint, available at once: a gateway endpoint
// routes its service's prefix list in its route tables, and an interface
// endpoint has a network interface in each of its subnets and DNS names.
type vpcEndpoint struct {
	Created       time.Time
	Tags          map[string]string `json:",omitempty"`
	ID            string
	VPCID         string
	Service       string
	Type          string
	Policy        string
	Label         string
	ClientToken   string           `json:",omitempty"`
	RouteTableIDs []string         `json:",omitempty"`
	Subnets       []endpointSubnet `json:",omitempty"`
	GroupIDs      []string         `json:",omitempty"`
	PrivateDNS    bool
}

// endpointSubnet is an interface endpoint's subnet and the network interface
// it has there, with an address of the subnet.
type endpointSubnet struct {
	ID          string
	Zone        string
	InterfaceID string
	PrivateIP   string
}

func endpointNotFound(id string) error {
	return awserror.New("InvalidVpcEndpointId.NotFound", fmt.Sprintf("The Vpc Endpoint Id '%s' does not exist", id))
}

func loadEndpoint(tx storage.ReadTx, scope request.Scope, id string) (*vpcEndpoint, error) {
	ep, ok, err := storage.Read[vpcEndpoint](tx, scope.Key("ec2", kindVPCEndpoint, id))
	if err == nil && !ok {
		err = endpointNotFound(id)
	}

	return ep, err
}

// prefixList is an AWS-managed prefix list of a gateway service.
type prefixList struct {
	ID    string
	Name  string
	CIDRs []string
}

// managedPrefixList returns a gateway service's prefix list in a Region,
// whose ID depends only on its name.
func managedPrefixList(region, service string) prefixList {
	name := "com.amazonaws." + region + "." + service
	sum := sha256.Sum256([]byte(name))
	return prefixList{ID: "pl-" + hex.EncodeToString(sum[:4]), Name: name, CIDRs: gatewayServices[service]}
}

// endpointService checks a service name, com.amazonaws.<Region>.<service>,
// against what an endpoint type reaches and returns the service.
func endpointService(region, name, kind string) (string, error) {
	service, ok := strings.CutPrefix(name, "com.amazonaws."+region+".")
	_, gateway := gatewayServices[service]
	_, iface := interfaceServices[service]
	if !ok || (kind == endpointGateway && !gateway) || (kind == endpointInterface && !iface) {
		return "", awserror.New("InvalidServiceName", fmt.Sprintf("The Vpc Endpoint Service '%s' does not exist",
			name))
	}

	return service, nil
}

// endpointPolicy checks a policy document, which must be a JSON object.
func endpointPolicy(text string) error {
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return awserror.New("MalformedPolicyDocument", "Policy document is not valid JSON: "+err.Error())
	}

	return nil
}

// dnsEntries returns an interface endpoint's DNS names: its regional and
// zonal names and, with private DNS, the service's own name.
func dnsEntries(region string, ep *vpcEndpoint) []api.DnsEntry {
	stem := ep.Service
	if label := interfaceServices[ep.Service]; label != "" {
		stem = label
	}

	domain := stem + "." + region + ".vpce.amazonaws.com"
	names := []string{ep.ID + "-" + ep.Label + "." + domain}
	for _, sn := range ep.Subnets {
		names = append(names, ep.ID+"-"+ep.Label+"-"+sn.Zone+"."+domain)
	}

	if ep.PrivateDNS {
		names = append(names, stem+"."+region+".amazonaws.com")
	}

	out := make([]api.DnsEntry, 0, len(names))
	for _, name := range names {
		out = append(out, api.DnsEntry{DnsName: new(name), HostedZoneId: new(endpointZone)})
	}

	return out
}

func endpointOutput(scope request.Scope, ep *vpcEndpoint, state string) api.VpcEndpoint {
	out := api.VpcEndpoint{
		VpcEndpointId: new(ep.ID), VpcEndpointType: new(ep.Type), VpcId: new(ep.VPCID), State: new(state),
		ServiceName: new("com.amazonaws." + scope.Region + "." + ep.Service), PolicyDocument: new(ep.Policy),
		RouteTableIds: slices.Clone(ep.RouteTableIDs), PrivateDnsEnabled: new(ep.PrivateDNS),
		RequesterManaged: new(false), CreationTimestamp: new(ep.Created), Tags: tagList(ep.Tags),
		OwnerId: new(scope.AccountID), Groups: []api.SecurityGroupIdentifier{}, DnsEntries: []api.DnsEntry{},
	}
	if ep.Type == endpointInterface {
		out.IpAddressType, out.DnsOptions = new(ipv4), &api.DnsOptions{DnsRecordIpType: new(ipv4)}
		out.DnsEntries = dnsEntries(scope.Region, ep)
		for _, sn := range ep.Subnets {
			out.SubnetIds = append(out.SubnetIds, sn.ID)
			out.NetworkInterfaceIds = append(out.NetworkInterfaceIds, sn.InterfaceID)
		}

		for _, id := range ep.GroupIDs {
			out.Groups = append(out.Groups, api.SecurityGroupIdentifier{GroupId: new(id)})
		}
	}

	return out
}

func endpointValues(scope request.Scope, ep *vpcEndpoint) values {
	v := values{
		"vpc-endpoint-id": {ep.ID}, filterVPCID: {ep.VPCID}, "vpc-endpoint-state": {stateAvailable},
		"service-name": {"com.amazonaws." + scope.Region + "." + ep.Service}, "vpc-endpoint-type": {ep.Type},
		"ip-address-type": {},
	}
	if ep.Type == endpointInterface {
		v["ip-address-type"] = []string{ipv4}
	}

	return v.withTags(ep.Tags)
}

// routeEndpoint adds a gateway endpoint's route to a route table of its VPC.
func routeEndpoint(tx storage.WriteTx, scope request.Scope, ep *vpcEndpoint, tableID string) error {
	rt, err := loadRouteTable(tx, scope, tableID)
	if err != nil {
		return err
	}

	if rt.VPCID != ep.VPCID {
		return invalidParameter(fmt.Sprintf("Route table %s is not in network %s", rt.ID, ep.VPCID))
	}

	list := managedPrefixList(scope.Region, ep.Service).ID
	if slices.ContainsFunc(rt.Routes, func(r route) bool { return r.PrefixList == list }) {
		return awserror.New("RouteAlreadyExists", fmt.Sprintf("route table %s already has a route with "+
			"destination-prefix-list-id %s", rt.ID, list))
	}

	rt.Routes = append(rt.Routes, route{PrefixList: list, GatewayID: ep.ID, Origin: originRoute})
	ep.RouteTableIDs = append(ep.RouteTableIDs, rt.ID)
	return storage.Write(tx, scope.Key("ec2", kindRouteTable, rt.ID), rt)
}

// unrouteEndpoint removes a gateway endpoint's route from a route table.
func unrouteEndpoint(tx storage.WriteTx, scope request.Scope, ep *vpcEndpoint, tableID string) error {
	ep.RouteTableIDs = slices.DeleteFunc(ep.RouteTableIDs, func(id string) bool { return id == tableID })
	rt, ok, err := storage.Read[routeTable](tx, scope.Key("ec2", kindRouteTable, tableID))
	if err != nil || !ok {
		return err
	}

	rt.Routes = slices.DeleteFunc(rt.Routes, func(r route) bool { return r.GatewayID == ep.ID })
	return storage.Write(tx, scope.Key("ec2", kindRouteTable, rt.ID), rt)
}

// addEndpointSubnet gives an interface endpoint a network interface in a
// subnet of its VPC, at most one per zone.
func addEndpointSubnet(tx storage.ReadTx, scope request.Scope, ep *vpcEndpoint, id string) error {
	sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, id))
	if err != nil || !ok {
		return common.ErrorsOr(err, subnetNotFound(id))
	}

	if sn.VPCID != ep.VPCID {
		return invalidParameter(fmt.Sprintf("Subnet %s is not in network %s", sn.ID, ep.VPCID))
	}

	if slices.ContainsFunc(ep.Subnets, func(other endpointSubnet) bool { return other.Zone == sn.Zone }) {
		return awserror.New("DuplicateSubnetsInSameZone", "Found another VPC endpoint subnet in the "+
			"availability zone of "+sn.ID+". VPC endpoint subnets should be in different availability zones.")
	}

	instances, err := list[instance](tx, scope, kindInstance)
	if err != nil {
		return err
	}

	ips, err := subnetAddresses(tx, scope, sn, instances)
	if err != nil {
		return err
	}

	ip, err := ips.take("")
	if err != nil {
		return err
	}

	ep.Subnets = append(ep.Subnets, endpointSubnet{ID: sn.ID, Zone: sn.Zone, InterfaceID: newID("eni"), PrivateIP: ip})
	return nil
}

// addEndpointGroup adds a security group of an interface endpoint's VPC.
func addEndpointGroup(tx storage.ReadTx, scope request.Scope, ep *vpcEndpoint, id string) error {
	g, ok, err := storage.Read[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, id))
	if err != nil || !ok {
		return common.ErrorsOr(err, groupNotFound(id))
	}

	if g.VPCID != ep.VPCID {
		return invalidParameter(fmt.Sprintf("Security group %s is not in network %s", g.ID, ep.VPCID))
	}

	if !slices.Contains(ep.GroupIDs, g.ID) {
		ep.GroupIDs = append(ep.GroupIDs, g.ID)
	}

	return nil
}

// checkPrivateDNS requires the VPC attributes an interface endpoint's
// private DNS names need.
func checkPrivateDNS(tx storage.ReadTx, scope request.Scope, ep *vpcEndpoint) error {
	if !ep.PrivateDNS {
		return nil
	}

	v, err := loadVPC(tx, scope, ep.VPCID)
	if err != nil {
		return err
	}

	if v.NoDNSSupport || !v.DNSHostnames {
		return invalidParameter("Enabling private DNS requires both enableDnsSupport and enableDnsHostnames " +
			"VPC attributes set to true for " + v.ID)
	}

	return nil
}

// endpointRequest is what CreateVpcEndpoint asks for.
type endpointRequest struct {
	in      *api.CreateVpcEndpointRequest
	tags    map[string]string
	service string
	kind    string
}

// build checks the request in tx and returns the new endpoint, routing its
// route tables.
func (r *endpointRequest) build(tx storage.WriteTx, scope request.Scope, now time.Time) (*vpcEndpoint, error) {
	v, err := loadVPC(tx, scope, common.Value(r.in.VpcId))
	if err != nil {
		return nil, err
	}

	ep := &vpcEndpoint{
		ID: newID("vpce"), VPCID: v.ID, Service: r.service, Type: r.kind, Created: now, Tags: r.tags,
		Policy: defaultEndpointPolicy, Label: newID("x")[2:10], ClientToken: common.Value(r.in.ClientToken),
	}
	if r.in.PolicyDocument != nil {
		ep.Policy = *r.in.PolicyDocument
	}

	if r.kind == endpointGateway {
		for _, id := range r.in.RouteTableIds {
			if stepErr := routeEndpoint(tx, scope, ep, id); stepErr != nil {
				return nil, stepErr
			}
		}

		return ep, nil
	}

	ep.PrivateDNS = r.in.PrivateDnsEnabled == nil || *r.in.PrivateDnsEnabled
	for _, id := range r.in.SubnetIds {
		if stepErr := addEndpointSubnet(tx, scope, ep, id); stepErr != nil {
			return nil, stepErr
		}
	}

	// Without security groups, an interface endpoint takes its VPC's default
	// group.
	groups := r.in.SecurityGroupIds
	if len(groups) == 0 {
		all, listErr := list[securityGroup](tx, scope, kindSecurityGroup)
		if listErr != nil {
			return nil, listErr
		}

		for _, g := range all {
			if g.VPCID == v.ID && g.Name == defaultGroupName {
				groups = append(groups, g.ID)
			}
		}
	}

	for _, id := range groups {
		if stepErr := addEndpointGroup(tx, scope, ep, id); stepErr != nil {
			return nil, stepErr
		}
	}

	return ep, checkPrivateDNS(tx, scope, ep)
}

// CreateVpcEndpoint creates a gateway endpoint (the default type) to S3 or
// DynamoDB in route tables, or an interface endpoint to a TinyStack service
// in subnets. A ClientToken already used returns its endpoint.
func (s *Service) CreateVpcEndpoint(
	ctx context.Context,
	in *api.CreateVpcEndpointRequest,
) (*api.CreateVpcEndpointResult, error) {
	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	r := &endpointRequest{in: in, kind: common.Value(in.VpcEndpointType)}
	switch r.kind {
	case "":
		r.kind = endpointGateway
	case endpointGateway, endpointInterface:
	default:
		return nil, invalidParameter(fmt.Sprintf("Value (%s) for parameter vpcEndpointType is invalid. "+
			"TinyStack supports Gateway and Interface endpoints.", r.kind))
	}

	if r.service, err = endpointService(m.Region, common.Value(in.ServiceName), r.kind); err != nil {
		return nil, err
	}

	gateway := r.kind == endpointGateway
	if gateway && (len(in.SubnetIds) > 0 || len(in.SecurityGroupIds) > 0 || common.Value(in.PrivateDnsEnabled)) {
		return nil, invalidParameter("Gateway endpoints take route tables, not subnets, security groups, " +
			"or private DNS")
	}

	if !gateway && len(in.RouteTableIds) > 0 {
		return nil, invalidParameter("Interface endpoints take subnets, not route tables")
	}

	if in.PolicyDocument != nil {
		if stepErr := endpointPolicy(*in.PolicyDocument); stepErr != nil {
			return nil, stepErr
		}
	}

	if r.tags, err = specTags(in.TagSpecifications, kindVPCEndpoint); err != nil {
		return nil, err
	}

	now := s.now().UTC()
	var out api.VpcEndpoint
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, listErr := list[vpcEndpoint](tx, scope, kindVPCEndpoint)
		if listErr != nil {
			return listErr
		}

		token := common.Value(in.ClientToken)
		if at := slices.IndexFunc(all, func(ep *vpcEndpoint) bool {
			return token != "" && ep.ClientToken == token
		}); at >= 0 {
			out = endpointOutput(scope, all[at], endpointAvailable)
			return nil
		}

		ep, buildErr := r.build(tx, scope, now)
		if buildErr != nil {
			return buildErr
		}

		// Interface endpoints report pending while their interfaces start.
		state := endpointAvailable
		if !gateway {
			state = endpointPending
		}

		out = endpointOutput(scope, ep, state)
		return storage.Write(tx, scope.Key("ec2", kindVPCEndpoint, ep.ID), ep)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVpcEndpointResult{VpcEndpoint: &out, ClientToken: in.ClientToken}, nil
}

// ModifyVpcEndpoint changes an endpoint's route tables (gateway endpoints),
// subnets, security groups, and private DNS (interface endpoints), and
// policy.
func (s *Service) ModifyVpcEndpoint(
	ctx context.Context,
	in *api.ModifyVpcEndpointRequest,
) (*api.ModifyVpcEndpointResult, error) {
	if in.PolicyDocument != nil {
		if err := endpointPolicy(*in.PolicyDocument); err != nil {
			return nil, err
		}
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		ep, err := loadEndpoint(tx, scope, common.Value(in.VpcEndpointId))
		if err != nil {
			return err
		}

		gateway := ep.Type == endpointGateway
		interfaceOnly := len(in.AddSubnetIds) > 0 || len(in.RemoveSubnetIds) > 0 || len(in.AddSecurityGroupIds) > 0 ||
			len(in.RemoveSecurityGroupIds) > 0 || in.PrivateDnsEnabled != nil
		gatewayOnly := len(in.AddRouteTableIds) > 0 || len(in.RemoveRouteTableIds) > 0
		if (gateway && interfaceOnly) || (!gateway && gatewayOnly) {
			return invalidParameter("The requested changes do not apply to a " + ep.Type + " endpoint")
		}

		if stepErr := modifyEndpoint(tx, scope, ep, in); stepErr != nil {
			return stepErr
		}

		return storage.Write(tx, scope.Key("ec2", kindVPCEndpoint, ep.ID), ep)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyVpcEndpointResult{Return: new(true)}, nil
}

// modifyEndpoint applies a ModifyVpcEndpoint request's changes to ep.
func modifyEndpoint(tx storage.WriteTx, scope request.Scope, ep *vpcEndpoint, in *api.ModifyVpcEndpointRequest) error {
	for _, id := range in.RemoveRouteTableIds {
		if !slices.Contains(ep.RouteTableIDs, id) {
			return routeTableNotFound(id)
		}

		if err := unrouteEndpoint(tx, scope, ep, id); err != nil {
			return err
		}
	}

	for _, id := range in.AddRouteTableIds {
		if err := routeEndpoint(tx, scope, ep, id); err != nil {
			return err
		}
	}

	for _, id := range in.RemoveSubnetIds {
		at := slices.IndexFunc(ep.Subnets, func(sn endpointSubnet) bool { return sn.ID == id })
		if at < 0 {
			return subnetNotFound(id)
		}

		ep.Subnets = slices.Delete(ep.Subnets, at, at+1)
	}

	for _, id := range in.AddSubnetIds {
		if err := addEndpointSubnet(tx, scope, ep, id); err != nil {
			return err
		}
	}

	for _, id := range in.RemoveSecurityGroupIds {
		if !slices.Contains(ep.GroupIDs, id) {
			return groupNotFound(id)
		}

		ep.GroupIDs = slices.DeleteFunc(ep.GroupIDs, func(g string) bool { return g == id })
	}

	for _, id := range in.AddSecurityGroupIds {
		if err := addEndpointGroup(tx, scope, ep, id); err != nil {
			return err
		}
	}

	switch {
	case common.Value(in.ResetPolicy):
		ep.Policy = defaultEndpointPolicy
	case in.PolicyDocument != nil:
		ep.Policy = *in.PolicyDocument
	}

	if in.PrivateDnsEnabled != nil {
		ep.PrivateDNS = *in.PrivateDnsEnabled
	}

	return checkPrivateDNS(tx, scope, ep)
}

// DeleteVpcEndpoints deletes endpoints and their routes, reporting the IDs
// that name none as unsuccessful.
func (s *Service) DeleteVpcEndpoints(
	ctx context.Context,
	in *api.DeleteVpcEndpointsRequest,
) (*api.DeleteVpcEndpointsResult, error) {
	out := &api.DeleteVpcEndpointsResult{Unsuccessful: []api.UnsuccessfulItem{}}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		for _, id := range in.VpcEndpointIds {
			ep, ok, err := storage.Read[vpcEndpoint](tx, scope.Key("ec2", kindVPCEndpoint, id))
			if err != nil {
				return err
			}

			if !ok {
				out.Unsuccessful = append(out.Unsuccessful, api.UnsuccessfulItem{
					ResourceId: new(id), Error: &api.UnsuccessfulItemError{
						Code:    new("InvalidVpcEndpoint.NotFound"),
						Message: new(fmt.Sprintf("The Vpc Endpoint Id '%s' does not exist", id)),
					},
				})

				continue
			}

			for _, table := range slices.Clone(ep.RouteTableIDs) {
				if stepErr := unrouteEndpoint(tx, scope, ep, table); stepErr != nil {
					return stepErr
				}
			}

			tx.Delete(scope.Key("ec2", kindVPCEndpoint, id))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeVpcEndpoints(
	ctx context.Context,
	in *api.DescribeVpcEndpointsRequest,
) (*api.DescribeVpcEndpointsResult, error) {
	match, err := compileFilters(in.Filters, endpointValues(request.Scope{}, &vpcEndpoint{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeVpcEndpointsResult{VpcEndpoints: []api.VpcEndpoint{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[vpcEndpoint](tx, scope, kindVPCEndpoint)
		if listErr != nil {
			return listErr
		}

		id := func(ep *vpcEndpoint) string { return ep.ID }
		selected, selectErr := selectIDs(all, id, in.VpcEndpointIds, endpointNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(ep *vpcEndpoint) bool {
			return !match.match(endpointValues(scope, ep))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, ep := range selected {
			out.VpcEndpoints = append(out.VpcEndpoints, endpointOutput(scope, ep, endpointAvailable))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DescribePrefixLists lists the AWS-managed prefix lists of the gateway
// services.
func (s *Service) DescribePrefixLists(
	ctx context.Context,
	in *api.DescribePrefixListsRequest,
) (*api.DescribePrefixListsResult, error) {
	match, err := compileFilters(in.Filters, filterPrefixList, "prefix-list-name")
	if err != nil {
		return nil, err
	}

	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	var all []*prefixList
	for _, service := range []string{serviceDynamoDB, serviceS3} {
		pl := managedPrefixList(m.Region, service)
		all = append(all, &pl)
	}

	slices.SortFunc(all, func(a, b *prefixList) int { return strings.Compare(a.ID, b.ID) })
	id := func(pl *prefixList) string { return pl.ID }
	selected, err := selectIDs(all, id, in.PrefixListIds, func(id string) error {
		return awserror.New("InvalidPrefixListID.NotFound", fmt.Sprintf("The prefix list ID '%s' does not exist", id))
	})
	if err != nil {
		return nil, err
	}

	selected = slices.DeleteFunc(selected, func(pl *prefixList) bool {
		return !match.match(values{filterPrefixList: {pl.ID}, "prefix-list-name": {pl.Name}})
	})
	selected, next, err := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
	if err != nil {
		return nil, err
	}

	out := &api.DescribePrefixListsResult{PrefixLists: []api.PrefixList{}, NextToken: next}
	for _, pl := range selected {
		out.PrefixLists = append(out.PrefixLists, api.PrefixList{
			PrefixListId: new(pl.ID), PrefixListName: new(pl.Name), Cidrs: pl.CIDRs,
		})
	}

	return out, nil
}

// endpointsUsing reports whether an endpoint uses a subnet or security group.
func endpointsUsing(tx storage.ReadTx, scope request.Scope, subnetID, groupID string) (bool, error) {
	all, err := list[vpcEndpoint](tx, scope, kindVPCEndpoint)
	return slices.ContainsFunc(all, func(ep *vpcEndpoint) bool {
		return slices.ContainsFunc(ep.Subnets, func(sn endpointSubnet) bool { return sn.ID == subnetID }) ||
			slices.Contains(ep.GroupIDs, groupID)
	}), err
}

// forgetRouteTable removes a deleted route table from the gateway endpoints
// that route through it.
func forgetRouteTable(tx storage.WriteTx, scope request.Scope, tableID string) error {
	all, err := list[vpcEndpoint](tx, scope, kindVPCEndpoint)
	if err != nil {
		return err
	}

	for _, ep := range all {
		if slices.Contains(ep.RouteTableIDs, tableID) {
			ep.RouteTableIDs = slices.DeleteFunc(ep.RouteTableIDs, func(id string) bool { return id == tableID })
			if stepErr := storage.Write(tx, scope.Key("ec2", kindVPCEndpoint, ep.ID), ep); stepErr != nil {
				return stepErr
			}
		}
	}

	return nil
}

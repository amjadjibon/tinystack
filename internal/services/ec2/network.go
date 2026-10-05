package ec2

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultVPCCIDR    = "172.31.0.0/16"
	maxVPCs           = 5
	maxDescribe       = 1000
	stateAvailable    = "available"
	filterState       = "state"
	filterOwnerID     = "owner-id"
	filterCIDR        = "cidr"
	filterVPCID       = "vpc-id"
	filterZone        = "availability-zone"
	filterSubnetID    = "subnet-id"
	filterDescription = "description"
	filterImageID     = "image-id"
	tenancyDefault    = "default"
	minPrefixLength   = 16
	maxPrefixLength   = 28
)

// zoneSuffixes name each Region's Availability Zones.
var zoneSuffixes = []string{"a", "b", "c"}

type vpc struct {
	Tags          map[string]string `json:",omitempty"`
	ID            string
	CIDR          string
	Tenancy       string
	AssociationID string
	DHCPOptionsID string
	IsDefault     bool
	// NoDNSSupport turns off enableDnsSupport, which is on by default.
	NoDNSSupport bool
	DNSHostnames bool
	UsageMetrics bool
}

type subnet struct {
	Tags  map[string]string `json:",omitempty"`
	ID    string
	VPCID string
	CIDR  string
	Zone  string
	// HostnameType is the private DNS host name type of instances launched
	// into the subnet; empty means ip-name.
	HostnameType string
	DefaultForAZ bool
	MapPublicIP  bool
	DNSARecord   bool
}

// zoneID returns the ID of a zone, such as use1-az1 for us-east-1a: the
// Region's abbreviation (us, e, 1) and the zone's position.
func zoneID(region, suffix string) string {
	abbreviations := map[string]string{
		"north": "n", "south": "s", "east": "e", "west": "w", "central": "c",
		"northeast": "ne", "northwest": "nw", "southeast": "se", "southwest": "sw",
	}
	parts := strings.Split(region, "-")
	id := parts[0]
	for _, part := range parts[1:] {
		if short, ok := abbreviations[part]; ok {
			id += short
		} else {
			id += part
		}
	}

	return fmt.Sprintf("%s-az%d", id, slices.Index(zoneSuffixes, suffix)+1)
}

// zone resolves a zone given by name or ID in region, defaulting to the
// first.
func zone(region, name, id string) (string, error) {
	for _, suffix := range zoneSuffixes {
		if (name == "" && id == "") || name == region+suffix || id == zoneID(region, suffix) {
			return region + suffix, nil
		}
	}

	given := name
	if given == "" {
		given = id
	}

	return "", invalidParameter(fmt.Sprintf("Value (%s) for parameter availabilityZone is invalid. "+
		"Subnets can currently only be created in the following availability zones: %s.", given,
		strings.Join(zoneNames(region), ", ")))
}

func zoneNames(region string) []string {
	names := make([]string, len(zoneSuffixes))
	for i, suffix := range zoneSuffixes {
		names[i] = region + suffix
	}

	return names
}

func zoneValues(region, suffix string) values {
	return values{
		"zone-name": {region + suffix}, "zone-id": {zoneID(region, suffix)}, "region-name": {region},
		filterState: {stateAvailable}, "opt-in-status": {"opt-in-not-required"}, "zone-type": {filterZone},
		"group-name": {region}, "network-border-group": {region},
	}
}

func (s *Service) DescribeAvailabilityZones(
	ctx context.Context,
	in *api.DescribeAvailabilityZonesRequest,
) (*api.DescribeAvailabilityZonesResult, error) {
	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	region := m.Region
	match, err := compileFilters(in.Filters, zoneValues(region, "").names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeAvailabilityZonesResult{AvailabilityZones: []api.AvailabilityZone{}}
	for _, suffix := range zoneSuffixes {
		name, id := region+suffix, zoneID(region, suffix)
		if (len(in.ZoneNames) > 0 || len(in.ZoneIds) > 0) &&
			!slices.Contains(in.ZoneNames, name) && !slices.Contains(in.ZoneIds, id) {
			continue
		}

		if !match.match(zoneValues(region, suffix)) {
			continue
		}

		out.AvailabilityZones = append(out.AvailabilityZones, api.AvailabilityZone{
			ZoneName: new(name), ZoneId: new(id), RegionName: new(region), State: new(stateAvailable),
			OptInStatus: new("opt-in-not-required"), ZoneType: new(filterZone),
			GroupName: new(region), NetworkBorderGroup: new(region),
		})
	}

	for _, given := range append(slices.Clone(in.ZoneNames), in.ZoneIds...) {
		if !slices.ContainsFunc(out.AvailabilityZones, func(z api.AvailabilityZone) bool {
			return *z.ZoneName == given || *z.ZoneId == given
		}) {
			return nil, invalidParameter("Invalid availability zone: [" + given + "]")
		}
	}

	return out, nil
}

// cidr parses an IPv4 CIDR block of a VPC or subnet: /16 to /28, with no host
// bits set.
func cidr(text, rangeCode string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(text)
	if err != nil || !p.Addr().Is4() || p.Masked() != p {
		return p, invalidParameter(fmt.Sprintf("Value (%s) for parameter cidrBlock is invalid. "+
			"This is not a valid CIDR block.", text))
	}

	if p.Bits() < minPrefixLength || p.Bits() > maxPrefixLength {
		return p, awserror.New(rangeCode, fmt.Sprintf("The CIDR '%s' is invalid.", text))
	}

	return p, nil
}

// defaults creates the Region's default VPC, with a default subnet in each
// zone and an internet gateway, the first time the Region is used. It also
// gives VPCs stored before route tables existed their main route table.
func (s *Service) defaults(ctx context.Context, scope request.Scope) error {
	created, upgraded := key(scope, kindDefaults, "vpc"), key(scope, kindDefaults, "network")
	done := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		_, done = tx.Get(created)
		if _, ok := tx.Get(upgraded); !ok {
			done = false
		}

		return nil
	})
	if err != nil || done {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(created); !exists {
			if err := createDefaultVPC(tx, scope); err != nil {
				return err
			}

			tx.Put(created, []byte("{}"))
		}

		if _, exists := tx.Get(upgraded); !exists {
			if err := upgradeVPCs(tx, scope); err != nil {
				return err
			}

			tx.Put(upgraded, []byte("{}"))
		}

		return nil
	})
}

// createDefaultVPC stores the Region's default VPC and what it comes with.
func createDefaultVPC(tx storage.WriteTx, scope request.Scope) error {
	v := &vpc{ID: newID("vpc"), CIDR: defaultVPCCIDR, Tenancy: tenancyDefault, IsDefault: true, DNSHostnames: true}
	g := &internetGateway{ID: newID("igw"), VPCID: v.ID}
	if err := write(tx, key(scope, kindInternetGateway, g.ID), g); err != nil {
		return err
	}

	if err := createVPC(tx, scope, v, route{CIDR: anywhere, GatewayID: g.ID, Origin: originRoute}); err != nil {
		return err
	}

	for i, suffix := range zoneSuffixes {
		sn := &subnet{
			ID: newID("subnet"), VPCID: v.ID, CIDR: fmt.Sprintf("172.31.%d.0/20", i*16),
			Zone: scope.Region + suffix, DefaultForAZ: true, MapPublicIP: true,
		}
		if err := write(tx, key(scope, kindSubnet, sn.ID), sn); err != nil {
			return err
		}
	}

	return nil
}

// upgradeVPCs gives each VPC that lacks one a main route table.
func upgradeVPCs(tx storage.WriteTx, scope request.Scope) error {
	vpcs, err := list[vpc](tx, scope, kindVPC)
	if err != nil {
		return err
	}

	for _, v := range vpcs {
		tables, listErr := vpcRouteTables(tx, scope, v.ID)
		if listErr != nil {
			return listErr
		}

		if !slices.ContainsFunc(tables, (*routeTable).main) {
			if createErr := createMainRouteTable(tx, scope, v); createErr != nil {
				return createErr
			}
		}
	}

	return nil
}

// createVPC stores a new VPC with its default security group and its main
// route table, which holds routes after the local one.
func createVPC(tx storage.WriteTx, scope request.Scope, v *vpc, routes ...route) error {
	v.AssociationID, v.DHCPOptionsID = newID("vpc-cidr-assoc"), newID("dopt")
	g := defaultGroup(v.ID)
	if err := write(tx, key(scope, kindSecurityGroup, g.ID), g); err != nil {
		return err
	}

	if err := createMainRouteTable(tx, scope, v, routes...); err != nil {
		return err
	}

	return write(tx, key(scope, kindVPC, v.ID), v)
}

func vpcOutput(scope request.Scope, v *vpc) api.Vpc {
	return api.Vpc{
		VpcId: new(v.ID), CidrBlock: new(v.CIDR), State: new(stateAvailable), OwnerId: new(scope.AccountID),
		InstanceTenancy: new(v.Tenancy), IsDefault: new(v.IsDefault), DhcpOptionsId: new(v.DHCPOptionsID),
		CidrBlockAssociationSet: []api.VpcCidrBlockAssociation{{
			AssociationId: new(v.AssociationID), CidrBlock: new(v.CIDR),
			CidrBlockState: &api.VpcCidrBlockState{State: new("associated")},
		}},
		Tags: tagList(v.Tags),
	}
}

func vpcValues(scope request.Scope, v *vpc) values {
	return values{
		filterVPCID:                             {v.ID},
		filterCIDR:                              {v.CIDR},
		"cidr-block-association.cidr-block":     {v.CIDR},
		"cidr-block-association.association-id": {v.AssociationID},
		"cidr-block-association.state":          {"associated"},
		"dhcp-options-id": {
			v.DHCPOptionsID,
		},
		"is-default":  boolValue(v.IsDefault),
		"isDefault":   boolValue(v.IsDefault),
		filterOwnerID: {scope.AccountID},
		filterState:   {stateAvailable},
	}.withTags(v.Tags)
}

func vpcNotFound(id string) error { return notFound("InvalidVpcID.NotFound", "vpc", id) }

func loadVPC(tx storage.ReadTx, scope request.Scope, id string) (*vpc, error) {
	v, ok, err := read[vpc](tx, key(scope, kindVPC, id))
	if err == nil && !ok {
		err = vpcNotFound(id)
	}

	return v, err
}

func (s *Service) CreateVpc(ctx context.Context, in *api.CreateVpcRequest) (*api.CreateVpcResult, error) {
	if in.CidrBlock == nil {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter cidrBlock")
	}

	if _, err := cidr(*in.CidrBlock, "InvalidVpc.Range"); err != nil {
		return nil, err
	}

	if value(in.AmazonProvidedIpv6CidrBlock) {
		return nil, invalidParameter("TinyStack VPCs have no IPv6 CIDR blocks")
	}

	tenancy := value(in.InstanceTenancy)
	switch tenancy {
	case "":
		tenancy = tenancyDefault
	case tenancyDefault, "dedicated":
	default:
		return nil, invalidParameter("Value (" + tenancy + ") for parameter instanceTenancy is invalid.")
	}

	tags, err := specTags(in.TagSpecifications, "vpc")
	if err != nil {
		return nil, err
	}

	v := &vpc{ID: newID("vpc"), CIDR: *in.CidrBlock, Tenancy: tenancy, Tags: tags}
	var out api.Vpc
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		if len(tx.Keys(key(scope, kindVPC, ""))) >= maxVPCs {
			return awserror.New("VpcLimitExceeded", "The maximum number of VPCs has been reached.")
		}

		out = vpcOutput(scope, v)
		return createVPC(tx, scope, v)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVpcResult{Vpc: &out}, nil
}

// DeleteVpc deletes a VPC that has no subnets, internet gateway, or security
// groups or route tables but its default and main ones left, including a
// default VPC, which does not come back.
func (s *Service) DeleteVpc(ctx context.Context, in *api.DeleteVpcRequest) (*api.DeleteVpcOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := loadVPC(tx, scope, value(in.VpcId))
		if err != nil {
			return err
		}

		subnets, err := list[subnet](tx, scope, kindSubnet)
		if err != nil {
			return err
		}

		groups, err := list[securityGroup](tx, scope, kindSecurityGroup)
		if err != nil {
			return err
		}

		gateway, err := vpcGateway(tx, scope, v.ID)
		if err != nil {
			return err
		}

		tables, err := vpcRouteTables(tx, scope, v.ID)
		if err != nil {
			return err
		}

		groups = slices.DeleteFunc(groups, func(g *securityGroup) bool { return g.VPCID != v.ID })
		if gateway != "" || slices.ContainsFunc(subnets, func(sn *subnet) bool { return sn.VPCID == v.ID }) ||
			slices.ContainsFunc(groups, func(g *securityGroup) bool { return g.Name != defaultGroupName }) ||
			slices.ContainsFunc(tables, func(rt *routeTable) bool { return !rt.main() }) {
			return awserror.New("DependencyViolation", fmt.Sprintf(dependencyFormat, kindVPC, v.ID))
		}

		for _, g := range groups {
			tx.Delete(key(scope, kindSecurityGroup, g.ID))
		}

		for _, rt := range tables {
			tx.Delete(key(scope, kindRouteTable, rt.ID))
		}

		tx.Delete(key(scope, kindVPC, v.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVpcOutput{}, nil
}

func (s *Service) DescribeVpcs(ctx context.Context, in *api.DescribeVpcsRequest) (*api.DescribeVpcsResult, error) {
	match, err := compileFilters(in.Filters, vpcValues(request.Scope{}, &vpc{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeVpcsResult{Vpcs: []api.Vpc{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[vpc](tx, scope, kindVPC)
		if listErr != nil {
			return listErr
		}

		id := func(v *vpc) string { return v.ID }
		selected, selectErr := selectIDs(all, id, in.VpcIds, vpcNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(v *vpc) bool { return !match.match(vpcValues(scope, v)) })
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, v := range selected {
			out.Vpcs = append(out.Vpcs, vpcOutput(scope, v))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func subnetOutput(scope request.Scope, sn *subnet) api.Subnet {
	p := netip.MustParsePrefix(sn.CIDR)
	suffix := strings.TrimPrefix(sn.Zone, scope.Region)
	return api.Subnet{
		SubnetId: new(sn.ID), VpcId: new(sn.VPCID), CidrBlock: new(sn.CIDR), State: new(stateAvailable),
		AvailabilityZone: new(sn.Zone), AvailabilityZoneId: new(zoneID(scope.Region, suffix)),
		// AWS reserves five addresses in every subnet.
		AvailableIpAddressCount: new(int64(1)<<(32-p.Bits()) - 5),
		DefaultForAz:            new(sn.DefaultForAZ), MapPublicIpOnLaunch: new(sn.MapPublicIP),
		OwnerId: new(scope.AccountID), AssignIpv6AddressOnCreation: new(false), Ipv6Native: new(false),
		SubnetArn: new(subnetARN(scope, sn)), EnableDns64: new(false), MapCustomerOwnedIpOnLaunch: new(false),
		PrivateDnsNameOptionsOnLaunch: &api.PrivateDnsNameOptionsOnLaunch{
			HostnameType:                    new(cmp.Or(sn.HostnameType, hostnameIPName)),
			EnableResourceNameDnsARecord:    new(sn.DNSARecord),
			EnableResourceNameDnsAAAARecord: new(false),
		},
		Tags: tagList(sn.Tags),
	}
}

func subnetARN(scope request.Scope, sn *subnet) string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:subnet/%s", scope.Region, scope.AccountID, sn.ID)
}

func subnetValues(scope request.Scope, sn *subnet) values {
	count := "0"
	if p, err := netip.ParsePrefix(sn.CIDR); err == nil {
		count = strconv.FormatInt(int64(1)<<(32-p.Bits())-5, 10)
	}

	zoneName, zoneIDValue := sn.Zone, ""
	if scope.Region != "" {
		zoneIDValue = zoneID(scope.Region, strings.TrimPrefix(sn.Zone, scope.Region))
	}

	return values{
		filterSubnetID: {sn.ID}, filterVPCID: {sn.VPCID}, "cidr-block": {sn.CIDR}, filterCIDR: {sn.CIDR},
		"cidrBlock": {sn.CIDR}, filterZone: {zoneName}, "availabilityZone": {zoneName},
		"availability-zone-id": {zoneIDValue}, "default-for-az": boolValue(sn.DefaultForAZ),
		"defaultForAz": boolValue(sn.DefaultForAZ), filterState: {stateAvailable}, filterOwnerID: {scope.AccountID},
		"map-public-ip-on-launch": boolValue(sn.MapPublicIP), "available-ip-address-count": {count},
		"subnet-arn": {subnetARN(scope, sn)}, "ipv6-native": boolValue(false),
	}.withTags(sn.Tags)
}

func subnetNotFound(id string) error { return notFound("InvalidSubnetID.NotFound", "subnet", id) }

func (s *Service) CreateSubnet(ctx context.Context, in *api.CreateSubnetRequest) (*api.CreateSubnetResult, error) {
	if in.CidrBlock == nil {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter cidrBlock")
	}

	block, err := cidr(*in.CidrBlock, "InvalidSubnet.Range")
	if err != nil {
		return nil, err
	}

	tags, err := specTags(in.TagSpecifications, "subnet")
	if err != nil {
		return nil, err
	}

	var out api.Subnet
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, loadErr := loadVPC(tx, scope, value(in.VpcId))
		if loadErr != nil {
			return loadErr
		}

		if parent := netip.MustParsePrefix(v.CIDR); !parent.Contains(block.Addr()) || block.Bits() < parent.Bits() {
			return awserror.New("InvalidSubnet.Range", fmt.Sprintf("The CIDR '%s' is invalid.", *in.CidrBlock))
		}

		subnets, listErr := list[subnet](tx, scope, kindSubnet)
		if listErr != nil {
			return listErr
		}

		for _, other := range subnets {
			if other.VPCID == v.ID && netip.MustParsePrefix(other.CIDR).Overlaps(block) {
				return awserror.New("InvalidSubnet.Conflict",
					fmt.Sprintf("The CIDR '%s' conflicts with another subnet", *in.CidrBlock))
			}
		}

		name, zoneErr := zone(scope.Region, value(in.AvailabilityZone), value(in.AvailabilityZoneId))
		if zoneErr != nil {
			return zoneErr
		}

		sn := &subnet{ID: newID("subnet"), VPCID: v.ID, CIDR: block.String(), Zone: name, Tags: tags}
		out = subnetOutput(scope, sn)
		return write(tx, key(scope, kindSubnet, sn.ID), sn)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateSubnetResult{Subnet: &out}, nil
}

func (s *Service) DeleteSubnet(ctx context.Context, in *api.DeleteSubnetRequest) (*api.DeleteSubnetOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := value(in.SubnetId)
		k := key(scope, kindSubnet, id)
		if _, ok := tx.Get(k); !ok {
			return subnetNotFound(id)
		}

		inUse, err := subnetInUse(tx, scope, id)
		if err != nil {
			return err
		}

		if inUse {
			return awserror.New("DependencyViolation", fmt.Sprintf(dependencyFormat, kindSubnet, id))
		}

		tx.Delete(k)
		return disassociateSubnet(tx, scope, id)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSubnetOutput{}, nil
}

func (s *Service) DescribeSubnets(
	ctx context.Context,
	in *api.DescribeSubnetsRequest,
) (*api.DescribeSubnetsResult, error) {
	match, err := compileFilters(in.Filters, subnetValues(request.Scope{}, &subnet{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeSubnetsResult{Subnets: []api.Subnet{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[subnet](tx, scope, kindSubnet)
		if listErr != nil {
			return listErr
		}

		id := func(sn *subnet) string { return sn.ID }
		selected, selectErr := selectIDs(all, id, in.SubnetIds, subnetNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(sn *subnet) bool { return !match.match(subnetValues(scope, sn)) })
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, sn := range selected {
			out.Subnets = append(out.Subnets, subnetOutput(scope, sn))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

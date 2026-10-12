package elbv2

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	schemeInternet = "internet-facing"
	schemeInternal = "internal"
	ipv4           = "ipv4"
	dualStack      = "dualstack"
)

var (
	namePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,30}[a-zA-Z0-9])?$`)
	// hostedZones are the Route 53 zones of load balancer DNS names, by type and
	// Region; other Regions report us-east-1's.
	hostedZones = map[string]map[string]string{
		typeApplication: {
			"us-east-1": "Z35SXDOTRQ7X7K", "us-east-2": "Z3AADJGX6KTTL2", "us-west-1": "Z368ELLRRE2KJ0",
			"us-west-2": "Z1H1FL5HABSF5", "eu-west-1": "Z32O12XQLNTSW2", "eu-central-1": "Z215JYRZR1TBD5",
			"ap-southeast-1": "Z1LMS91P8CMLE5", "ap-northeast-1": "Z14GRHDCWA56QT",
		},
		typeNetwork: {
			"us-east-1": "Z26RNL4JYFTOTI", "us-east-2": "ZLMOA37VPKANP", "us-west-1": "Z24FKFUX50B4VW",
			"us-west-2": "Z18D5FSROUN65G", "eu-west-1": "Z2IFOLAFXWLO4F", "eu-central-1": "Z3F0SRJ5LGBH90",
			"ap-southeast-1": "ZKVM4W9LS7TM", "ap-northeast-1": "Z31USIVHYNEOWT",
		},
	}
)

func hostedZone(kind, region string) string {
	return cmp.Or(hostedZones[kind][region], hostedZones[kind]["us-east-1"])
}

// ClassicHostedZone is the Route 53 zone of a Classic Load Balancer's DNS name, which AWS shares with
// Application Load Balancers in each Region.
func ClassicHostedZone(region string) string { return hostedZone(typeApplication, region) }

// balancer is a load balancer's record.
type balancer struct {
	Attributes map[string]string
	Tags       map[string]string `json:",omitempty"`
	LB         api.LoadBalancer
	ID         string
	// Label is the first label of the DNS name, which the gateway routes by.
	Label string
}

func (b *balancer) name() string     { return common.Value(b.LB.LoadBalancerName) }
func (b *balancer) kind() string     { return common.Value(b.LB.Type) }
func (b *balancer) arnValue() string { return common.Value(b.LB.LoadBalancerArn) }

// short is the type as ARNs spell it.
func short(kind string) string {
	if kind == typeNetwork {
		return "net"
	}

	return "app"
}

func lbNotFound() error {
	return awserror.New("LoadBalancerNotFoundException", "One or more load balancers not found")
}

// findLB returns the load balancer an ARN names.
func findLB(tx storage.ReadTx, scope request.Scope, arnValue string) (*balancer, error) {
	parts, ok := arnParts(arnValue, "loadbalancer", scope)
	if !ok || len(parts) != 3 {
		return nil, validation("'%s' is not a valid load balancer ARN", arnValue)
	}

	b, found, err := storage.Read[balancer](tx, scope.Key(service, kindLB, parts[1]))
	switch {
	case err != nil:
		return nil, err
	case !found || b.ID != parts[2] || short(b.kind()) != parts[0]:
		return nil, lbNotFound()
	}

	return b, nil
}

func digits(n int) string {
	var b strings.Builder
	for range n {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteString(d.String())
	}

	return b.String()
}

// placement is where a load balancer's nodes are.
type placement struct {
	vpc   string
	zones []api.AvailabilityZone
}

// place checks subnets in EC2: they must exist, share a VPC, and be in
// different zones, at least two for an application load balancer.
func (s *Service) place(ctx context.Context, scope request.Scope, kind string, subnets []string,
	mappings []api.SubnetMapping,
) (placement, error) {
	for _, m := range mappings {
		subnets = append(subnets, common.Value(m.SubnetId))
	}

	if len(subnets) == 0 {
		return placement{}, validation("At least one subnet must be specified")
	}

	out, err := s.call(ctx, scope, "ec2", "DescribeSubnets", map[string]any{"SubnetIds": anyList(subnets)})
	if err != nil {
		return placement{}, awserror.New("SubnetNotFoundException",
			"The subnet ID '"+strings.Join(subnets, ",")+"' is not valid")
	}

	var p placement
	seen := map[string]bool{}
	for _, sn := range objects(out, "Subnets") {
		zone, vpc := text(sn, "AvailabilityZone"), text(sn, "VpcId")
		switch {
		case p.vpc != "" && vpc != p.vpc:
			return placement{}, awserror.New("InvalidSubnetException", "The subnets must be in the same VPC")
		case seen[zone]:
			return placement{}, awserror.New("InvalidConfigurationRequestException",
				"A load balancer cannot be attached to multiple subnets in the same Availability Zone")
		}

		seen[zone], p.vpc = true, vpc
		p.zones = append(p.zones, api.AvailabilityZone{
			ZoneName: new(zone), SubnetId: new(text(sn, "SubnetId")),
			LoadBalancerAddresses: []api.LoadBalancerAddress{},
		})
	}

	if kind == typeApplication && len(p.zones) < 2 {
		return placement{}, validation("At least two subnets in two different Availability Zones must be specified")
	}

	slices.SortFunc(p.zones, func(a, b api.AvailabilityZone) int {
		return strings.Compare(common.Value(a.ZoneName), common.Value(b.ZoneName))
	})
	return p, nil
}

// securityGroups checks security groups in a VPC; an application load
// balancer without any gets the VPC's default group.
func (s *Service) securityGroups(ctx context.Context, scope request.Scope, vpc string, ids []string,
	fallback bool,
) ([]string, error) {
	params := map[string]any{"GroupIds": anyList(ids)}
	if len(ids) == 0 {
		if !fallback {
			return []string{}, nil
		}

		params = map[string]any{"Filters": []any{
			map[string]any{memberName: "vpc-id", memberValues: []any{vpc}},
			map[string]any{memberName: "group-name", memberValues: []any{"default"}},
		}}
	}

	out, err := s.call(ctx, scope, "ec2", "DescribeSecurityGroups", params)
	if err != nil {
		return nil, awserror.New("InvalidSecurityGroupException", "One or more security groups are invalid")
	}

	var found []string
	for _, g := range objects(out, "SecurityGroups") {
		if text(g, "VpcId") != vpc {
			return nil, awserror.New("InvalidSecurityGroupException",
				"Security group '"+text(g, "GroupId")+"' does not belong to VPC '"+vpc+"'")
		}

		found = append(found, text(g, "GroupId"))
	}

	return found, nil
}

func (s *Service) CreateLoadBalancer(
	ctx context.Context, in *api.CreateLoadBalancerInput,
) (*api.CreateLoadBalancerOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, kind, scheme := common.Value(in.Name), common.Value(in.Type), common.Value(in.Scheme)
	if kind == "" {
		kind = typeApplication
	}

	if scheme == "" {
		scheme = schemeInternet
	}

	addressType := common.Value(in.IpAddressType)
	if addressType == "" {
		addressType = ipv4
	}

	switch {
	case !namePattern.MatchString(name) || strings.HasPrefix(name, "internal-"):
		return nil, validation("The load balancer name '%s' must be 1 to 32 letters, digits, and hyphens, and "+
			"must not begin or end with a hyphen or begin with 'internal-'", name)
	case kind != typeApplication && kind != typeNetwork:
		return nil, validation("TinyStack supports application and network load balancers, not %s", kind)
	case scheme != schemeInternet && scheme != schemeInternal:
		return nil, awserror.New("InvalidSchemeException", "Scheme must be internet-facing or internal")
	case addressType != ipv4 && addressType != dualStack:
		return nil, validation("IpAddressType must be ipv4 or dualstack")
	}

	where, err := s.place(ctx, m.Scope, kind, in.Subnets, in.SubnetMappings)
	if err != nil {
		return nil, err
	}

	groups, err := s.securityGroups(ctx, m.Scope, where.vpc, in.SecurityGroups, kind == typeApplication)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	id := resourceID()
	label := name + "-" + digits(10)
	if kind == typeNetwork {
		label = name + "-" + id
	}

	if scheme == schemeInternal {
		label = "internal-" + label
	}

	dns := label + "." + m.Region + ".elb.amazonaws.com"
	if kind == typeNetwork {
		dns = label + ".elb." + m.Region + ".amazonaws.com"
	}

	zone := hostedZone(kind, m.Region)

	defaults := applicationAttributes
	if kind == typeNetwork {
		defaults = networkAttributes
	}

	b := &balancer{ID: id, Label: label, Tags: tags, Attributes: defaults, LB: api.LoadBalancer{
		LoadBalancerArn: new(arn(m.Scope, "loadbalancer/"+short(kind)+"/"+name+"/"+id)),
		DNSName:         new(dns), CanonicalHostedZoneId: new(zone), CreatedTime: new(s.now().UTC()),
		LoadBalancerName: new(name), Scheme: new(scheme), VpcId: new(where.vpc),
		State: &api.LoadBalancerState{Code: new(stateActive)}, Type: new(kind), AvailabilityZones: where.zones,
		SecurityGroups: groups, IpAddressType: new(addressType),
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		old, found, readErr := storage.Read[balancer](tx, m.Key(service, kindLB, name))
		switch {
		case readErr != nil:
			return readErr
		case found && old.kind() == kind && common.Value(old.LB.Scheme) == scheme:
			// Creating a load balancer again with the same settings returns it.
			b = old
			return nil
		case found:
			return awserror.New("DuplicateLoadBalancerNameException", "A load balancer with the same name '"+name+
				"' exists, but with different settings")
		}

		if writeErr := storage.Write(tx, dnsPrefix+strings.ToLower(label), dnsEntry{
			Account: m.AccountID, Region: m.Region, Name: name,
		}); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, m.Key(service, kindLB, name), b)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLoadBalancerOutput{LoadBalancers: []api.LoadBalancer{b.LB}}, nil
}

func (s *Service) DescribeLoadBalancers(
	ctx context.Context, in *api.DescribeLoadBalancersInput,
) (*api.DescribeLoadBalancersOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var selected []*balancer
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := storage.Records[balancer](tx, m.Key(service, kindLB, ""))
		if listErr != nil {
			return listErr
		}

		for _, want := range in.LoadBalancerArns {
			if !slices.ContainsFunc(all, func(b *balancer) bool { return b.arnValue() == want }) {
				return lbNotFound()
			}
		}

		for _, want := range in.Names {
			if !slices.ContainsFunc(all, func(b *balancer) bool { return b.name() == want }) {
				return awserror.New("LoadBalancerNotFoundException", "Load balancers '["+want+"]' not found")
			}
		}

		for _, b := range all {
			if (len(in.LoadBalancerArns) == 0 || slices.Contains(in.LoadBalancerArns, b.arnValue())) &&
				(len(in.Names) == 0 || slices.Contains(in.Names, b.name())) {
				selected = append(selected, b)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(selected, (*balancer).name, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeLoadBalancersOutput{LoadBalancers: []api.LoadBalancer{}, NextMarker: next}
	for _, b := range selected {
		out.LoadBalancers = append(out.LoadBalancers, b.LB)
	}

	return out, nil
}

// DeleteLoadBalancer deletes a load balancer with its listeners and rules;
// deleting one that does not exist succeeds.
func (s *Service) DeleteLoadBalancer(
	ctx context.Context, in *api.DeleteLoadBalancerInput,
) (*api.DeleteLoadBalancerOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	return &api.DeleteLoadBalancerOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		b, findErr := findLB(tx, m.Scope, common.Value(in.LoadBalancerArn))
		switch {
		case findErr != nil && awserror.From(findErr).Code == "LoadBalancerNotFoundException":
			return nil
		case findErr != nil:
			return findErr
		case b.Attributes["deletion_protection.enabled"] == valueTrue:
			return awserror.New("OperationNotPermittedException", "Load balancer '"+b.arnValue()+
				"' cannot be deleted because deletion protection is enabled")
		}

		for _, k := range tx.Keys(m.Key(service, kindListener, b.name()+"/")) {
			for _, r := range tx.Keys(m.Key(service, kindRule, k[strings.LastIndexByte(k, '/')+1:]+"/")) {
				tx.Delete(r)
			}

			tx.Delete(k)
		}

		tx.Delete(dnsPrefix + strings.ToLower(b.Label))
		tx.Delete(m.Key(service, kindLB, b.name()))
		return nil
	})
}

// changeLB runs fn on a load balancer's record in a transaction and saves it.
func (s *Service) changeLB(ctx context.Context, arnValue string, fn func(*balancer) error) (*balancer, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var b *balancer
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var findErr error
		if b, findErr = findLB(tx, m.Scope, arnValue); findErr != nil {
			return findErr
		}

		if fnErr := fn(b); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, m.Key(service, kindLB, b.name()), b)
	})

	return b, err
}

func lbAttributes(b *balancer) []api.LoadBalancerAttribute {
	out := []api.LoadBalancerAttribute{}
	for _, k := range attributeKeys(b.Attributes) {
		out = append(out, api.LoadBalancerAttribute{Key: new(k), Value: new(b.Attributes[k])})
	}

	return out
}

func (s *Service) DescribeLoadBalancerAttributes(
	ctx context.Context, in *api.DescribeLoadBalancerAttributesInput,
) (*api.DescribeLoadBalancerAttributesOutput, error) {
	b, err := s.changeLB(ctx, common.Value(in.LoadBalancerArn), func(*balancer) error { return nil })
	if err != nil {
		return nil, err
	}

	return &api.DescribeLoadBalancerAttributesOutput{Attributes: lbAttributes(b)}, nil
}

func (s *Service) ModifyLoadBalancerAttributes(
	ctx context.Context, in *api.ModifyLoadBalancerAttributesInput,
) (*api.ModifyLoadBalancerAttributesOutput, error) {
	keys, values := make([]string, len(in.Attributes)), make([]string, len(in.Attributes))
	for i, a := range in.Attributes {
		keys[i], values[i] = common.Value(a.Key), common.Value(a.Value)
	}

	b, err := s.changeLB(ctx, common.Value(in.LoadBalancerArn), func(b *balancer) error {
		defaults := applicationAttributes
		if b.kind() == typeNetwork {
			defaults = networkAttributes
		}

		var setErr error
		b.Attributes, setErr = setAttributes(b.Attributes, defaults, keys, values)
		return setErr
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyLoadBalancerAttributesOutput{Attributes: lbAttributes(b)}, nil
}

func (s *Service) SetSecurityGroups(
	ctx context.Context, in *api.SetSecurityGroupsInput,
) (*api.SetSecurityGroupsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var vpc string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		b, findErr := findLB(tx, m.Scope, common.Value(in.LoadBalancerArn))
		if findErr == nil {
			vpc = common.Value(b.LB.VpcId)
		}

		return findErr
	})
	if err != nil {
		return nil, err
	}

	groups, err := s.securityGroups(ctx, m.Scope, vpc, in.SecurityGroups, false)
	if err != nil {
		return nil, err
	}

	if _, err = s.changeLB(ctx, common.Value(in.LoadBalancerArn), func(b *balancer) error {
		b.LB.SecurityGroups = groups
		return nil
	}); err != nil {
		return nil, err
	}

	return &api.SetSecurityGroupsOutput{SecurityGroupIds: groups}, nil
}

func (s *Service) SetSubnets(ctx context.Context, in *api.SetSubnetsInput) (*api.SetSubnetsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var kind string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		b, findErr := findLB(tx, m.Scope, common.Value(in.LoadBalancerArn))
		if findErr == nil {
			kind = b.kind()
		}

		return findErr
	})
	if err != nil {
		return nil, err
	}

	where, err := s.place(ctx, m.Scope, kind, in.Subnets, in.SubnetMappings)
	if err != nil {
		return nil, err
	}

	b, err := s.changeLB(ctx, common.Value(in.LoadBalancerArn), func(b *balancer) error {
		if where.vpc != common.Value(b.LB.VpcId) {
			return awserror.New("InvalidSubnetException", fmt.Sprintf("The subnets must be in VPC '%s'",
				common.Value(b.LB.VpcId)))
		}

		b.LB.AvailabilityZones = where.zones
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.SetSubnetsOutput{AvailabilityZones: b.LB.AvailabilityZones, IpAddressType: b.LB.IpAddressType}, nil
}

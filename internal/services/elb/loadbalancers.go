package elb

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/elbv2"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxNameLength = 32
	defaultPage   = 400
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

func validName(name string) bool {
	return len(name) >= 1 && len(name) <= maxNameLength && namePattern.MatchString(name) &&
		!strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-")
}

// placement is where a load balancer's nodes are: subnets of one VPC, or bare Availability Zones.
type placement struct {
	vpc     string
	subnets []string
	zones   []string
	groups  []string
}

// place checks the subnets or Availability Zones and security groups of a new load balancer in the transaction.
func (s *Service) place(
	tx storage.ReadTx,
	scope request.Scope,
	in *api.CreateAccessPointInput,
) (placement, error) {
	var p placement
	switch {
	case len(in.Subnets) > 0 && len(in.AvailabilityZones) > 0:
		return p, configuration("Specify either Subnets or AvailabilityZones, not both")
	case len(in.Subnets) == 0 && len(in.AvailabilityZones) == 0:
		return p, configuration("Either Subnets or AvailabilityZones must be specified")
	case len(in.Subnets) == 0:
		return s.placeZones(scope, in)
	}

	for _, id := range in.Subnets {
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return p, remap(err, "SubnetNotFoundException")
		}

		zone := common.Value(sn.AvailabilityZone)
		switch {
		case p.vpc != "" && p.vpc != common.Value(sn.VpcId):
			return p, awserror.New("InvalidSubnetException", "The subnets must be in the same VPC")
		case slices.Contains(p.zones, zone):
			return p, awserror.New("InvalidSubnetException", "Only one subnet per Availability Zone is allowed")
		}

		p.vpc = common.Value(sn.VpcId)
		p.subnets = append(p.subnets, id)
		p.zones = append(p.zones, zone)
	}

	groups, err := s.network.MountGroups(tx, scope, p.vpc, in.SecurityGroups)
	if err != nil {
		return p, remap(err, "InvalidSecurityGroupException")
	}

	p.groups = groups
	return p, nil
}

// placeZones places a load balancer outside any VPC, which has no subnets or security groups.
func (s *Service) placeZones(scope request.Scope, in *api.CreateAccessPointInput) (placement, error) {
	var p placement
	if len(in.SecurityGroups) > 0 {
		return p, configuration("SecurityGroups require Subnets")
	}

	for _, zone := range in.AvailabilityZones {
		if !slices.Contains(s.network.Zones(scope.Region), zone) {
			return p, invalid("'%s' is not an Availability Zone of %s", zone, scope.Region)
		}

		if !slices.Contains(p.zones, zone) {
			p.zones = append(p.zones, zone)
		}
	}

	return p, nil
}

func (s *Service) CreateLoadBalancer(
	ctx context.Context,
	in *api.CreateAccessPointInput,
) (*api.CreateAccessPointOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	name := common.Value(in.LoadBalancerName)
	if !validName(name) {
		return nil, invalid("LoadBalancerName must be 1 to 32 alphanumeric characters or hyphens and cannot begin " +
			"or end with a hyphen")
	}

	listeners, err := checkListeners(in.Listeners)
	if err != nil {
		return nil, err
	}

	scheme := common.Value(common.OrElse(in.Scheme, new(schemeInternet)))
	if scheme != schemeInternet && scheme != schemeInternal {
		return nil, awserror.New("InvalidSchemeException", "Scheme must be internet-facing or internal")
	}

	if scheme == schemeInternal && len(in.Subnets) == 0 {
		return nil, awserror.New("InvalidSchemeException", "Scheme internal is valid only for load balancers in a VPC")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	label := name + "-" + common.RandomID(10, digits)
	if scheme == schemeInternal {
		label = "internal-" + label
	}

	dns := label + "." + scope.Region + ".elb.amazonaws.com"
	zone := elbv2.ClassicHostedZone(scope.Region)
	b := &balancer{Tags: tags, Attributes: defaultAttributes(), Description: api.LoadBalancerDescription{
		LoadBalancerName: new(name), DNSName: new(dns), CanonicalHostedZoneName: new(dns),
		CanonicalHostedZoneNameID: new(zone), ListenerDescriptions: listeners,
		HealthCheck: defaultHealthCheck(in.Listeners), Instances: []api.Instance{},
		SourceSecurityGroup: &api.SourceSecurityGroup{
			OwnerAlias: new(sourceGroupOwner), GroupName: new(sourceGroupName),
		},
		CreatedTime: new(time.Now().UTC()), Scheme: new(scheme),
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(scope.Key(serviceName, balancerKind, name)); exists {
			return awserror.New("DuplicateAccessPointNameException",
				"The specified load balancer name already exists for this account: "+name)
		}

		p, e := s.place(tx, scope, in)
		if e != nil {
			return e
		}

		b.Description.AvailabilityZones, b.Description.Subnets = p.zones, p.subnets
		b.Description.SecurityGroups = p.groups
		if p.vpc != "" {
			b.Description.VPCId = new(p.vpc)
		}

		return save(tx, scope, b)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAccessPointOutput{DNSName: new(dns)}, nil
}

func (s *Service) DescribeLoadBalancers(
	ctx context.Context,
	in *api.DescribeAccessPointsInput,
) (*api.DescribeAccessPointsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	limit := int(common.Value(common.OrElse(in.PageSize, new(int64(defaultPage)))))
	if limit < 1 || limit > defaultPage {
		return nil, invalid("PageSize must be between 1 and %d", defaultPage)
	}

	var items []*balancer
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := storage.Records[balancer](tx, scope.Prefix(serviceName, balancerKind))
		if e != nil {
			return e
		}

		for _, name := range in.LoadBalancerNames {
			if !slices.ContainsFunc(all, func(b *balancer) bool { return b.name() == name }) {
				return notFound(name)
			}
		}

		items = slices.DeleteFunc(all, func(b *balancer) bool {
			return len(in.LoadBalancerNames) > 0 && !slices.Contains(in.LoadBalancerNames, b.name())
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	items, marker, err := common.Page(items, (*balancer).name, in.Marker, limit, invalid("Invalid Marker"))
	if err != nil {
		return nil, err
	}

	out := &api.DescribeAccessPointsOutput{
		NextMarker:               marker,
		LoadBalancerDescriptions: []api.LoadBalancerDescription{},
	}
	for _, b := range items {
		out.LoadBalancerDescriptions = append(out.LoadBalancerDescriptions, b.Description)
	}

	return out, nil
}

// DeleteLoadBalancer succeeds for a load balancer that does not exist, as AWS does.
func (s *Service) DeleteLoadBalancer(ctx context.Context, in *api.DeleteAccessPointInput) (
	*api.DeleteAccessPointOutput, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.LoadBalancerName == nil {
		return nil, invalid("LoadBalancerName is required")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, balancerKind, *in.LoadBalancerName))
		return nil
	})

	return &api.DeleteAccessPointOutput{}, err
}

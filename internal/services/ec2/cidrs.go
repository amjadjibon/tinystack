package ec2

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type cidrAssociation struct {
	ID          string
	CIDR        string
	State       string
	BorderGroup string
	Pool        string
}

func vpcIPv4Blocks(v *vpc) []string {
	blocks := []string{v.CIDR}
	for _, a := range v.CIDRs {
		if a.State == stateAssociated {
			blocks = append(blocks, a.CIDR)
		}
	}

	return blocks
}

func vpcIPv6Blocks(v *vpc) []string {
	var blocks []string
	for _, a := range v.IPv6CIDRs {
		if a.State == stateAssociated {
			blocks = append(blocks, a.CIDR)
		}
	}

	return blocks
}

func containsBlock(blocks []string, p netip.Prefix) bool {
	return slices.ContainsFunc(blocks, func(block string) bool {
		parent := netip.MustParsePrefix(block)
		return parent.Bits() <= p.Bits() && parent.Contains(p.Addr())
	})
}

func ipv6Block(text string, bits int) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(text)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Masked() != p || p.Bits() != bits {
		return p, invalidParameter(fmt.Sprintf("IPv6 CIDR must be a canonical /%d block", bits))
	}

	return p, nil
}

// amazonIPv6 assigns a stable /56 from AWS's example IPv6 range, without network I/O.
func amazonIPv6(scope request.Scope, vpcID string) string {
	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/" + vpcID))
	return fmt.Sprintf("2600:1f18:%x:%x00::/56", (uint16(sum[0])<<8|uint16(sum[1]))&0x7fff, sum[2])
}

func addVpcIPv6(scope request.Scope, v *vpc, text, border string) error {
	p, err := ipv6Block(text, 56)
	if err != nil {
		return err
	}

	for _, block := range vpcIPv6Blocks(v) {
		if netip.MustParsePrefix(block).Overlaps(p) {
			return invalidParameter("IPv6 CIDR overlaps an associated block")
		}
	}

	if border == "" {
		border = scope.Region
	}

	if border != scope.Region {
		return invalidParameter("Unsupported IPv6 network border group")
	}

	v.IPv6CIDRs = append(v.IPv6CIDRs, cidrAssociation{
		ID:   newID("vpc-cidr-assoc"),
		CIDR: p.String(), State: stateAssociated, BorderGroup: border,
	})
	return nil
}

func subnetIPv6Output(a *cidrAssociation) api.SubnetIpv6CidrBlockAssociation {
	source := amazonAlias
	if a.Pool != "" {
		source = "byoip"
	}
	return api.SubnetIpv6CidrBlockAssociation{
		AssociationId: new(a.ID), Ipv6CidrBlock: new(a.CIDR),
		Ipv6CidrBlockState:   &api.SubnetCidrBlockState{State: new(a.State)},
		Ipv6AddressAttribute: new("public"), IpSource: new(source),
	}
}

func vpcIPv6Output(a *cidrAssociation) api.VpcIpv6CidrBlockAssociation {
	pool, source := "Amazon", amazonAlias
	if a.Pool != "" {
		pool, source = a.Pool, "byoip"
	}
	return api.VpcIpv6CidrBlockAssociation{
		AssociationId: new(a.ID), Ipv6CidrBlock: new(a.CIDR),
		Ipv6CidrBlockState: &api.VpcCidrBlockState{State: new(a.State)}, Ipv6Pool: new(pool),
		NetworkBorderGroup: new(a.BorderGroup), Ipv6AddressAttribute: new("public"), IpSource: new(source),
	}
}

func addSubnetIPv6(tx storage.ReadTx, scope request.Scope, v *vpc, sn *subnet, text string) error {
	p, err := ipv6Block(text, 64)
	if err != nil {
		return err
	}

	if !containsBlock(vpcIPv6Blocks(v), p) {
		return awserror.New("InvalidSubnet.Range", "IPv6 block is outside the VPC")
	}

	if slices.ContainsFunc(sn.IPv6CIDRs, func(a cidrAssociation) bool { return a.State == stateAssociated }) {
		return awserror.New("InvalidSubnet.Conflict", "Subnet already has an IPv6 block")
	}

	all, err := storage.Records[subnet](tx, scope.Key("ec2", kindSubnet, ""))
	if err != nil {
		return err
	}

	for _, other := range all {
		if other.VPCID != v.ID {
			continue
		}

		for _, a := range other.IPv6CIDRs {
			if a.State == stateAssociated && netip.MustParsePrefix(a.CIDR).Overlaps(p) {
				return awserror.New("InvalidSubnet.Conflict", "IPv6 block overlaps another subnet")
			}
		}
	}

	pool := ""
	for _, a := range v.IPv6CIDRs {
		if a.State == stateAssociated && netip.MustParsePrefix(a.CIDR).Contains(p.Addr()) {
			pool = a.Pool
		}
	}
	sn.IPv6CIDRs = append(sn.IPv6CIDRs, cidrAssociation{
		ID:   newID("subnet-cidr-assoc"),
		CIDR: p.String(), State: stateAssociated, Pool: pool,
	})
	return nil
}

func secondaryIPv4(v *vpc, text string) (netip.Prefix, error) {
	p, err := cidr(text, "InvalidVpc.Range")
	if err != nil {
		return p, err
	}

	for _, denied := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"} {
		if netip.MustParsePrefix(denied).Overlaps(p) {
			return p, invalidParameter("IPv4 CIDR is reserved")
		}
	}

	private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	category := func(block netip.Prefix) int {
		for i, text := range private {
			if netip.MustParsePrefix(text).Overlaps(block) {
				return i
			}
		}

		return -1
	}

	for _, text := range vpcIPv4Blocks(v) {
		existing := netip.MustParsePrefix(text)
		if netip.MustParsePrefix("10.0.0.0/15").Overlaps(existing) &&
			netip.MustParsePrefix("10.0.0.0/16").Overlaps(p) {
			return p, invalidParameter("Restricted secondary IPv4 range")
		}
		if existing.Overlaps(p) {
			return p, invalidParameter("CIDR overlaps an associated block")
		}

		if category(existing) != category(p) && category(p) != -1 {
			return p, invalidParameter("Secondary CIDR cannot use another RFC1918 range")
		}

		if category(existing) >= 0 && netip.MustParsePrefix("198.19.0.0/16").Overlaps(p) {
			return p, invalidParameter("198.19.0.0/16 cannot be associated with RFC1918 ranges")
		}

		if category(existing) == 1 && netip.MustParsePrefix("172.31.0.0/16").Overlaps(p) {
			return p, invalidParameter("172.31.0.0/16 cannot be a secondary CIDR in this range")
		}
	}

	return p, nil
}

// syncLocalRoutes keeps all VPC tables' local routes consistent with active CIDR associations.
func syncLocalRoutes(tx storage.WriteTx, scope request.Scope, v *vpc) error {
	tables, err := vpcRouteTables(tx, scope, v.ID)
	if err != nil {
		return err
	}

	for _, rt := range tables {
		rt.Routes = slices.DeleteFunc(rt.Routes, func(r route) bool { return r.GatewayID == targetLocal })
		for _, block := range vpcIPv4Blocks(v) {
			rt.Routes = append(rt.Routes, route{CIDR: block, GatewayID: targetLocal, Origin: originTable})
		}

		for _, block := range vpcIPv6Blocks(v) {
			rt.Routes = append(rt.Routes, route{CIDRv6: block, GatewayID: targetLocal, Origin: originTable})
		}

		if err := storage.Write(tx, scope.Key("ec2", kindRouteTable, rt.ID), rt); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) AssociateVpcCidrBlock(ctx context.Context, in *api.AssociateVpcCidrBlockRequest) (
	*api.AssociateVpcCidrBlockResult, error,
) {
	if err := onlyOne(
		in.CidrBlock != nil,
		common.Value(in.AmazonProvidedIpv6CidrBlock),
		in.Ipv6CidrBlock != nil,
	); err != nil {
		return nil, err
	}

	if err := explicitIPv6Pool(
		in.Ipv6CidrBlock,
		in.Ipv6Pool,
		common.Value(in.AmazonProvidedIpv6CidrBlock),
	); err != nil {
		return nil, err
	}
	out := &api.AssociateVpcCidrBlockResult{VpcId: in.VpcId}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := loadVPC(tx, scope, common.Value(in.VpcId))
		if err != nil {
			return err
		}

		if in.CidrBlock != nil {
			if in.Ipv6CidrBlockNetworkBorderGroup != nil {
				return invalidParameter("Border group requires IPv6")
			}

			p, err := secondaryIPv4(v, *in.CidrBlock)
			if err != nil {
				return err
			}

			tables, err := vpcRouteTables(tx, scope, v.ID)
			if err != nil {
				return err
			}

			for _, rt := range tables {
				for i := range rt.Routes {
					r := &rt.Routes[i]
					if r.CIDR != "" && r.GatewayID != targetLocal && p.Contains(netip.MustParsePrefix(r.CIDR).Addr()) &&
						p.Bits() <= netip.MustParsePrefix(r.CIDR).Bits() {
						return invalidParameter("CIDR conflicts with a route")
					}
				}
			}

			if err := checkPeeringCIDR(tx, scope, v.ID, p); err != nil {
				return err
			}

			a := cidrAssociation{ID: newID("vpc-cidr-assoc"), CIDR: p.String(), State: stateAssociated}
			v.CIDRs = append(v.CIDRs, a)
			out.CidrBlockAssociation = &api.VpcCidrBlockAssociation{
				AssociationId: new(a.ID), CidrBlock: new(a.CIDR),
				CidrBlockState: &api.VpcCidrBlockState{State: new(a.State)},
			}
		} else {
			text := common.Value(in.Ipv6CidrBlock)
			if common.Value(in.AmazonProvidedIpv6CidrBlock) {
				text = amazonIPv6(scope, v.ID)
			}

			block, parseErr := ipv6Block(text, 56)
			if parseErr != nil {
				return parseErr
			}
			if peerErr := checkPeeringCIDR(tx, scope, v.ID, block); peerErr != nil {
				return peerErr
			}
			if err := addVpcIPv6(scope, v, text, common.Value(in.Ipv6CidrBlockNetworkBorderGroup)); err != nil {
				return err
			}

			v.IPv6CIDRs[len(v.IPv6CIDRs)-1].Pool = common.Value(in.Ipv6Pool)
			out.Ipv6CidrBlockAssociation = new(vpcIPv6Output(&v.IPv6CIDRs[len(v.IPv6CIDRs)-1]))
		}

		if err := refreshPeeringCIDRs(tx, scope, v); err != nil {
			return err
		}
		if err := syncLocalRoutes(tx, scope, v); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key("ec2", kindVPC, v.ID), v)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DisassociateVpcCidrBlock(ctx context.Context, in *api.DisassociateVpcCidrBlockRequest) (
	*api.DisassociateVpcCidrBlockResult, error,
) {
	out := &api.DisassociateVpcCidrBlockResult{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		all, err := storage.Records[vpc](tx, scope.Key("ec2", kindVPC, ""))
		if err != nil {
			return err
		}

		id := common.Value(in.AssociationId)
		for _, v := range all {
			if v.AssociationID == id {
				return awserror.New("OperationNotPermitted", "Cannot remove primary IPv4 block")
			}

			for _, associations := range [][]cidrAssociation{v.CIDRs, v.IPv6CIDRs} {
				for i := range associations {
					a := &associations[i]
					if a.ID != id {
						continue
					}

					if a.State != stateAssociated {
						return invalidParameter("CIDR block is not associated")
					}

					subnets, err := storage.Records[subnet](tx, scope.Key("ec2", kindSubnet, ""))
					if err != nil {
						return err
					}

					p := netip.MustParsePrefix(a.CIDR)
					for _, sn := range subnets {
						if sn.VPCID != v.ID {
							continue
						}

						if p.Overlaps(netip.MustParsePrefix(sn.CIDR)) {
							return awserror.New("DependencyViolation", "CIDR has subnets")
						}

						for _, sa := range sn.IPv6CIDRs {
							if sa.State == stateAssociated && p.Overlaps(netip.MustParsePrefix(sa.CIDR)) {
								return awserror.New("DependencyViolation", "CIDR has IPv6 subnets")
							}
						}
					}

					a.State = "disassociated"
					out.VpcId = new(v.ID)
					if p.Addr().Is4() {
						out.CidrBlockAssociation = &api.VpcCidrBlockAssociation{
							AssociationId: new(id), CidrBlock: new(a.CIDR),
							CidrBlockState: &api.VpcCidrBlockState{State: new(a.State)},
						}
					} else {
						out.Ipv6CidrBlockAssociation = new(vpcIPv6Output(a))
					}

					if err := refreshPeeringCIDRs(tx, scope, v); err != nil {
						return err
					}
					if err := syncLocalRoutes(tx, scope, v); err != nil {
						return err
					}

					return storage.Write(tx, scope.Key("ec2", kindVPC, v.ID), v)
				}
			}
		}

		return associationNotFound(id)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) AssociateSubnetCidrBlock(ctx context.Context, in *api.AssociateSubnetCidrBlockRequest) (
	*api.AssociateSubnetCidrBlockResult, error,
) {
	out := &api.AssociateSubnetCidrBlockResult{SubnetId: in.SubnetId}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, common.Value(in.SubnetId)))
		if err != nil || !ok {
			return common.ErrorsOr(err, subnetNotFound(common.Value(in.SubnetId)))
		}

		v, err := loadVPC(tx, scope, sn.VPCID)
		if err != nil {
			return err
		}

		if err := addSubnetIPv6(tx, scope, v, sn, common.Value(in.Ipv6CidrBlock)); err != nil {
			return err
		}

		out.Ipv6CidrBlockAssociation = new(subnetIPv6Output(&sn.IPv6CIDRs[len(sn.IPv6CIDRs)-1]))
		return storage.Write(tx, scope.Key("ec2", kindSubnet, sn.ID), sn)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DisassociateSubnetCidrBlock(ctx context.Context, in *api.DisassociateSubnetCidrBlockRequest) (
	*api.DisassociateSubnetCidrBlockResult, error,
) {
	out := &api.DisassociateSubnetCidrBlockResult{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		all, err := storage.Records[subnet](tx, scope.Key("ec2", kindSubnet, ""))
		if err != nil {
			return err
		}

		for _, sn := range all {
			for i := range sn.IPv6CIDRs {
				a := &sn.IPv6CIDRs[i]
				if a.ID != common.Value(in.AssociationId) {
					continue
				}

				if a.State != stateAssociated {
					return invalidParameter("CIDR block is not associated")
				}

				enis, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
				if err != nil {
					return err
				}

				if slices.ContainsFunc(
					enis,
					func(ni *networkInterface) bool { return ni.SubnetID == sn.ID && len(ni.IPv6) > 0 },
				) {
					return awserror.New("DependencyViolation", "Subnet IPv6 block has network interfaces")
				}

				a.State, sn.AssignIPv6 = "disassociated", false
				out.SubnetId, out.Ipv6CidrBlockAssociation = new(sn.ID), new(subnetIPv6Output(a))
				return storage.Write(tx, scope.Key("ec2", kindSubnet, sn.ID), sn)
			}
		}

		return associationNotFound(common.Value(in.AssociationId))
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// explicitIPv6Pool implements the model's CIDR/pool cross-field contract. Pool ownership and provisioning
// remain outside this records-only API; explicit CIDRs keep the supplied external pool reference.
func explicitIPv6Pool(cidr, pool *string, amazon bool) error {
	if amazon && pool != nil {
		return invalidParameter("AmazonProvidedIpv6CidrBlock cannot be combined with Ipv6Pool")
	}
	if cidr != nil && common.Value(pool) == "" {
		return awserror.New("MissingParameter", "Ipv6CidrBlock requires Ipv6Pool")
	}
	if pool != nil && cidr == nil {
		return awserror.New(
			"NotImplemented",
			"Allocation from an external IPv6 pool requires an explicit Ipv6CidrBlock",
		)
	}
	return nil
}

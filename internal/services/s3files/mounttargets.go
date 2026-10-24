package s3files

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/mountaddr"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	ipv4Only  = "IPV4_ONLY"
	ipv6Only  = "IPV6_ONLY"
	dualStack = "DUAL_STACK"
)

func mtResponse(scope request.Scope, r *mountTarget) *api.GetMountTargetResponse {
	out := &api.GetMountTargetResponse{
		AvailabilityZoneId: new(r.ZoneID),
		OwnerId:            new(scope.AccountID),
		MountTargetId:      new(r.ID),
		FileSystemId:       new(r.FileSystemID),
		SubnetId:           new(r.SubnetID),
		VpcId:              new(r.VpcID),
		SecurityGroups:     slices.Clone(r.SecurityGroup),
		Status:             new(r.State),
	}

	if r.IPv4 != "" {
		out.Ipv4Address = new(r.IPv4)
	}

	if r.IPv6 != "" {
		out.Ipv6Address = new(r.IPv6)
	}

	return out
}

// ec2Error reports a missing EC2 reference as not found and any other EC2 rejection as invalid input.
func ec2Error(err error) error {
	e, ok := errors.AsType[*awserror.Error](err)
	if !ok {
		return err
	}

	if strings.HasSuffix(e.Code, ".NotFound") {
		return notFound("%s", e.Message)
	}

	return invalid("%s", e.Message)
}

func addressError(err error, name string) error {
	switch {
	case errors.Is(err, mountaddr.ErrOutsideSubnet):
		return invalid("%s must be a usable address in the subnet CIDR", name)
	case errors.Is(err, mountaddr.ErrInUse):
		return conflict("%s is already used by another mount target in the subnet", name)
	case errors.Is(err, mountaddr.ErrExhausted):
		return conflict("The subnet has no unused %s addresses", name)
	}

	return err
}

func parseAddress(value *string, name string, v6 bool) (netip.Addr, error) {
	if value == nil {
		return netip.Addr{}, nil
	}

	addr, err := netip.ParseAddr(*value)
	if err != nil || addr.Is6() != v6 || addr.Is4In6() {
		family := "IPv4"
		if v6 {
			family = "IPv6"
		}

		return netip.Addr{}, invalid("%s must be an %s address", name, family)
	}

	return addr, nil
}

func checkGroups(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return invalid("securityGroups must not repeat %s", id)
		}

		seen[id] = true
	}

	return nil
}

// associatedIPv6 returns the first associated IPv6 CIDR of the subnet.
func associatedIPv6(subnet *ec2api.Subnet) (netip.Prefix, bool) {
	for _, a := range subnet.Ipv6CidrBlockAssociationSet {
		if a.Ipv6CidrBlockState == nil || common.Value(a.Ipv6CidrBlockState.State) != "associated" {
			continue
		}

		if p, err := netip.ParsePrefix(common.Value(a.Ipv6CidrBlock)); err == nil {
			return p, true
		}
	}

	return netip.Prefix{}, false
}

func (s *Service) CreateMountTarget(
	ctx context.Context,
	in *api.CreateMountTargetRequest,
) (*api.CreateMountTargetResponse, error) {
	kind := common.Value(common.OrElse(in.IpAddressType, new(ipv4Only)))
	wantV4, wantV6 := kind != ipv6Only, kind != ipv4Only
	if in.Ipv4Address != nil && !wantV4 {
		return nil, invalid("ipv4Address cannot be set when ipAddressType is %s", kind)
	}

	if in.Ipv6Address != nil && !wantV6 {
		return nil, invalid("ipv6Address cannot be set when ipAddressType is %s", kind)
	}

	want4, err := parseAddress(in.Ipv4Address, "ipv4Address", false)
	if err != nil {
		return nil, err
	}

	want6, err := parseAddress(in.Ipv6Address, "ipv6Address", true)
	if err != nil {
		return nil, err
	}

	if groupErr := checkGroups(in.SecurityGroups); groupErr != nil {
		return nil, groupErr
	}

	var out *api.GetMountTargetResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		fs, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		if fs.State == deleting {
			return conflict("File system %s is being deleted", fs.ID)
		}

		subnet, e := s.network.MountSubnet(tx, scope, common.Value(in.SubnetId))
		if e != nil {
			return ec2Error(e)
		}

		groups, e := s.network.MountGroups(tx, scope, common.Value(subnet.VpcId), in.SecurityGroups)
		if e != nil {
			return ec2Error(e)
		}

		all, e := storage.Records[mountTarget](tx, scope.Prefix(serviceName, kindMT))
		if e != nil {
			return e
		}

		for _, m := range all {
			switch {
			case m.FileSystemID != fs.ID:
			case m.VpcID != common.Value(subnet.VpcId):
				return conflict("File system %s has mount targets in VPC %s; all its mount targets must share one VPC",
					fs.ID, m.VpcID)
			case m.Zone == common.Value(subnet.AvailabilityZone):
				return conflict("File system %s already has mount target %s in Availability Zone %s",
					fs.ID, m.ID, m.Zone)
			}
		}

		r := &mountTarget{
			ID:            "fsmt-" + common.RandomID(17, "0123456789abcdef"),
			FileSystemID:  fs.ID,
			SubnetID:      common.Value(in.SubnetId),
			VpcID:         common.Value(subnet.VpcId),
			ZoneID:        common.Value(subnet.AvailabilityZoneId),
			Zone:          common.Value(subnet.AvailabilityZone),
			SecurityGroup: groups,
			State:         creating,
			Due:           time.Now().UTC().Add(transitionDelay),
		}

		if wantV4 {
			block, parseErr := netip.ParsePrefix(common.Value(subnet.CidrBlock))
			if parseErr != nil {
				return invalid("Subnet %s has no IPv4 CIDR", r.SubnetID)
			}

			ip, pickErr := mountaddr.Pick(block, want4, func(addr netip.Addr) bool {
				return slices.ContainsFunc(
					all,
					func(m *mountTarget) bool { return m.SubnetID == r.SubnetID && m.IPv4 == addr.String() },
				)
			})
			if pickErr != nil {
				return addressError(pickErr, "ipv4Address")
			}

			r.IPv4 = ip.String()
		}

		if wantV6 {
			block, ok := associatedIPv6(subnet)
			if !ok {
				return invalid("Subnet %s has no IPv6 CIDR, which ipAddressType %s needs", r.SubnetID, kind)
			}

			ip, pickErr := mountaddr.Pick(block, want6, func(addr netip.Addr) bool {
				return slices.ContainsFunc(
					all,
					func(m *mountTarget) bool { return m.SubnetID == r.SubnetID && m.IPv6 == addr.String() },
				)
			})
			if pickErr != nil {
				return addressError(pickErr, "ipv6Address")
			}

			r.IPv6 = ip.String()
		}

		out = mtResponse(scope, r)

		return queue(tx, scope, kindMT, r.ID, r)
	})
	if err != nil {
		return nil, err
	}

	return new(api.CreateMountTargetResponse(*out)), nil
}

func (s *Service) GetMountTarget(
	ctx context.Context,
	in *api.GetMountTargetRequest,
) (*api.GetMountTargetResponse, error) {
	var out *api.GetMountTargetResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readMT(tx, scope, common.Value(in.MountTargetId))
		if e != nil {
			return e
		}

		out = mtResponse(scope, r)
		return nil
	})

	return out, err
}

// ListMountTargets lists every mount target, or those of the file system, or of the access point's file system.
func (s *Service) ListMountTargets(
	ctx context.Context,
	in *api.ListMountTargetsRequest,
) (*api.ListMountTargetsResponse, error) {
	out := &api.ListMountTargetsResponse{MountTargets: []api.ListMountTargetsDescription{}}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		parent := ""
		if in.FileSystemId != nil {
			fs, e := readFS(tx, scope, *in.FileSystemId)
			if e != nil {
				return e
			}

			parent = fs.ID
		}

		if in.AccessPointId != nil {
			ap, e := readAP(tx, scope, *in.AccessPointId)
			if e != nil {
				return e
			}

			if parent != "" && parent != ap.FileSystemID {
				return invalid("Access point %s does not belong to file system %s", ap.ID, parent)
			}

			parent = ap.FileSystemID
		}

		all, e := storage.Records[mountTarget](tx, scope.Prefix(serviceName, kindMT))
		if e != nil {
			return e
		}

		if parent != "" {
			all = slices.DeleteFunc(all, func(m *mountTarget) bool { return m.FileSystemID != parent })
		}

		all, out.NextToken, e = common.Page(all, func(m *mountTarget) string { return m.ID }, in.NextToken,
			pageLimit(in.MaxResults, listPageSize), errBadToken)
		if e != nil {
			return e
		}

		for _, m := range all {
			d := api.ListMountTargetsDescription{
				AvailabilityZoneId: new(m.ZoneID),
				FileSystemId:       new(m.FileSystemID),
				Status:             new(m.State),
				MountTargetId:      new(m.ID),
				OwnerId:            new(scope.AccountID),
				SubnetId:           new(m.SubnetID),
				VpcId:              new(m.VpcID),
			}

			if m.IPv4 != "" {
				d.Ipv4Address = new(m.IPv4)
			}

			if m.IPv6 != "" {
				d.Ipv6Address = new(m.IPv6)
			}

			out.MountTargets = append(out.MountTargets, d)
		}

		return nil
	})

	return out, err
}

// UpdateMountTarget replaces the security groups; an empty list selects the VPC's default group, as on create.
func (s *Service) UpdateMountTarget(
	ctx context.Context,
	in *api.UpdateMountTargetRequest,
) (*api.UpdateMountTargetResponse, error) {
	if groupErr := checkGroups(in.SecurityGroups); groupErr != nil {
		return nil, groupErr
	}

	var out *api.GetMountTargetResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readMT(tx, scope, common.Value(in.MountTargetId))
		if e != nil {
			return e
		}

		if r.State == deleting {
			return conflict("Mount target %s is being deleted", r.ID)
		}

		groups, e := s.network.MountGroups(tx, scope, r.VpcID, in.SecurityGroups)
		if e != nil {
			return ec2Error(e)
		}

		r.SecurityGroup = groups
		out = mtResponse(scope, r)

		return storage.Write(tx, mtKey(scope, r.ID), r)
	})
	if err != nil {
		return nil, err
	}

	return new(api.UpdateMountTargetResponse(*out)), nil
}

func (s *Service) DeleteMountTarget(
	ctx context.Context,
	in *api.DeleteMountTargetRequest,
) (*api.DeleteMountTargetOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readMT(tx, scope, common.Value(in.MountTargetId))
		if e != nil {
			return e
		}

		if r.State == deleting {
			return nil
		}

		r.State = deleting
		r.Due = time.Now().UTC().Add(transitionDelay)

		return queue(tx, scope, kindMT, r.ID, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteMountTargetOutput{}, nil
}

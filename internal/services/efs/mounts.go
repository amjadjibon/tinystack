package efs

import (
	"context"
	"errors"
	"net/netip"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func groupsOK(ids []string) error {
	if len(ids) > 5 {
		return failure("SecurityGroupLimitExceeded", "At most five security groups per mount target")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !groupID.MatchString(id) || seen[id] {
			return invalid("SecurityGroups must contain unique valid IDs")
		}
		seen[id] = true
	}
	return nil
}

func networkError(err error, code string) error {
	var e *awserror.Error
	if errors.As(err, &e) {
		return failure(code, e.Message)
	}
	return err
}

func (s *Service) CreateMountTarget(
	ctx context.Context,
	in *api.CreateMountTargetRequest,
) (*api.MountTargetDescription, error) {
	if !subnetID.MatchString(common.Value(in.SubnetId)) {
		return nil, invalid("A valid SubnetId is required")
	}
	if in.IpAddressType != nil && *in.IpAddressType != "IPV4_ONLY" {
		return nil, invalid("Only IPV4_ONLY is supported by local EC2 subnets")
	}
	if err := groupsOK(in.SecurityGroups); err != nil {
		return nil, err
	}
	if in.IpAddress != nil {
		ip, e := netip.ParseAddr(*in.IpAddress)
		if e != nil || !ip.Is4() {
			return nil, invalid("IpAddress must be an IPv4 address")
		}
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	id := "fsmt-" + common.RandomID(17, "0123456789abcdef")
	var out *api.MountTargetDescription
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		parent, e := read(tx, scope, in.FileSystemId, kindFS)
		if e != nil {
			return e
		}
		if s.network == nil {
			return failure("SubnetNotFound", "No EC2 subnet lookup is configured")
		}
		subnet, e := s.network.MountSubnet(tx, scope, *in.SubnetId)
		if e != nil {
			return networkError(e, "SubnetNotFound")
		}
		groups, e := s.network.MountGroups(tx, scope, common.Value(subnet.VpcId), in.SecurityGroups)
		if e != nil {
			return networkError(e, "SecurityGroupNotFound")
		}
		all, e := rows(tx, scope, kindMT)
		if e != nil {
			return e
		}
		for _, r := range all {
			sameZone := common.Value(r.MT.AvailabilityZoneName) == common.Value(subnet.AvailabilityZone)
			otherVPC := common.Value(r.MT.VpcId) != common.Value(subnet.VpcId)
			if r.Parent == parent.ID && (sameZone || otherVPC) {
				return failure(
					"MountTargetConflict",
					"File systems support one VPC and one target per Availability Zone",
				)
			}
		}
		block, e := netip.ParsePrefix(common.Value(subnet.CidrBlock))
		if e != nil || !block.Addr().Is4() {
			return invalid("Subnet must have an IPv4 CIDR")
		}
		// This is an unreserved metadata address, never an EC2 allocation. Skip AWS's
		// reserved first four addresses and the last address; avoid other EFS records.
		ip := block.Masked().Addr().Next().Next().Next().Next()
		if in.IpAddress != nil {
			ip, _ = netip.ParseAddr(*in.IpAddress)
		}
		usable := func(addr netip.Addr) bool {
			first := block.Masked().Addr().Next().Next().Next().Next()
			return block.Contains(addr) && addr.Compare(first) >= 0 && block.Contains(addr.Next())
		}
		used := func(addr netip.Addr) bool {
			return slices.ContainsFunc(all, func(r *record) bool {
				return common.Value(r.MT.SubnetId) == *in.SubnetId && common.Value(r.MT.IpAddress) == addr.String()
			})
		}
		if in.IpAddress != nil {
			if !usable(ip) {
				return invalid("IpAddress must be a usable address in the subnet CIDR")
			}
			if used(ip) {
				return failure("IpAddressInUse", "Another EFS record uses this address")
			}
		} else {
			for usable(ip) && used(ip) {
				ip = ip.Next()
			}
			if !usable(ip) {
				return failure("NoFreeAddressesInSubnet", "No unused metadata addresses in subnet")
			}
		}
		out = &api.MountTargetDescription{
			OwnerId: new(scope.AccountID), MountTargetId: new(id), FileSystemId: new(parent.ID),
			SubnetId: in.SubnetId, LifeCycleState: new(available), IpAddress: new(ip.String()), VpcId: subnet.VpcId,
			AvailabilityZoneName: subnet.AvailabilityZone, AvailabilityZoneId: subnet.AvailabilityZoneId,
		}
		return save(tx, scope, &record{ID: id, Kind: kindMT, Parent: parent.ID, MT: out, Groups: groups})
	})
	return out, err
}

func (s *Service) DescribeMountTargets(
	ctx context.Context,
	in *api.DescribeMountTargetsRequest,
) (*api.DescribeMountTargetsResponse, error) {
	filters := 0
	for _, p := range []*string{in.FileSystemId, in.MountTargetId, in.AccessPointId} {
		if p != nil {
			filters++
		}
	}
	if filters != 1 {
		return nil, invalid("Specify exactly one FileSystemId, MountTargetId or AccessPointId")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.DescribeMountTargetsResponse{Marker: in.Marker, MountTargets: []api.MountTargetDescription{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindMT)
		if e != nil {
			return e
		}
		if in.MountTargetId != nil {
			r, readErr := read(tx, scope, in.MountTargetId, kindMT)
			if readErr != nil {
				return readErr
			}
			all = []*record{r}
		} else {
			value, kind := in.FileSystemId, kindFS
			if in.AccessPointId != nil {
				value, kind = in.AccessPointId, kindAP
			}
			r, readErr := read(tx, scope, value, kind)
			if readErr != nil {
				return readErr
			}
			parent := r.ID
			if kind == kindAP {
				parent = r.Parent
			}
			all = slices.DeleteFunc(all, func(r *record) bool { return r.Parent != parent })
		}
		all, out.NextMarker, e = page(
			all,
			func(r *record) string { return r.ID },
			in.Marker,
			in.MaxItems,
			scope,
			"mounts/"+common.Value(
				in.FileSystemId,
			)+"/"+common.Value(
				in.MountTargetId,
			)+"/"+common.Value(
				in.AccessPointId,
			),
		)
		if e != nil {
			return e
		}
		for _, r := range all {
			out.MountTargets = append(out.MountTargets, *common.Clone(r.MT))
		}
		return nil
	})
	return out, err
}

func (s *Service) DescribeMountTargetSecurityGroups(
	ctx context.Context,
	in *api.DescribeMountTargetSecurityGroupsRequest,
) (*api.DescribeMountTargetSecurityGroupsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.DescribeMountTargetSecurityGroupsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, readErr := read(tx, scope, in.MountTargetId, kindMT)
		if readErr != nil {
			return readErr
		}
		out.SecurityGroups = slices.Clone(r.Groups)
		return nil
	})
	return out, err
}

func (s *Service) ModifyMountTargetSecurityGroups(
	ctx context.Context,
	in *api.ModifyMountTargetSecurityGroupsRequest,
) (*api.ModifyMountTargetSecurityGroupsOutput, error) {
	if err := groupsOK(in.SecurityGroups); err != nil {
		return nil, err
	}
	// Modifying replaces the entire list; an empty list removes all groups.
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, in.MountTargetId, kindMT)
		if readErr != nil {
			return readErr
		}
		groups := []string{}
		var e error
		if len(in.SecurityGroups) > 0 {
			groups, e = s.network.MountGroups(tx, scope, common.Value(r.MT.VpcId), in.SecurityGroups)
		}
		if e != nil {
			return networkError(e, "SecurityGroupNotFound")
		}
		r.Groups = groups
		return save(tx, scope, r)
	})
	return &api.ModifyMountTargetSecurityGroupsOutput{}, err
}

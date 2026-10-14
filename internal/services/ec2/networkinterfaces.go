package ec2

import (
	"cmp"
	"context"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindNetworkInterface = "network-interface"

type networkInterface struct {
	Tags              map[string]string `json:",omitempty"`
	ID                string
	VPCID             string
	SubnetID          string
	Zone              string
	Description       string
	Kind              string
	ClientToken       string
	Attachment        *api.NetworkInterfaceAttachment
	PrivateIPs        []string
	IPv6              []string
	GroupIDs          []string
	NoSourceDestCheck bool
}

func eniNotFound(id string) error {
	return notFound("InvalidNetworkInterfaceID.NotFound", "network interface", id)
}

func loadInterface(tx storage.ReadTx, scope request.Scope, id string) (*networkInterface, error) {
	ni, ok, err := storage.Read[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, id))
	if err == nil && !ok {
		err = eniNotFound(id)
	}

	return ni, err
}

func eniValues(scope request.Scope, ni *networkInterface) values {
	state, instanceID, attachmentID := stateAvailable, "", ""
	if ni.Attachment != nil {
		state, instanceID, attachmentID = ec2InUse, common.Value(
			ni.Attachment.InstanceId,
		), common.Value(
			ni.Attachment.AttachmentId,
		)
	}

	return values{
		ec2NetworkInterfaceId: {ni.ID}, filterVPCID: {ni.VPCID}, filterSubnetID: {ni.SubnetID},
		filterGroupID: ni.GroupIDs, "status": {state}, filterDescription: {ni.Description}, filterZone: {ni.Zone},
		"attachment.instance-id": {instanceID}, "attachment.attachment-id": {attachmentID},
		filterOwnerID: {scope.AccountID}, filterPrivateIP: ni.PrivateIPs, "addresses.private-ip-address": ni.PrivateIPs,
		"interface-type": {ni.Kind}, "requester-managed": {"false"},
	}.withTags(ni.Tags)
}

func eniOutput(tx storage.ReadTx, scope request.Scope, ni *networkInterface) (api.NetworkInterface, error) {
	status := stateAvailable
	if ni.Attachment != nil {
		status = ec2InUse
	}

	primary := ""
	if len(ni.PrivateIPs) > 0 {
		primary = ni.PrivateIPs[0]
	}

	out := api.NetworkInterface{
		NetworkInterfaceId: new(ni.ID),
		VpcId:              new(ni.VPCID),
		SubnetId:           new(ni.SubnetID),
		AvailabilityZone: new(
			ni.Zone,
		),
		AvailabilityZoneId: new(zoneID(scope.Region, strings.TrimPrefix(ni.Zone, scope.Region))),
		OwnerId:            new(scope.AccountID),
		Description:        new(ni.Description),
		InterfaceType:      new(ni.Kind),
		MacAddress:         new(macAddress(ni.ID)),
		PrivateIpAddress:   optional(primary),
		PrivateDnsName:     new(dnsName(scope.Region, primary, false)),
		Status:             new(status),
		SourceDestCheck: new(
			!ni.NoSourceDestCheck,
		),
		RequesterManaged: new(false),
		Attachment:       common.Clone(ni.Attachment),
		TagSet: tagList(
			ni.Tags,
		),
		Groups:             []api.GroupIdentifier{},
		PrivateIpAddresses: []api.NetworkInterfacePrivateIpAddress{},
		Ipv6Addresses:      []api.NetworkInterfaceIpv6Address{},
	}

	for i, ip := range ni.PrivateIPs {
		out.PrivateIpAddresses = append(out.PrivateIpAddresses, api.NetworkInterfacePrivateIpAddress{
			PrivateIpAddress: new(ip), Primary: new(i == 0), PrivateDnsName: new(dnsName(scope.Region, ip, false)),
		})
	}

	for _, ip := range ni.IPv6 {
		out.Ipv6Addresses = append(out.Ipv6Addresses, api.NetworkInterfaceIpv6Address{Ipv6Address: new(ip)})
	}

	for _, id := range ni.GroupIDs {
		g, err := findGroup(tx, scope, id, "")
		if err != nil {
			return out, err
		}

		out.Groups = append(out.Groups, api.GroupIdentifier{GroupId: new(g.ID), GroupName: new(g.Name)})
	}

	return out, nil
}

func interfaceGroups(tx storage.ReadTx, scope request.Scope, vpcID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		all, err := storage.Records[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, ""))
		if err != nil {
			return nil, err
		}

		for _, g := range all {
			if g.VPCID == vpcID && g.Name == defaultGroupName {
				return []string{g.ID}, nil
			}
		}

		return nil, groupNotFound(defaultGroupName)
	}

	for _, id := range ids {
		g, err := findGroup(tx, scope, id, "")
		if err != nil {
			return nil, err
		}

		if g.VPCID != vpcID {
			return nil, invalidParameter("Security group belongs to another VPC")
		}
	}

	return slices.Clone(ids), nil
}

func allocateIPv6(tx storage.ReadTx, scope request.Scope, sn *subnet, wants []api.InstanceIpv6Address,
	count *int64,
) ([]string, error) {
	if len(wants) > 0 && count != nil {
		return nil, invalidParameter("Specify IPv6 addresses or count, not both")
	}

	n := int64(len(wants))
	if count != nil {
		n = *count
	} else if n == 0 && sn.AssignIPv6 {
		n = 1
	}

	if n < 0 {
		return nil, invalidParameter("IPv6 address count cannot be negative")
	}

	if n == 0 {
		return nil, nil
	}

	at := slices.IndexFunc(sn.IPv6CIDRs, func(a cidrAssociation) bool { return a.State == stateAssociated })
	if at < 0 {
		return nil, invalidParameter("Subnet has no IPv6 CIDR")
	}

	p := netip.MustParsePrefix(sn.IPv6CIDRs[at].CIDR)
	all, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
	if err != nil {
		return nil, err
	}

	used := map[netip.Addr]bool{}
	for _, ni := range all {
		for _, text := range ni.IPv6 {
			used[netip.MustParseAddr(text)] = true
		}
	}

	// AWS reserves the first four IPv6 addresses, and the last.
	next := p.Addr().Next().Next().Next().Next()
	lastBytes := p.Addr().As16()
	for i := 8; i < len(lastBytes); i++ {
		lastBytes[i] = 0xff
	}
	last := netip.AddrFrom16(lastBytes)
	var out []string
	for i := range n {
		ip := next
		if len(wants) > 0 {
			ip, err = netip.ParseAddr(common.Value(wants[i].Ipv6Address))
			if err != nil || !p.Contains(ip) || ip.Less(next) || ip == last {
				return nil, invalidParameter("IPv6 address outside subnet")
			}

			if used[ip] {
				return nil, awserror.New("InvalidIPAddress.InUse", "IPv6 address is in use")
			}
		} else {
			for used[ip] {
				ip = ip.Next()
			}
		}

		if !p.Contains(ip) {
			return nil, awserror.New("InsufficientFreeAddressesInSubnet", "No free IPv6 addresses")
		}

		used[ip] = true
		out = append(out, ip.String())
	}

	return out, nil
}

func (s *Service) CreateNetworkInterface(ctx context.Context, in *api.CreateNetworkInterfaceRequest) (
	*api.CreateNetworkInterfaceResult, error,
) {
	tags, err := specTags(in.TagSpecifications, kindNetworkInterface)
	if err != nil {
		return nil, err
	}

	kind := cmp.Or(common.Value(in.InterfaceType), "interface")
	if !slices.Contains([]string{"interface", "efa", ec2EfaOnly, "trunk"}, kind) {
		return nil, invalidParameter("Invalid network interface type")
	}

	if in.SecondaryPrivateIpAddressCount != nil &&
		(len(in.PrivateIpAddresses) > 0 || *in.SecondaryPrivateIpAddressCount < 0) {
		return nil, invalidParameter("Private IP addresses and count cannot be combined; count must be nonnegative")
	}

	var out api.NetworkInterface
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, recordsErr := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
		if recordsErr != nil {
			return recordsErr
		}

		for _, ni := range all {
			if in.ClientToken != nil && ni.ClientToken == *in.ClientToken {
				if ni.SubnetID != common.Value(in.SubnetId) {
					return awserror.New("IdempotentParameterMismatch", "Subnet differs")
				}

				out, recordsErr = eniOutput(tx, scope, ni)
				return recordsErr
			}
		}

		sn, ok, recordsErr := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, common.Value(in.SubnetId)))
		if recordsErr != nil || !ok {
			return common.ErrorsOr(recordsErr, subnetNotFound(common.Value(in.SubnetId)))
		}

		groups, recordsErr := interfaceGroups(tx, scope, sn.VPCID, in.Groups)
		if recordsErr != nil {
			return recordsErr
		}

		ni := &networkInterface{
			ID:          newID("eni"),
			VPCID:       sn.VPCID,
			SubnetID:    sn.ID,
			Zone:        sn.Zone,
			Tags:        tags,
			Kind:        kind,
			Description: common.Value(in.Description),
			GroupIDs:    groups,
			ClientToken: common.Value(in.ClientToken),
		}

		instances, recordsErr := storage.Records[instance](tx, scope.Key("ec2", kindInstance, ""))
		if recordsErr != nil {
			return recordsErr
		}

		ips, recordsErr := subnetAddresses(tx, scope, sn, instances)
		if recordsErr != nil {
			return recordsErr
		}

		primary := common.Value(in.PrivateIpAddress)
		var secondary []string
		for _, a := range in.PrivateIpAddresses {
			if common.Value(a.Primary) {
				if primary != "" {
					return invalidParameter("Only one primary IP address is allowed")
				}

				primary = common.Value(a.PrivateIpAddress)
			} else {
				secondary = append(secondary, common.Value(a.PrivateIpAddress))
			}
		}

		if kind == ec2EfaOnly {
			if primary != "" || len(in.PrivateIpAddresses) > 0 || common.Value(in.SecondaryPrivateIpAddressCount) > 0 ||
				len(in.Ipv6Addresses) > 0 || common.Value(in.Ipv6AddressCount) > 0 {
				return invalidParameter("EFA-only interfaces cannot have IP addresses")
			}
		} else {
			ip, takeErr := ips.take(primary)
			if takeErr != nil {
				return takeErr
			}

			ni.PrivateIPs = append(ni.PrivateIPs, ip)
			for _, want := range secondary {
				if want == "" {
					return invalidParameter("PrivateIpAddress cannot be empty")
				}

				ip, takeErr2 := ips.take(want)
				if takeErr2 != nil {
					return takeErr2
				}

				ni.PrivateIPs = append(ni.PrivateIPs, ip)
			}

			for range common.Value(in.SecondaryPrivateIpAddressCount) {
				ip, takeErr3 := ips.take("")
				if takeErr3 != nil {
					return takeErr3
				}

				ni.PrivateIPs = append(ni.PrivateIPs, ip)
			}

			ni.IPv6, takeErr = allocateIPv6(tx, scope, sn, in.Ipv6Addresses, in.Ipv6AddressCount)
			if takeErr != nil {
				return takeErr
			}
		}

		out, recordsErr = eniOutput(tx, scope, ni)
		if recordsErr != nil {
			return recordsErr
		}

		return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateNetworkInterfaceResult{NetworkInterface: &out}, nil
}

func (s *Service) DeleteNetworkInterface(ctx context.Context, in *api.DeleteNetworkInterfaceRequest) (
	*api.DeleteNetworkInterfaceOutput, error,
) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		if ni.Attachment != nil {
			return awserror.New("InvalidParameterValue", "Network interface is attached")
		}

		logs, err := storage.Records[flowLog](tx, scope.Key("ec2", kindFlowLog, ""))
		if err != nil {
			return err
		}

		if slices.ContainsFunc(logs, func(f *flowLog) bool { return f.ResourceID == ni.ID }) {
			return awserror.New("DependencyViolation", "Network interface has flow logs")
		}

		tx.Delete(scope.Key("ec2", kindNetworkInterface, ni.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteNetworkInterfaceOutput{}, nil
}

func (s *Service) AttachNetworkInterface(ctx context.Context, in *api.AttachNetworkInterfaceRequest) (
	*api.AttachNetworkInterfaceResult, error,
) {
	id := newID("eni-attach")
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		i, ok, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, common.Value(in.InstanceId)))
		if err != nil || !ok {
			return common.ErrorsOr(err, instanceNotFound(common.Value(in.InstanceId)))
		}

		if !i.active() || ni.Attachment != nil {
			return awserror.New("IncorrectState", "Interface or instance is unavailable")
		}

		if ni.Zone != i.Zone || ni.VPCID != i.VPCID {
			return invalidParameter("Interface and instance must be in the same AZ/VPC")
		}

		index := common.Value(in.DeviceIndex)
		if index < 0 {
			return invalidParameter("DeviceIndex must be nonnegative")
		}

		all, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
		if err != nil {
			return err
		}

		for _, other := range all {
			if other.Attachment != nil && common.Value(other.Attachment.InstanceId) == i.ID &&
				common.Value(other.Attachment.DeviceIndex) == index {
				return invalidParameter("Device index is already occupied")
			}
		}

		ni.Attachment = &api.NetworkInterfaceAttachment{
			AttachmentId: new(id), InstanceId: new(i.ID),
			InstanceOwnerId: new(scope.AccountID), DeviceIndex: new(index), DeleteOnTermination: new(false),
			AttachTime: new(s.now().UTC()), Status: new("attached"),
		}

		return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
	})
	if err != nil {
		return nil, err
	}

	return &api.AttachNetworkInterfaceResult{AttachmentId: new(id)}, nil
}

func (s *Service) DetachNetworkInterface(ctx context.Context, in *api.DetachNetworkInterfaceRequest) (
	*api.DetachNetworkInterfaceOutput, error,
) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
		if err != nil {
			return err
		}

		for _, ni := range all {
			if ni.Attachment == nil || common.Value(ni.Attachment.AttachmentId) != common.Value(in.AttachmentId) {
				continue
			}

			if common.Value(ni.Attachment.DeviceIndex) == 0 {
				return awserror.New("OperationNotPermitted", "Cannot detach primary interface")
			}

			ni.Attachment = nil
			return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
		}

		return notFound("InvalidAttachmentID.NotFound", "attachment", common.Value(in.AttachmentId))
	})
	if err != nil {
		return nil, err
	}

	return &api.DetachNetworkInterfaceOutput{}, nil
}

func (s *Service) DescribeNetworkInterfaceAttribute(
	ctx context.Context,
	in *api.DescribeNetworkInterfaceAttributeRequest,
) (
	*api.DescribeNetworkInterfaceAttributeResult, error,
) {
	out := &api.DescribeNetworkInterfaceAttributeResult{NetworkInterfaceId: in.NetworkInterfaceId}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		switch common.Value(in.Attribute) {
		case "description":
			out.Description = &api.AttributeValue{Value: new(ni.Description)}
		case "sourceDestCheck":
			out.SourceDestCheck = attribute(!ni.NoSourceDestCheck)
		case "attachment":
			out.Attachment = common.Clone(ni.Attachment)
		case "groupSet":
			item, err := eniOutput(tx, scope, ni)
			if err != nil {
				return err
			}

			out.Groups = item.Groups
		default:
			return invalidParameter("Unsupported network interface attribute")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ModifyNetworkInterfaceAttribute(ctx context.Context, in *api.ModifyNetworkInterfaceAttributeRequest) (
	*api.ModifyNetworkInterfaceAttributeOutput, error,
) {
	if err := onlyOne(
		in.Description != nil,
		in.SourceDestCheck != nil,
		in.Groups != nil,
		in.Attachment != nil,
	); err != nil {
		return nil, err
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		switch {
		case in.Description != nil:
			ni.Description = common.Value(in.Description.Value)
		case in.SourceDestCheck != nil:
			ni.NoSourceDestCheck = !common.Value(in.SourceDestCheck.Value)
		case in.Groups != nil:
			if len(in.Groups) == 0 {
				return invalidParameter("At least one security group is required")
			}

			ni.GroupIDs, err = interfaceGroups(tx, scope, ni.VPCID, in.Groups)
			if err != nil {
				return err
			}
			if ni.Attachment != nil && common.Value(ni.Attachment.DeviceIndex) == 0 {
				instanceID := common.Value(ni.Attachment.InstanceId)
				owner, found, readErr := storage.Read[instance](tx, scope.Key("ec2", kindInstance, instanceID))
				if readErr != nil || !found {
					return common.ErrorsOr(readErr, instanceNotFound(instanceID))
				}
				owner.Groups = nil
				for _, groupID := range ni.GroupIDs {
					group, groupErr := findGroup(tx, scope, groupID, "")
					if groupErr != nil {
						return groupErr
					}
					owner.Groups = append(owner.Groups, groupRef{ID: group.ID, Name: group.Name})
				}
				if writeErr := storage.Write(tx, scope.Key("ec2", kindInstance, instanceID), owner); writeErr != nil {
					return writeErr
				}
			}

		case in.Attachment != nil:
			if ni.Attachment == nil ||
				common.Value(in.Attachment.AttachmentId) != common.Value(ni.Attachment.AttachmentId) {
				return notFound("InvalidAttachmentID.NotFound", "attachment", common.Value(in.Attachment.AttachmentId))
			}

			if in.Attachment.DeleteOnTermination == nil {
				return invalidParameter("DeleteOnTermination is required")
			}

			ni.Attachment.DeleteOnTermination = common.Clone(in.Attachment.DeleteOnTermination)
		}

		return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyNetworkInterfaceAttributeOutput{}, nil
}

func (s *Service) ResetNetworkInterfaceAttribute(ctx context.Context, in *api.ResetNetworkInterfaceAttributeRequest) (
	*api.ResetNetworkInterfaceAttributeOutput, error,
) {
	if common.Value(in.SourceDestCheck) != "true" {
		return nil, invalidParameter("SourceDestCheck must be true to reset it")
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		ni.NoSourceDestCheck = false
		return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
	})
	if err != nil {
		return nil, err
	}

	return &api.ResetNetworkInterfaceAttributeOutput{}, nil
}

func (s *Service) AssignPrivateIpAddresses(ctx context.Context, in *api.AssignPrivateIpAddressesRequest) (
	*api.AssignPrivateIpAddressesResult, error,
) {
	if err := onlyOne(len(in.PrivateIpAddresses) > 0, in.SecondaryPrivateIpAddressCount != nil); err != nil {
		return nil, err
	}

	if in.SecondaryPrivateIpAddressCount != nil && *in.SecondaryPrivateIpAddressCount < 1 {
		return nil, invalidParameter("SecondaryPrivateIpAddressCount must be positive")
	}

	out := &api.AssignPrivateIpAddressesResult{NetworkInterfaceId: in.NetworkInterfaceId}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		if ni.Kind == ec2EfaOnly {
			return invalidParameter("EFA-only interfaces cannot have IP addresses")
		}

		sn, _, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, ni.SubnetID))
		if err != nil {
			return err
		}

		instances, err := storage.Records[instance](tx, scope.Key("ec2", kindInstance, ""))
		if err != nil {
			return err
		}

		ips, err := subnetAddresses(tx, scope, sn, instances)
		if err != nil {
			return err
		}

		all, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
		if err != nil {
			return err
		}

		wants := slices.Clone(in.PrivateIpAddresses)
		for range common.Value(in.SecondaryPrivateIpAddressCount) {
			wants = append(wants, "")
		}

		changed := map[string]*networkInterface{}
		for _, want := range wants {
			if want != "" && slices.Contains(ni.PrivateIPs, want) {
				continue
			}

			if want != "" && common.Value(in.AllowReassignment) {
				for _, other := range all {
					at := slices.Index(other.PrivateIPs, want)
					if at < 0 {
						continue
					}

					if at == 0 || other.SubnetID != sn.ID {
						return invalidParameter("Cannot reassign a primary or foreign-subnet IP")
					}

					other.PrivateIPs = slices.Delete(other.PrivateIPs, at, at+1)
					changed[other.ID] = other
					delete(ips.used, netip.MustParseAddr(want))
				}
			}

			ip, err := ips.take(want)
			if err != nil {
				return err
			}

			ni.PrivateIPs = append(ni.PrivateIPs, ip)
			out.AssignedPrivateIpAddresses = append(
				out.AssignedPrivateIpAddresses,
				api.AssignedPrivateIpAddress{PrivateIpAddress: new(ip)},
			)
		}

		for _, other := range changed {
			if err := storage.Write(tx, scope.Key("ec2", kindNetworkInterface, other.ID), other); err != nil {
				return err
			}
		}

		return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UnassignPrivateIpAddresses(ctx context.Context, in *api.UnassignPrivateIpAddressesRequest) (
	*api.UnassignPrivateIpAddressesOutput, error,
) {
	if len(in.PrivateIpAddresses) == 0 {
		return nil, invalidParameter("PrivateIpAddresses is required")
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		ni, err := loadInterface(tx, scope, common.Value(in.NetworkInterfaceId))
		if err != nil {
			return err
		}

		for _, ip := range in.PrivateIpAddresses {
			at := slices.Index(ni.PrivateIPs, ip)
			if at <= 0 {
				return invalidParameter("Cannot unassign primary or unassigned private IP")
			}

			ni.PrivateIPs = slices.Delete(ni.PrivateIPs, at, at+1)
		}

		return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
	})
	if err != nil {
		return nil, err
	}

	return &api.UnassignPrivateIpAddressesOutput{}, nil
}

func createInstanceInterface(tx storage.WriteTx, scope request.Scope, i *instance, sn *subnet, now time.Time) error {
	ni := &networkInterface{
		ID: newID("eni"), SubnetID: sn.ID, VPCID: sn.VPCID, Zone: sn.Zone,
		Kind: "interface", PrivateIPs: []string{i.PrivateIP}, Description: "Primary network interface",
		Attachment: &api.NetworkInterfaceAttachment{
			AttachmentId: new(newID("eni-attach")), InstanceId: new(i.ID),
			InstanceOwnerId: new(scope.AccountID), DeviceIndex: new(int64(0)), DeleteOnTermination: new(true),
			AttachTime: new(now), Status: new("attached"),
		},
	}

	for _, g := range i.Groups {
		ni.GroupIDs = append(ni.GroupIDs, g.ID)
	}

	var err error
	ni.IPv6, err = allocateIPv6(tx, scope, sn, nil, nil)
	if err != nil {
		return err
	}

	return storage.Write(tx, scope.Key("ec2", kindNetworkInterface, ni.ID), ni)
}

func releaseInstanceInterfaces(tx storage.WriteTx, scope request.Scope, instanceID string) error {
	all, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
	if err != nil {
		return err
	}

	for _, ni := range all {
		if ni.Attachment == nil || common.Value(ni.Attachment.InstanceId) != instanceID {
			continue
		}

		key := scope.Key("ec2", kindNetworkInterface, ni.ID)
		if common.Value(ni.Attachment.DeleteOnTermination) {
			tx.Delete(key)
			continue
		}

		ni.Attachment = nil
		if err := storage.Write(tx, key, ni); err != nil {
			return err
		}
	}

	return nil
}

func fillInstanceInterfaces(tx storage.ReadTx, scope request.Scope, out *api.Instance) error {
	all, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
	if err != nil {
		return err
	}

	for _, ni := range all {
		if ni.Attachment == nil || common.Value(ni.Attachment.InstanceId) != common.Value(out.InstanceId) {
			continue
		}

		item, err := eniOutput(tx, scope, ni)
		if err != nil {
			return err
		}

		attached := ni.Attachment
		entry := api.InstanceNetworkInterface{
			NetworkInterfaceId: new(ni.ID), SubnetId: new(ni.SubnetID),
			VpcId: new(ni.VPCID), Description: new(ni.Description), MacAddress: item.MacAddress,
			OwnerId: new(scope.AccountID), Status: item.Status, SourceDestCheck: item.SourceDestCheck,
			Groups: item.Groups, PrivateIpAddress: item.PrivateIpAddress, PrivateDnsName: item.PrivateDnsName,
			InterfaceType: new(ni.Kind), Attachment: &api.InstanceNetworkInterfaceAttachment{
				AttachmentId:        attached.AttachmentId,
				DeviceIndex:         attached.DeviceIndex,
				AttachTime:          attached.AttachTime,
				DeleteOnTermination: attached.DeleteOnTermination,
				Status:              attached.Status,
			},
		}

		for _, ip := range ni.IPv6 {
			entry.Ipv6Addresses = append(entry.Ipv6Addresses, api.InstanceIpv6Address{Ipv6Address: new(ip)})
		}

		for i, ip := range ni.PrivateIPs {
			entry.PrivateIpAddresses = append(entry.PrivateIpAddresses, api.InstancePrivateIpAddress{
				PrivateIpAddress: new(ip), PrivateDnsName: new(dnsName(scope.Region, ip, false)), Primary: new(i == 0),
			})
		}
		if common.Value(ni.Attachment.DeviceIndex) == 0 {
			out.SecurityGroups, out.SourceDestCheck = item.Groups, item.SourceDestCheck
		}
		out.NetworkInterfaces = append(out.NetworkInterfaces, entry)
	}

	return nil
}

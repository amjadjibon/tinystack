package ec2

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindNetworkACL = "network-acl"
	actionAllow    = "allow"
	actionDeny     = "deny"
	// lastRule is the number of the deny-all entry every ACL ends with, which
	// cannot be changed.
	lastRule = 32767
)

// networkACL is a VPC's network ACL. Each subnet is associated with exactly
// one ACL of its VPC, the VPC's default one unless it was replaced.
type networkACL struct {
	Tags         map[string]string `json:",omitempty"`
	ID           string
	VPCID        string
	ClientToken  string           `json:",omitempty"`
	Associations []aclAssociation `json:",omitempty"`
	Entries      []aclEntry
	Default      bool
}

type aclAssociation struct {
	ID       string
	SubnetID string
}

// aclEntry is one numbered rule of an ACL. Protocol is a protocol number or
// -1; TCP and UDP rules have ports, and ICMP rules a type and code.
type aclEntry struct {
	From     *int64 `json:",omitempty"`
	To       *int64 `json:",omitempty"`
	ICMPType *int64 `json:",omitempty"`
	ICMPCode *int64 `json:",omitempty"`
	CIDR     string
	Protocol string
	Action   string
	Number   int64
	Egress   bool
}

func aclNotFound(id string) error { return notFound("InvalidNetworkAclID.NotFound", "networkAcl", id) }

func loadACL(tx storage.ReadTx, scope request.Scope, id string) (*networkACL, error) {
	acl, ok, err := storage.Read[networkACL](tx, scope.Key("ec2", kindNetworkACL, id))
	if err == nil && !ok {
		err = aclNotFound(id)
	}

	return acl, err
}

// newACL returns an ACL that denies all traffic, or for a VPC's default ACL
// first allows it.
func newACL(vpcID string, isDefault bool) *networkACL {
	acl := &networkACL{ID: newID("acl"), VPCID: vpcID, Default: isDefault}
	for _, egress := range []bool{false, true} {
		if isDefault {
			acl.Entries = append(acl.Entries, aclEntry{
				Number: 100, Protocol: allProtocols, Action: actionAllow, CIDR: anywhere, Egress: egress,
			})
		}

		acl.Entries = append(acl.Entries, aclEntry{
			Number: lastRule, Protocol: allProtocols, Action: actionDeny, CIDR: anywhere, Egress: egress,
		})
	}

	return acl
}

// vpcACLs returns a VPC's network ACLs, ordered by ID.
func vpcACLs(tx storage.ReadTx, scope request.Scope, vpcID string) ([]*networkACL, error) {
	all, err := list[networkACL](tx, scope, kindNetworkACL)
	return slices.DeleteFunc(all, func(acl *networkACL) bool { return acl.VPCID != vpcID }), err
}

// createDefaultACL stores a VPC's default network ACL, associated with
// subnets.
func createDefaultACL(tx storage.WriteTx, scope request.Scope, vpcID string, subnets ...string) error {
	acl := newACL(vpcID, true)
	for _, id := range subnets {
		acl.Associations = append(acl.Associations, aclAssociation{ID: newID("aclassoc"), SubnetID: id})
	}

	return storage.Write(tx, scope.Key("ec2", kindNetworkACL, acl.ID), acl)
}

// associateDefaultACL associates a new subnet with its VPC's default ACL.
func associateDefaultACL(tx storage.WriteTx, scope request.Scope, sn *subnet) error {
	acls, err := vpcACLs(tx, scope, sn.VPCID)
	if err != nil {
		return err
	}

	for _, acl := range acls {
		if acl.Default {
			acl.Associations = append(acl.Associations, aclAssociation{ID: newID("aclassoc"), SubnetID: sn.ID})
			return storage.Write(tx, scope.Key("ec2", kindNetworkACL, acl.ID), acl)
		}
	}

	return nil
}

// dissociateACL removes a deleted subnet's ACL association.
func dissociateACL(tx storage.WriteTx, scope request.Scope, sn *subnet) error {
	acls, err := vpcACLs(tx, scope, sn.VPCID)
	if err != nil {
		return err
	}

	for _, acl := range acls {
		at := slices.IndexFunc(acl.Associations, func(a aclAssociation) bool { return a.SubnetID == sn.ID })
		if at >= 0 {
			acl.Associations = slices.Delete(acl.Associations, at, at+1)
			return storage.Write(tx, scope.Key("ec2", kindNetworkACL, acl.ID), acl)
		}
	}

	return nil
}

func aclOutput(scope request.Scope, acl *networkACL) api.NetworkAcl {
	out := api.NetworkAcl{
		NetworkAclId: new(acl.ID), VpcId: new(acl.VPCID), OwnerId: new(scope.AccountID), IsDefault: new(acl.Default),
		Tags: tagList(acl.Tags), Associations: []api.NetworkAclAssociation{}, Entries: []api.NetworkAclEntry{},
	}
	for _, a := range acl.Associations {
		out.Associations = append(out.Associations, api.NetworkAclAssociation{
			NetworkAclAssociationId: new(a.ID), NetworkAclId: new(acl.ID), SubnetId: new(a.SubnetID),
		})
	}

	for _, e := range acl.Entries {
		item := api.NetworkAclEntry{
			CidrBlock: new(e.CIDR), Egress: new(e.Egress), Protocol: new(e.Protocol), RuleAction: new(e.Action),
			RuleNumber: new(e.Number),
		}
		if e.From != nil {
			item.PortRange = &api.PortRange{From: e.From, To: e.To}
		}

		if e.ICMPType != nil {
			item.IcmpTypeCode = &api.IcmpTypeCode{Type: e.ICMPType, Code: e.ICMPCode}
		}

		out.Entries = append(out.Entries, item)
	}

	return out
}

func aclValues(scope request.Scope, acl *networkACL) values {
	v := values{
		"network-acl-id": {acl.ID}, filterVPCID: {acl.VPCID}, filterOwnerID: {scope.AccountID},
		"default": boolValue(acl.Default), "association.association-id": {}, "association.network-acl-id": {},
		"association.subnet-id": {}, "entry.cidr": {}, "entry.egress": {}, "entry.protocol": {},
		"entry.rule-action": {}, "entry.rule-number": {}, "entry.port-range.from": {}, "entry.port-range.to": {},
		"entry.icmp.type": {}, "entry.icmp.code": {},
	}
	for _, a := range acl.Associations {
		v["association.association-id"] = append(v["association.association-id"], a.ID)
		v["association.network-acl-id"] = append(v["association.network-acl-id"], acl.ID)
		v["association.subnet-id"] = append(v["association.subnet-id"], a.SubnetID)
	}

	add := func(name string, n *int64) {
		if n != nil {
			v[name] = append(v[name], strconv.FormatInt(*n, 10))
		}
	}
	for _, e := range acl.Entries {
		v["entry.cidr"] = append(v["entry.cidr"], e.CIDR)
		v["entry.egress"] = append(v["entry.egress"], strconv.FormatBool(e.Egress))
		v["entry.protocol"] = append(v["entry.protocol"], e.Protocol)
		v["entry.rule-action"] = append(v["entry.rule-action"], e.Action)
		add("entry.rule-number", &e.Number)
		add("entry.port-range.from", e.From)
		add("entry.port-range.to", e.To)
		add("entry.icmp.type", e.ICMPType)
		add("entry.icmp.code", e.ICMPCode)
	}

	return v.withTags(acl.Tags)
}

func (s *Service) CreateNetworkAcl(
	ctx context.Context,
	in *api.CreateNetworkAclRequest,
) (*api.CreateNetworkAclResult, error) {
	tags, err := specTags(in.TagSpecifications, kindNetworkACL)
	if err != nil {
		return nil, err
	}

	var out api.NetworkAcl
	token := common.Value(in.ClientToken)
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, loadErr := loadVPC(tx, scope, common.Value(in.VpcId))
		if loadErr != nil {
			return loadErr
		}

		all, listErr := list[networkACL](tx, scope, kindNetworkACL)
		if listErr != nil {
			return listErr
		}

		// A ClientToken already used returns the ACL it created.
		at := slices.IndexFunc(all, func(acl *networkACL) bool { return token != "" && acl.ClientToken == token })
		if at >= 0 {
			out = aclOutput(scope, all[at])
			return nil
		}

		acl := newACL(v.ID, false)
		acl.Tags, acl.ClientToken = tags, token
		out = aclOutput(scope, acl)
		return storage.Write(tx, scope.Key("ec2", kindNetworkACL, acl.ID), acl)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateNetworkAclResult{NetworkAcl: &out, ClientToken: in.ClientToken}, nil
}

// DeleteNetworkAcl deletes an ACL that is not its VPC's default and has no
// subnets.
func (s *Service) DeleteNetworkAcl(
	ctx context.Context,
	in *api.DeleteNetworkAclRequest,
) (*api.DeleteNetworkAclOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		acl, err := loadACL(tx, scope, common.Value(in.NetworkAclId))
		if err != nil {
			return err
		}

		switch {
		case acl.Default:
			return invalidParameter("cannot delete default network ACL " + acl.ID)
		case len(acl.Associations) > 0:
			return awserror.New("DependencyViolation", fmt.Sprintf(dependencyFormat, "networkAcl", acl.ID))
		}

		tx.Delete(scope.Key("ec2", kindNetworkACL, acl.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteNetworkAclOutput{}, nil
}

func (s *Service) DescribeNetworkAcls(
	ctx context.Context,
	in *api.DescribeNetworkAclsRequest,
) (*api.DescribeNetworkAclsResult, error) {
	match, err := compileFilters(in.Filters, aclValues(request.Scope{}, &networkACL{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeNetworkAclsResult{NetworkAcls: []api.NetworkAcl{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[networkACL](tx, scope, kindNetworkACL)
		if listErr != nil {
			return listErr
		}

		id := func(acl *networkACL) string { return acl.ID }
		selected, selectErr := selectIDs(all, id, in.NetworkAclIds, aclNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(acl *networkACL) bool {
			return !match.match(aclValues(scope, acl))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, acl := range selected {
			out.NetworkAcls = append(out.NetworkAcls, aclOutput(scope, acl))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// aclRule is what CreateNetworkAclEntry and ReplaceNetworkAclEntry ask for.
type aclRule struct {
	ports    *api.PortRange
	icmp     *api.IcmpTypeCode
	cidr     string
	protocol string
	action   string
	number   int64
	egress   bool
}

// entry checks a rule and returns the entry it makes: an IPv4 block, a
// protocol (TCP and UDP need ports, ICMP a type and code), and a number
// from 1 to 32766.
func (r *aclRule) entry() (aclEntry, error) {
	e := aclEntry{Number: r.number, Egress: r.egress, Action: r.action}
	if r.number < 1 || r.number >= lastRule {
		return e, invalidParameter(fmt.Sprintf("Value (%d) for parameter ruleNumber is invalid. "+
			"Rule numbers must be from 1 to 32766.", r.number))
	}

	if r.action != actionAllow && r.action != actionDeny {
		return e, invalidParameter(fmt.Sprintf("Value (%s) for parameter ruleAction is invalid.", r.action))
	}

	block, err := netip.ParsePrefix(r.cidr)
	if err != nil || !block.Addr().Is4() {
		return e, invalidParameter(fmt.Sprintf("Value (%s) for parameter cidrBlock is invalid. "+
			"This is not a valid CIDR block.", r.cidr))
	}

	e.CIDR = block.Masked().String()
	proto, err := protocol(r.protocol)
	if err != nil {
		return e, err
	}

	numbers := map[string]string{protoTCP: "6", protoUDP: "17", protoICMP: "1", protoICMPv6: "58"}
	e.Protocol = cmp.Or(numbers[proto], proto)
	switch proto {
	case protoTCP, protoUDP:
		if r.ports == nil || r.ports.From == nil || r.ports.To == nil {
			return e, awserror.New("MissingParameter", "The request must contain the parameter portRange")
		}

		if *r.ports.From < 0 || *r.ports.To > 65535 || *r.ports.From > *r.ports.To {
			return e, invalidParameter("Invalid port range: ports must be from 0 to 65535, from at most to")
		}

		e.From, e.To = r.ports.From, r.ports.To
	case protoICMP, protoICMPv6:
		if r.icmp == nil || r.icmp.Type == nil || r.icmp.Code == nil {
			return e, awserror.New("MissingParameter", "The request must contain the parameter icmpTypeCode")
		}

		e.ICMPType, e.ICMPCode = r.icmp.Type, r.icmp.Code
	}

	return e, nil
}

// changeEntry loads an ACL, runs fn on it with the index of the entry with
// number in the given direction (-1 when there is none), and stores it with
// its entries in order.
func (s *Service) changeEntry(
	ctx context.Context,
	dry *bool,
	aclID string,
	number int64,
	egress bool,
	fn func(acl *networkACL, at int) error,
) error {
	return s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		acl, err := loadACL(tx, scope, aclID)
		if err != nil {
			return err
		}

		at := slices.IndexFunc(acl.Entries, func(e aclEntry) bool { return e.Number == number && e.Egress == egress })
		if fnErr := fn(acl, at); fnErr != nil {
			return fnErr
		}

		// Inbound entries come first, each direction by number.
		slices.SortFunc(acl.Entries, func(a, b aclEntry) int {
			switch {
			case a.Egress == b.Egress:
				return cmp.Compare(a.Number, b.Number)
			case b.Egress:
				return -1
			default:
				return 1
			}
		})
		return storage.Write(tx, scope.Key("ec2", kindNetworkACL, acl.ID), acl)
	})
}

func entryNotFound(number int64) error {
	return awserror.New("InvalidNetworkAclEntry.NotFound",
		fmt.Sprintf("The network acl entry identified by %d does not exist", number))
}

func (s *Service) CreateNetworkAclEntry(
	ctx context.Context,
	in *api.CreateNetworkAclEntryRequest,
) (*api.CreateNetworkAclEntryOutput, error) {
	e, err := (&aclRule{
		ports: in.PortRange, icmp: in.IcmpTypeCode, cidr: common.Value(in.CidrBlock), protocol: common.Value(in.Protocol),
		action: common.Value(in.RuleAction), number: common.Value(in.RuleNumber), egress: common.Value(in.Egress),
	}).entry()
	if err != nil {
		return nil, err
	}

	err = s.changeEntry(ctx, in.DryRun, common.Value(in.NetworkAclId), e.Number, e.Egress,
		func(acl *networkACL, at int) error {
			if at >= 0 {
				return awserror.New("NetworkAclEntryAlreadyExists",
					fmt.Sprintf("The network acl entry identified by %d already exists.", e.Number))
			}

			acl.Entries = append(acl.Entries, e)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.CreateNetworkAclEntryOutput{}, nil
}

func (s *Service) ReplaceNetworkAclEntry(
	ctx context.Context,
	in *api.ReplaceNetworkAclEntryRequest,
) (*api.ReplaceNetworkAclEntryOutput, error) {
	e, err := (&aclRule{
		ports: in.PortRange, icmp: in.IcmpTypeCode, cidr: common.Value(in.CidrBlock), protocol: common.Value(in.Protocol),
		action: common.Value(in.RuleAction), number: common.Value(in.RuleNumber), egress: common.Value(in.Egress),
	}).entry()
	if err != nil {
		return nil, err
	}

	err = s.changeEntry(ctx, in.DryRun, common.Value(in.NetworkAclId), e.Number, e.Egress,
		func(acl *networkACL, at int) error {
			if at < 0 {
				return entryNotFound(e.Number)
			}

			acl.Entries[at] = e
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.ReplaceNetworkAclEntryOutput{}, nil
}

func (s *Service) DeleteNetworkAclEntry(
	ctx context.Context,
	in *api.DeleteNetworkAclEntryRequest,
) (*api.DeleteNetworkAclEntryOutput, error) {
	number := common.Value(in.RuleNumber)
	err := s.changeEntry(ctx, in.DryRun, common.Value(in.NetworkAclId), number, common.Value(in.Egress),
		func(acl *networkACL, at int) error {
			switch {
			case number == lastRule:
				return invalidParameter(fmt.Sprintf("The network acl entry identified by %d cannot be deleted",
					number))
			case at < 0:
				return entryNotFound(number)
			}

			acl.Entries = slices.Delete(acl.Entries, at, at+1)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteNetworkAclEntryOutput{}, nil
}

// ReplaceNetworkAclAssociation moves a subnet to another ACL of its VPC
// under a new association ID.
func (s *Service) ReplaceNetworkAclAssociation(
	ctx context.Context,
	in *api.ReplaceNetworkAclAssociationRequest,
) (*api.ReplaceNetworkAclAssociationResult, error) {
	moved := aclAssociation{ID: newID("aclassoc")}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		to, err := loadACL(tx, scope, common.Value(in.NetworkAclId))
		if err != nil {
			return err
		}

		all, err := list[networkACL](tx, scope, kindNetworkACL)
		if err != nil {
			return err
		}

		id := common.Value(in.AssociationId)
		for _, from := range all {
			at := slices.IndexFunc(from.Associations, func(a aclAssociation) bool { return a.ID == id })
			if at < 0 {
				continue
			}

			if from.VPCID != to.VPCID {
				return invalidParameter(fmt.Sprintf("Network ACL %s is not in network %s", to.ID, from.VPCID))
			}

			moved.SubnetID = from.Associations[at].SubnetID
			from.Associations = slices.Delete(from.Associations, at, at+1)
			if writeErr := storage.Write(tx, scope.Key("ec2", kindNetworkACL, from.ID), from); writeErr != nil {
				return writeErr
			}

			// Replacing an association with the ACL that holds it changes its
			// ID.
			if from.ID == to.ID {
				to = from
			}

			to.Associations = append(to.Associations, moved)
			return storage.Write(tx, scope.Key("ec2", kindNetworkACL, to.ID), to)
		}

		return associationNotFound(id)
	})
	if err != nil {
		return nil, err
	}

	return &api.ReplaceNetworkAclAssociationResult{NewAssociationId: new(moved.ID)}, nil
}

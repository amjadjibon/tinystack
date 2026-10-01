package cloudformation

import (
	"context"
	"reflect"
	"slices"
)

// sourceKeys are the members of an AWS::EC2::SecurityGroup ingress rule,
// and destinationKeys those of an egress rule, beyond the protocol, ports,
// and Description.
var (
	sourceKeys = []string{
		memberCidrIP, memberCidrIPv6, "SourceSecurityGroupId", "SourceSecurityGroupOwnerId", "SourcePrefixListId",
	}
	destinationKeys = []string{memberCidrIP, memberCidrIPv6, "DestinationSecurityGroupId", "DestinationPrefixListId"}
)

// allTraffic is the egress rule EC2 gives a new security group, which a
// group without SecurityGroupEgress keeps.
var allTraffic = []any{map[string]any{memberIPProtocol: "-1", memberCidrIP: "0.0.0.0/0"}}

func init() {
	register("AWS::EC2::VPC", &resourceType{
		properties: []string{memberCidrBlock, "InstanceTenancy", memberTags},
		attributes: []string{memberCidrBlock, memberVpcID, "DefaultSecurityGroup"},
		replace:    []string{memberCidrBlock, "InstanceTenancy"},
		create:     createVpc,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, retagEC2(ctx, c, r.Physical, r.Properties, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ec2", "DeleteVpc", map[string]any{memberVpcID: r.Physical})
			if gone(err, "InvalidVpcID.NotFound") {
				return nil
			}

			return err
		},
	})
	register("AWS::EC2::Subnet", &resourceType{
		properties: []string{memberVpcID, memberCidrBlock, memberAvailabilityZone, "AvailabilityZoneId", memberTags},
		attributes: []string{memberSubnetID, memberVpcID, memberAvailabilityZone, memberCidrBlock},
		replace:    []string{memberVpcID, memberCidrBlock, memberAvailabilityZone, "AvailabilityZoneId"},
		create:     createSubnet,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, retagEC2(ctx, c, r.Physical, r.Properties, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ec2", "DeleteSubnet", map[string]any{memberSubnetID: r.Physical})
			if gone(err, "InvalidSubnetID.NotFound") {
				return nil
			}

			return err
		},
	})
	register("AWS::EC2::SecurityGroup", &resourceType{
		name: memberGroupName,
		properties: []string{
			memberGroupName, "GroupDescription", memberVpcID, "SecurityGroupIngress", "SecurityGroupEgress", memberTags,
		},
		attributes: []string{memberGroupID, memberVpcID},
		replace:    []string{memberGroupName, "GroupDescription", memberVpcID},
		create:     createSecurityGroup,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := applyRules(ctx, c, r.Physical, r.Properties, p); err != nil {
				return nil, err
			}

			return r.Attributes, retagEC2(ctx, c, r.Physical, r.Properties, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ec2", "DeleteSecurityGroup", map[string]any{memberGroupID: r.Physical})
			if gone(err, "InvalidGroup.NotFound") {
				return nil
			}

			return err
		},
	})
}

// tagSpecifications renders a resource's Tags property as EC2's.
func tagSpecifications(kind string, p props) []any {
	tags := p.tags()
	if len(tags) == 0 {
		return nil
	}

	return []any{map[string]any{"ResourceType": kind, memberTags: tagList(tags, memberKey, memberValue)}}
}

// retagEC2 moves an EC2 resource's tags from old's Tags property to p's.
func retagEC2(ctx context.Context, c *client, id string, old, p props) error {
	set, removed := tagChanges(old.tags(), p.tags())
	if len(removed) > 0 {
		keys := make([]any, 0, len(removed))
		for _, k := range removed {
			keys = append(keys, map[string]any{memberKey: k})
		}

		if _, err := c.call(ctx, "ec2", "DeleteTags", map[string]any{
			keyResources: []any{id}, memberTags: keys,
		}); err != nil {
			return err
		}
	}

	if len(set) == 0 {
		return nil
	}

	_, err := c.call(ctx, "ec2", "CreateTags", map[string]any{
		keyResources: []any{id}, memberTags: tagList(set, memberKey, memberValue),
	})
	return err
}

func createVpc(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	params := pick(map[string]any{}, p, memberCidrBlock, "InstanceTenancy")
	if specs := tagSpecifications("vpc", p); specs != nil {
		params["TagSpecifications"] = specs
	}

	out, err := c.call(ctx, "ec2", "CreateVpc", params)
	if err != nil {
		return "", nil, err
	}

	id := outText(out, "Vpc", memberVpcID)
	groups, err := c.call(ctx, "ec2", "DescribeSecurityGroups", map[string]any{"Filters": []any{
		map[string]any{memberName: "vpc-id", "Values": []any{id}},
		map[string]any{memberName: "group-name", "Values": []any{nameDefault}},
	}})
	if err != nil {
		return "", nil, err
	}

	var group string
	if list, _ := groups["SecurityGroups"].([]any); len(list) > 0 {
		m, _ := list[0].(map[string]any)
		group = outText(m, memberGroupID)
	}

	return id, map[string]any{
		memberCidrBlock: outText(out, "Vpc", memberCidrBlock), memberVpcID: id, "DefaultSecurityGroup": group,
	}, nil
}

func createSubnet(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	params := pick(map[string]any{}, p, memberVpcID, memberCidrBlock, memberAvailabilityZone, "AvailabilityZoneId")
	if specs := tagSpecifications("subnet", p); specs != nil {
		params["TagSpecifications"] = specs
	}

	out, err := c.call(ctx, "ec2", "CreateSubnet", params)
	if err != nil {
		return "", nil, err
	}

	id := outText(out, "Subnet", memberSubnetID)
	return id, map[string]any{
		memberSubnetID:         id,
		memberVpcID:            outText(out, "Subnet", memberVpcID),
		memberAvailabilityZone: outText(out, "Subnet", memberAvailabilityZone),
		memberCidrBlock:        outText(out, "Subnet", memberCidrBlock),
	}, nil
}

func createSecurityGroup(
	ctx context.Context, c *client, p props,
) (physical string, attributes map[string]any, err error) {
	params := pick(map[string]any{memberGroupName: c.nameOf(p, memberGroupName, 255)}, p, memberVpcID)
	params[memberDescription] = p.str("GroupDescription")
	if specs := tagSpecifications("security-group", p); specs != nil {
		params["TagSpecifications"] = specs
	}

	out, err := c.call(ctx, "ec2", "CreateSecurityGroup", params)
	if err != nil {
		return "", nil, err
	}

	id := outText(out, memberGroupID)
	err = applyRules(ctx, c, id, props{}, p)
	if err != nil {
		return "", nil, err
	}

	vpc := p.str(memberVpcID)
	if vpc == "" {
		groups, describeErr := c.call(ctx, "ec2", "DescribeSecurityGroups", map[string]any{"GroupIds": []any{id}})
		if describeErr != nil {
			return "", nil, describeErr
		}

		if list, _ := groups["SecurityGroups"].([]any); len(list) > 0 {
			m, _ := list[0].(map[string]any)
			vpc = outText(m, memberVpcID)
		}
	}

	return id, map[string]any{memberGroupID: id, memberVpcID: vpc}, nil
}

// egressRules returns a group's egress rules: SecurityGroupEgress, or the
// rule EC2 creates when it is absent.
func egressRules(p props) []any {
	if p.has("SecurityGroupEgress") {
		return p.list("SecurityGroupEgress")
	}

	return allTraffic
}

// applyRules moves a security group's rules from old's to p's.
func applyRules(ctx context.Context, c *client, id string, old, p props) error {
	ingress := &ruleSet{
		id: id, keys: sourceKeys, revoke: "RevokeSecurityGroupIngress",
		grant: "AuthorizeSecurityGroupIngress",
	}
	if err := ingress.apply(ctx, c, old.list("SecurityGroupIngress"), p.list("SecurityGroupIngress")); err != nil {
		return err
	}

	egress := &ruleSet{
		id: id, keys: destinationKeys, revoke: "RevokeSecurityGroupEgress",
		grant: "AuthorizeSecurityGroupEgress",
	}
	return egress.apply(ctx, c, egressRules(old), egressRules(p))
}

// ruleSet names a security group's ingress or egress rules: the members
// that name a rule's peer, and the operations that revoke and grant them.
type ruleSet struct {
	id, revoke, grant string
	keys              []string
}

// apply revokes the rules in before but not after, and then grants those
// only after has.
func (set *ruleSet) apply(ctx context.Context, c *client, before, after []any) error {
	removed, added := ruleChanges(before, after)
	if err := set.call(ctx, c, set.revoke, removed); err != nil {
		return err
	}

	return set.call(ctx, c, set.grant, added)
}

func (set *ruleSet) call(ctx context.Context, c *client, operation string, rules []any) error {
	if len(rules) == 0 {
		return nil
	}

	permissions := make([]any, 0, len(rules))
	for _, rule := range rules {
		perm, err := permission(rule, set.keys)
		if err != nil {
			return err
		}

		permissions = append(permissions, perm)
	}

	_, err := c.call(ctx, "ec2", operation, map[string]any{memberGroupID: set.id, "IpPermissions": permissions})
	return err
}

// ruleChanges returns the rules in before but not after, and those in after
// but not before.
func ruleChanges(before, after []any) (removed, added []any) {
	has := func(list []any, rule any) bool {
		return slices.ContainsFunc(list, func(r any) bool { return reflect.DeepEqual(r, rule) })
	}

	for _, r := range before {
		if !has(after, r) {
			removed = append(removed, r)
		}
	}

	for _, r := range after {
		if !has(before, r) {
			added = append(added, r)
		}
	}

	return removed, added
}

// permission renders a CloudFormation security group rule as an EC2
// IpPermission; keys are the members that name its peer.
func permission(rule any, keys []string) (map[string]any, error) {
	r, _ := rule.(map[string]any)
	for k := range r {
		if !slices.Contains(keys, k) &&
			!slices.Contains([]string{memberIPProtocol, "FromPort", "ToPort", memberDescription}, k) {
			return nil, validation("TinyStack does not support the %s property of a security group rule", k)
		}
	}

	rp := props(r)
	out := pick(map[string]any{memberIPProtocol: rp.str(memberIPProtocol)}, rp, "FromPort", "ToPort")
	peer := func(m map[string]any) []any {
		if rp.has(memberDescription) {
			m[memberDescription] = rp.str(memberDescription)
		}

		return []any{m}
	}

	switch {
	case rp.has(memberCidrIP):
		out["IpRanges"] = peer(map[string]any{memberCidrIP: rp.str(memberCidrIP)})
	case rp.has(memberCidrIPv6):
		out["Ipv6Ranges"] = peer(map[string]any{memberCidrIPv6: rp.str(memberCidrIPv6)})
	case rp.has("SourceSecurityGroupId"), rp.has("DestinationSecurityGroupId"):
		pair := map[string]any{memberGroupID: rp.str("SourceSecurityGroupId") + rp.str("DestinationSecurityGroupId")}
		if rp.has("SourceSecurityGroupOwnerId") {
			pair["UserId"] = rp.str("SourceSecurityGroupOwnerId")
		}

		out["UserIdGroupPairs"] = peer(pair)
	case rp.has("SourcePrefixListId"), rp.has("DestinationPrefixListId"):
		out["PrefixListIds"] = peer(map[string]any{
			"PrefixListId": rp.str("SourcePrefixListId") + rp.str("DestinationPrefixListId"),
		})
	default:
		return nil, validation("A security group rule needs a CIDR range, a security group, or a prefix list")
	}

	return out, nil
}

package ec2

import (
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
	kindSecurityGroup = "security-group"
	defaultGroupName  = "default"
	allProtocols      = "-1"
	anywhere          = "0.0.0.0/0"
	maxRules          = 60
	maxDescription    = 255
	filterGroupID     = "group-id"
	protoTCP          = "tcp"
	protoUDP          = "udp"
	protoICMP         = "icmp"
	protoICMPv6       = "icmpv6"
)

type securityGroup struct {
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Name        string
	Description string
	VPCID       string
	Ingress     []rule `json:",omitempty"`
	Egress      []rule `json:",omitempty"`
}

// rule is one security group rule with one source or destination: an IPv4
// or IPv6 CIDR, a prefix list, or a security group. FromPort and ToPort are
// nil when the protocol has no ports.
type rule struct {
	FromPort    *int64            `json:",omitempty"`
	ToPort      *int64            `json:",omitempty"`
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Protocol    string
	CIDR        string `json:",omitempty"`
	CIDRv6      string `json:",omitempty"`
	PrefixList  string `json:",omitempty"`
	Group       string `json:",omitempty"`
	Description string `json:",omitempty"`
}

// same reports whether two rules allow the same traffic.
func (r *rule) same(o *rule) bool {
	return r.Protocol == o.Protocol && value(r.FromPort) == value(o.FromPort) && value(r.ToPort) == value(o.ToPort) &&
		(r.FromPort == nil) == (o.FromPort == nil) && r.CIDR == o.CIDR && r.CIDRv6 == o.CIDRv6 &&
		r.PrefixList == o.PrefixList && r.Group == o.Group
}

func (r *rule) peer() string {
	return r.CIDR + r.CIDRv6 + r.PrefixList + r.Group
}

func groupNotFound(id string) error {
	return awserror.New("InvalidGroup.NotFound", fmt.Sprintf("The security group '%s' does not exist", id))
}

func groupARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/%s", scope.Region, scope.AccountID, id)
}

// defaultGroup returns the default security group of a new VPC: all traffic
// from its own members in, and all traffic out.
func defaultGroup(vpcID string) *securityGroup {
	g := &securityGroup{
		ID: newID("sg"), Name: defaultGroupName, Description: "default VPC security group", VPCID: vpcID,
	}
	g.Ingress = []rule{{ID: newID("sgr"), Protocol: allProtocols, Group: g.ID}}
	g.Egress = []rule{{ID: newID("sgr"), Protocol: allProtocols, CIDR: anywhere}}
	return g
}

// defaultVPC returns the Region's default VPC.
func defaultVPC(tx storage.ReadTx, scope request.Scope) (*vpc, error) {
	vpcs, err := list[vpc](tx, scope, kindVPC)
	if err != nil {
		return nil, err
	}

	for _, v := range vpcs {
		if v.IsDefault {
			return v, nil
		}
	}

	return nil, awserror.New("VPCIdNotSpecified", "No default VPC for this user")
}

// findGroup returns the security group with id, or, without one, the group
// named name in the default VPC.
func findGroup(tx storage.ReadTx, scope request.Scope, id, name string) (*securityGroup, error) {
	if id != "" {
		g, ok, err := read[securityGroup](tx, key(scope, kindSecurityGroup, id))
		if err == nil && !ok {
			err = groupNotFound(id)
		}

		return g, err
	}

	if name == "" {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter groupName or groupId")
	}

	v, err := defaultVPC(tx, scope)
	if err != nil {
		return nil, err
	}

	groups, err := list[securityGroup](tx, scope, kindSecurityGroup)
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		if g.VPCID == v.ID && g.Name == name {
			return g, nil
		}
	}

	return nil, awserror.New("InvalidGroup.NotFound",
		fmt.Sprintf("The security group '%s' does not exist in default VPC '%s'", name, v.ID))
}

// protocol normalizes an IP protocol given by name or number.
func protocol(text string) (string, error) {
	switch p := strings.ToLower(text); p {
	case protoTCP, "6":
		return protoTCP, nil
	case protoUDP, "17":
		return protoUDP, nil
	case protoICMP, "1":
		return protoICMP, nil
	case protoICMPv6, "58":
		return protoICMPv6, nil
	case allProtocols, "all":
		return allProtocols, nil
	default:
		if n, err := strconv.Atoi(p); err == nil && n >= 0 && n <= 255 {
			return p, nil
		}

		return "", invalidParameter(fmt.Sprintf("Invalid value '%s' for IP protocol. Unknown protocol.", text))
	}
}

// ports checks a permission's ports: TCP and UDP take a range from 0 to
// 65535, ICMP a type and code from -1 to 255, and other protocols none.
func ports(proto string, from, to *int64) (fromPort, toPort *int64, err error) {
	switch proto {
	case protoTCP, protoUDP:
		if from == nil || to == nil || *from < 0 || *to > 65535 || *from > *to {
			return nil, nil, invalidParameter(fmt.Sprintf("Invalid port range for protocol %s: "+
				"FromPort and ToPort must be from 0 to 65535, with FromPort at most ToPort", proto))
		}

		return from, to, nil
	case protoICMP, protoICMPv6:
		fromPort, toPort = new(int64(-1)), new(int64(-1))
		if from != nil {
			fromPort = from
		}

		if to != nil {
			toPort = to
		}

		if *fromPort < -1 || *fromPort > 255 || *toPort < -1 || *toPort > 255 {
			return nil, nil, invalidParameter("Invalid ICMP type or code: each must be from -1 to 255")
		}

		return fromPort, toPort, nil
	default:
		return nil, nil, nil
	}
}

// expand turns IpPermissions into one rule per source, checking each and the
// security groups they name.
func expand(tx storage.ReadTx, scope request.Scope, perms []api.IpPermission) ([]rule, error) {
	var out []rule
	for i := range perms {
		p := &perms[i]
		proto, err := protocol(value(p.IpProtocol))
		if err != nil {
			return nil, err
		}

		from, to, err := ports(proto, p.FromPort, p.ToPort)
		if err != nil {
			return nil, err
		}

		base := rule{Protocol: proto, FromPort: from, ToPort: to}
		add := func(r rule, description *string) error {
			if len(value(description)) > maxDescription {
				return invalidParameter("Rule descriptions must be at most 255 characters")
			}

			r.Description = value(description)
			out = append(out, r)
			return nil
		}

		for _, rg := range p.IpRanges {
			block, parseErr := netip.ParsePrefix(value(rg.CidrIp))
			if parseErr != nil || !block.Addr().Is4() {
				return nil, invalidParameter(fmt.Sprintf("CIDR block %s is malformed", value(rg.CidrIp)))
			}

			r := base
			r.CIDR = block.Masked().String()
			if addErr := add(r, rg.Description); addErr != nil {
				return nil, addErr
			}
		}

		for _, rg := range p.Ipv6Ranges {
			block, parseErr := netip.ParsePrefix(value(rg.CidrIpv6))
			if parseErr != nil || !block.Addr().Is6() {
				return nil, invalidParameter(fmt.Sprintf("CIDR block %s is malformed", value(rg.CidrIpv6)))
			}

			r := base
			r.CIDRv6 = block.Masked().String()
			if addErr := add(r, rg.Description); addErr != nil {
				return nil, addErr
			}
		}

		for _, pl := range p.PrefixListIds {
			r := base
			r.PrefixList = value(pl.PrefixListId)
			if addErr := add(r, pl.Description); addErr != nil {
				return nil, addErr
			}
		}

		for _, pair := range p.UserIdGroupPairs {
			if owner := value(pair.UserId); owner != "" && owner != scope.AccountID {
				return nil, invalidParameter("TinyStack does not support security group references to other accounts")
			}

			g, findErr := findGroup(tx, scope, value(pair.GroupId), value(pair.GroupName))
			if findErr != nil {
				return nil, findErr
			}

			r := base
			r.Group = g.ID
			if addErr := add(r, pair.Description); addErr != nil {
				return nil, addErr
			}
		}
	}

	return out, nil
}

// permissions groups rules into IpPermissions by protocol and ports, in the
// order they were added.
func permissions(scope request.Scope, rules []rule) []api.IpPermission {
	out := []api.IpPermission{}
	for i := range rules {
		r := &rules[i]
		at := slices.IndexFunc(out, func(p api.IpPermission) bool {
			return *p.IpProtocol == r.Protocol && value(p.FromPort) == value(r.FromPort) &&
				value(p.ToPort) == value(r.ToPort) && (p.FromPort == nil) == (r.FromPort == nil)
		})
		if at < 0 {
			out = append(out, api.IpPermission{IpProtocol: new(r.Protocol), FromPort: r.FromPort, ToPort: r.ToPort})
			at = len(out) - 1
		}

		p := &out[at]
		var description *string
		if r.Description != "" {
			description = new(r.Description)
		}

		switch {
		case r.CIDR != "":
			p.IpRanges = append(p.IpRanges, api.IpRange{CidrIp: new(r.CIDR), Description: description})
		case r.CIDRv6 != "":
			p.Ipv6Ranges = append(p.Ipv6Ranges, api.Ipv6Range{CidrIpv6: new(r.CIDRv6), Description: description})
		case r.PrefixList != "":
			p.PrefixListIds = append(p.PrefixListIds, api.PrefixListId{
				PrefixListId: new(r.PrefixList), Description: description,
			})
		default:
			p.UserIdGroupPairs = append(p.UserIdGroupPairs, api.UserIdGroupPair{
				GroupId: new(r.Group), UserId: new(scope.AccountID), Description: description,
			})
		}
	}

	return out
}

func groupOutput(scope request.Scope, g *securityGroup) api.SecurityGroup {
	return api.SecurityGroup{
		GroupId: new(g.ID), GroupName: new(g.Name), Description: new(g.Description), VpcId: new(g.VPCID),
		OwnerId: new(scope.AccountID), SecurityGroupArn: new(groupARN(scope, g.ID)),
		IpPermissions: permissions(scope, g.Ingress), IpPermissionsEgress: permissions(scope, g.Egress),
		Tags: tagList(g.Tags),
	}
}

func ruleOutput(scope request.Scope, g *securityGroup, r *rule, egress bool) api.SecurityGroupRule {
	out := api.SecurityGroupRule{
		SecurityGroupRuleId: new(r.ID), GroupId: new(g.ID), GroupOwnerId: new(scope.AccountID),
		IsEgress: new(egress), IpProtocol: new(r.Protocol), FromPort: new(int64(-1)), ToPort: new(int64(-1)),
		SecurityGroupRuleArn: new(fmt.Sprintf("arn:aws:ec2:%s:%s:security-group-rule/%s",
			scope.Region, scope.AccountID, r.ID)),
		Tags: tagList(r.Tags),
	}
	if r.FromPort != nil {
		out.FromPort, out.ToPort = r.FromPort, r.ToPort
	}

	if r.Description != "" {
		out.Description = new(r.Description)
	}

	switch {
	case r.CIDR != "":
		out.CidrIpv4 = new(r.CIDR)
	case r.CIDRv6 != "":
		out.CidrIpv6 = new(r.CIDRv6)
	case r.PrefixList != "":
		out.PrefixListId = new(r.PrefixList)
	default:
		out.ReferencedGroupInfo = &api.ReferencedSecurityGroup{GroupId: new(r.Group), UserId: new(scope.AccountID)}
	}

	return out
}

func groupValues(scope request.Scope, g *securityGroup) values {
	v := values{
		filterGroupID: {g.ID}, "group-name": {g.Name}, "description": {g.Description}, filterVPCID: {g.VPCID},
		filterOwnerID: {scope.AccountID},
	}
	for prefix, rules := range map[string][]rule{"ip-permission.": g.Ingress, "egress.ip-permission.": g.Egress} {
		for _, name := range []string{
			filterCIDR, "ipv6-cidr", "prefix-list-id", filterGroupID, "protocol", "from-port",
			"to-port",
		} {
			v[prefix+name] = []string{}
		}

		for i := range rules {
			r := &rules[i]
			v[prefix+"protocol"] = append(v[prefix+"protocol"], r.Protocol)
			if r.FromPort != nil {
				v[prefix+"from-port"] = append(v[prefix+"from-port"], strconv.FormatInt(*r.FromPort, 10))
				v[prefix+"to-port"] = append(v[prefix+"to-port"], strconv.FormatInt(*r.ToPort, 10))
			}

			for name, peer := range map[string]string{
				filterCIDR: r.CIDR, "ipv6-cidr": r.CIDRv6, "prefix-list-id": r.PrefixList, filterGroupID: r.Group,
			} {
				if peer != "" {
					v[prefix+name] = append(v[prefix+name], peer)
				}
			}
		}
	}

	return v.withTags(g.Tags)
}

func (s *Service) CreateSecurityGroup(
	ctx context.Context,
	in *api.CreateSecurityGroupRequest,
) (*api.CreateSecurityGroupResult, error) {
	name, description := value(in.GroupName), value(in.Description)
	switch {
	case name == "" || description == "":
		return nil, awserror.New("MissingParameter", "The request must contain the parameters groupName and "+
			"groupDescription")
	case len(name) > maxDescription || len(description) > maxDescription:
		return nil, invalidParameter("Group names and descriptions must be at most 255 characters")
	case strings.HasPrefix(name, "sg-"):
		return nil, invalidParameter("Group names may not be in the format sg-*")
	case name == defaultGroupName:
		return nil, awserror.New("InvalidGroup.Reserved", "The security group 'default' is reserved")
	}

	tags, err := specTags(in.TagSpecifications, "security-group")
	if err != nil {
		return nil, err
	}

	g := &securityGroup{ID: newID("sg"), Name: name, Description: description, Tags: tags}
	g.Egress = []rule{{ID: newID("sgr"), Protocol: allProtocols, CIDR: anywhere}}
	var arn string
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, findErr := defaultVPC(tx, scope)
		if in.VpcId != nil {
			v, findErr = loadVPC(tx, scope, *in.VpcId)
		}

		if findErr != nil {
			return findErr
		}

		groups, listErr := list[securityGroup](tx, scope, kindSecurityGroup)
		if listErr != nil {
			return listErr
		}

		if slices.ContainsFunc(groups, func(o *securityGroup) bool { return o.VPCID == v.ID && o.Name == name }) {
			return awserror.New("InvalidGroup.Duplicate",
				fmt.Sprintf("The security group '%s' already exists for VPC '%s'", name, v.ID))
		}

		g.VPCID, arn = v.ID, groupARN(scope, g.ID)
		return write(tx, key(scope, kindSecurityGroup, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateSecurityGroupResult{GroupId: new(g.ID), SecurityGroupArn: &arn, Tags: tagList(tags)}, nil
}

// DeleteSecurityGroup deletes a group that is not a default group and that
// no other group's rules name.
func (s *Service) DeleteSecurityGroup(
	ctx context.Context,
	in *api.DeleteSecurityGroupRequest,
) (*api.DeleteSecurityGroupResult, error) {
	var id string
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := findGroup(tx, scope, value(in.GroupId), value(in.GroupName))
		if err != nil {
			return err
		}

		if g.Name == defaultGroupName {
			return awserror.New("CannotDelete",
				fmt.Sprintf(`the specified group: %q name: "default" cannot be deleted by a user`, g.ID))
		}

		groups, err := list[securityGroup](tx, scope, kindSecurityGroup)
		if err != nil {
			return err
		}

		for _, other := range groups {
			names := func(r rule) bool { return r.Group == g.ID }
			if other.ID != g.ID &&
				(slices.ContainsFunc(other.Ingress, names) || slices.ContainsFunc(other.Egress, names)) {
				return awserror.New("DependencyViolation", fmt.Sprintf("resource %s has a dependent object", g.ID))
			}
		}

		id = g.ID
		tx.Delete(key(scope, kindSecurityGroup, g.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSecurityGroupResult{Return: new(true), GroupId: &id}, nil
}

func (s *Service) DescribeSecurityGroups(
	ctx context.Context,
	in *api.DescribeSecurityGroupsRequest,
) (*api.DescribeSecurityGroupsResult, error) {
	match, err := compileFilters(in.Filters, groupValues(request.Scope{}, &securityGroup{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeSecurityGroupsResult{SecurityGroups: []api.SecurityGroup{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[securityGroup](tx, scope, kindSecurityGroup)
		if listErr != nil {
			return listErr
		}

		id := func(g *securityGroup) string { return g.ID }
		selected, selectErr := selectIDs(all, id, in.GroupIds, groupNotFound)
		if selectErr != nil {
			return selectErr
		}

		if len(in.GroupNames) > 0 {
			var named []*securityGroup
			for _, name := range in.GroupNames {
				g, findErr := findGroup(tx, scope, "", name)
				if findErr != nil {
					return findErr
				}

				if slices.ContainsFunc(selected, func(o *securityGroup) bool { return o.ID == g.ID }) {
					named = append(named, g)
				}
			}

			selected = named
			slices.SortFunc(selected, func(a, b *securityGroup) int { return strings.Compare(a.ID, b.ID) })
		}

		selected = slices.DeleteFunc(
			selected,
			func(g *securityGroup) bool { return !match.match(groupValues(scope, g)) },
		)
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, g := range selected {
			out.SecurityGroups = append(out.SecurityGroups, groupOutput(scope, g))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// authorize adds rules to a group's ingress or egress rules.
func (s *Service) authorize(
	ctx context.Context,
	dry *bool,
	id, name string,
	perms []api.IpPermission,
	specs []api.TagSpecification,
	egress bool,
) ([]api.SecurityGroupRule, error) {
	tags, err := specTags(specs, "security-group-rule")
	if err != nil {
		return nil, err
	}

	var added []api.SecurityGroupRule
	err = s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		g, findErr := findGroup(tx, scope, id, name)
		if findErr != nil {
			return findErr
		}

		rules, expandErr := expand(tx, scope, perms)
		if expandErr != nil {
			return expandErr
		}

		current := &g.Ingress
		if egress {
			current = &g.Egress
		}

		for i := range rules {
			r := &rules[i]
			if slices.ContainsFunc(*current, func(o rule) bool { return o.same(r) }) {
				return awserror.New("InvalidPermission.Duplicate", fmt.Sprintf(
					`the specified rule "peer: %s, %s, ALLOW" already exists`, r.peer(), r.Protocol))
			}

			r.ID, r.Tags = newID("sgr"), tags
			*current = append(*current, *r)
			added = append(added, ruleOutput(scope, g, r, egress))
		}

		if len(*current) > maxRules {
			return awserror.New("RulesPerSecurityGroupLimitExceeded",
				"The maximum number of rules per security group has been reached.")
		}

		return write(tx, key(scope, kindSecurityGroup, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return added, nil
}

// revoke removes rules from a group, by rule ID or by permission; the
// permissions that match no rule are returned.
func (s *Service) revoke(
	ctx context.Context,
	dry *bool,
	id, name string,
	perms []api.IpPermission,
	ruleIDs []string,
	egress bool,
) ([]api.IpPermission, error) {
	var unknown []api.IpPermission
	err := s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := findGroup(tx, scope, id, name)
		if err != nil {
			return err
		}

		current := &g.Ingress
		if egress {
			current = &g.Egress
		}

		for _, ruleID := range ruleIDs {
			i := slices.IndexFunc(*current, func(r rule) bool { return r.ID == ruleID })
			if i < 0 {
				return awserror.New("InvalidSecurityGroupRuleId.NotFound",
					fmt.Sprintf("The security group rule ID '%s' does not exist", ruleID))
			}

			*current = slices.Delete(*current, i, i+1)
		}

		removed := len(ruleIDs)
		for i := range perms {
			rules, expandErr := expand(tx, scope, perms[i:i+1])
			if expandErr != nil {
				return expandErr
			}

			found := false
			for j := range rules {
				if at := slices.IndexFunc(*current, func(o rule) bool { return o.same(&rules[j]) }); at >= 0 {
					*current = slices.Delete(*current, at, at+1)
					found, removed = true, removed+1
				}
			}

			if !found {
				unknown = append(unknown, perms[i])
			}
		}

		if removed == 0 {
			return awserror.New("InvalidPermission.NotFound",
				"The specified rule does not exist in this security group.")
		}

		return write(tx, key(scope, kindSecurityGroup, g.ID), g)
	})

	return unknown, err
}

func (s *Service) AuthorizeSecurityGroupIngress(
	ctx context.Context,
	in *api.AuthorizeSecurityGroupIngressRequest,
) (*api.AuthorizeSecurityGroupIngressResult, error) {
	rules, err := s.authorize(ctx, in.DryRun, value(in.GroupId), value(in.GroupName), in.IpPermissions,
		in.TagSpecifications, false)
	if err != nil {
		return nil, err
	}

	return &api.AuthorizeSecurityGroupIngressResult{Return: new(true), SecurityGroupRules: rules}, nil
}

func (s *Service) AuthorizeSecurityGroupEgress(
	ctx context.Context,
	in *api.AuthorizeSecurityGroupEgressRequest,
) (*api.AuthorizeSecurityGroupEgressResult, error) {
	rules, err := s.authorize(ctx, in.DryRun, value(in.GroupId), "", in.IpPermissions, in.TagSpecifications, true)
	if err != nil {
		return nil, err
	}

	return &api.AuthorizeSecurityGroupEgressResult{Return: new(true), SecurityGroupRules: rules}, nil
}

func (s *Service) RevokeSecurityGroupIngress(
	ctx context.Context,
	in *api.RevokeSecurityGroupIngressRequest,
) (*api.RevokeSecurityGroupIngressResult, error) {
	unknown, err := s.revoke(ctx, in.DryRun, value(in.GroupId), value(in.GroupName), in.IpPermissions,
		in.SecurityGroupRuleIds, false)
	if err != nil {
		return nil, err
	}

	return &api.RevokeSecurityGroupIngressResult{Return: new(true), UnknownIpPermissions: unknown}, nil
}

func (s *Service) RevokeSecurityGroupEgress(
	ctx context.Context,
	in *api.RevokeSecurityGroupEgressRequest,
) (*api.RevokeSecurityGroupEgressResult, error) {
	unknown, err := s.revoke(ctx, in.DryRun, value(in.GroupId), "", in.IpPermissions, in.SecurityGroupRuleIds, true)
	if err != nil {
		return nil, err
	}

	return &api.RevokeSecurityGroupEgressResult{Return: new(true), UnknownIpPermissions: unknown}, nil
}

func (s *Service) DescribeSecurityGroupRules(
	ctx context.Context,
	in *api.DescribeSecurityGroupRulesRequest,
) (*api.DescribeSecurityGroupRulesResult, error) {
	match, err := compileFilters(in.Filters, filterGroupID, "security-group-rule-id")
	if err != nil {
		return nil, err
	}

	var found []*api.SecurityGroupRule
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		groups, listErr := list[securityGroup](tx, scope, kindSecurityGroup)
		if listErr != nil {
			return listErr
		}

		for _, g := range groups {
			for _, side := range []struct {
				rules  []rule
				egress bool
			}{{g.Ingress, false}, {g.Egress, true}} {
				for i := range side.rules {
					r := &side.rules[i]
					if match.match(values{filterGroupID: {g.ID}, "security-group-rule-id": {r.ID}}.withTags(r.Tags)) {
						out := ruleOutput(scope, g, r, side.egress)
						found = append(found, &out)
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(r *api.SecurityGroupRule) string { return *r.SecurityGroupRuleId }
	slices.SortFunc(found, func(a, b *api.SecurityGroupRule) int { return strings.Compare(id(a), id(b)) })
	found, err = selectIDs(found, id, in.SecurityGroupRuleIds, func(ruleID string) error {
		return awserror.New("InvalidSecurityGroupRuleId.NotFound",
			fmt.Sprintf("The security group rule ID '%s' does not exist", ruleID))
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(found, id, in.NextToken, in.MaxResults, maxDescribe)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeSecurityGroupRulesResult{SecurityGroupRules: []api.SecurityGroupRule{}, NextToken: next}
	for _, r := range selected {
		out.SecurityGroupRules = append(out.SecurityGroupRules, *r)
	}

	return out, nil
}

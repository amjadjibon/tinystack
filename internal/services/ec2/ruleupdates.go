package ec2

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func ruleNotFound(id string) error {
	return notFound("InvalidSecurityGroupRuleId.NotFound", "security group rule", id)
}

func modifiedRule(tx storage.ReadTx, scope request.Scope, old *rule, in *api.SecurityGroupRuleRequest) (rule, error) {
	r := *common.Clone(old)
	if in == nil {
		return r, invalidParameter("SecurityGroupRule is required")
	}

	if in.IpProtocol != nil {
		r.Protocol = *in.IpProtocol
	}

	if in.FromPort != nil {
		r.FromPort = common.Clone(in.FromPort)
	}

	if in.ToPort != nil {
		r.ToPort = common.Clone(in.ToPort)
	}

	if in.Description != nil {
		r.Description = *in.Description
	}

	if in.CidrIpv4 != nil || in.CidrIpv6 != nil || in.PrefixListId != nil || in.ReferencedGroupId != nil {
		if err := onlyOne(
			in.CidrIpv4 != nil,
			in.CidrIpv6 != nil,
			in.PrefixListId != nil,
			in.ReferencedGroupId != nil,
		); err != nil {
			return r, err
		}

		r.CIDR, r.CIDRv6, r.PrefixList, r.Group = common.Value(in.CidrIpv4), common.Value(in.CidrIpv6),
			common.Value(in.PrefixListId), common.Value(in.ReferencedGroupId)
	}

	expanded, err := expand(tx, scope, permissions(scope, []rule{r}))
	if err != nil {
		return r, err
	}

	if len(expanded) != 1 {
		return r, invalidParameter("Rule must have exactly one source or destination")
	}

	expanded[0].ID, expanded[0].Tags = r.ID, r.Tags
	return expanded[0], nil
}

func (s *Service) ModifySecurityGroupRules(ctx context.Context, in *api.ModifySecurityGroupRulesRequest) (
	*api.ModifySecurityGroupRulesResult, error,
) {
	if len(in.SecurityGroupRules) == 0 {
		return nil, invalidParameter("SecurityGroupRules is required")
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := findGroup(tx, scope, common.Value(in.GroupId), "")
		if err != nil {
			return err
		}

		seen := map[string]bool{}
		for _, change := range in.SecurityGroupRules {
			id := common.Value(change.SecurityGroupRuleId)
			if seen[id] {
				return invalidParameter("Duplicate rule update")
			}

			seen[id] = true
			found := false
			for _, rules := range [][]rule{g.Ingress, g.Egress} {
				at := slices.IndexFunc(rules, func(r rule) bool { return r.ID == id })
				if at < 0 {
					continue
				}

				candidate, err := modifiedRule(tx, scope, &rules[at], change.SecurityGroupRule)
				if err != nil {
					return err
				}

				rules[at], found = candidate, true
			}

			if !found {
				return ruleNotFound(id)
			}
		}

		for _, rules := range [][]rule{g.Ingress, g.Egress} {
			for i := range rules {
				for j := range i {
					if rules[i].same(&rules[j]) {
						return awserror.New("InvalidPermission.Duplicate", "Rule already exists")
					}
				}
			}
		}

		return storage.Write(tx, scope.Key("ec2", kindSecurityGroup, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifySecurityGroupRulesResult{Return: new(true)}, nil
}

func (s *Service) updateRuleDescriptions(ctx context.Context, dry *bool, id, name string, egress bool,
	perms []api.IpPermission, descriptions []api.SecurityGroupRuleDescription,
) error {
	if err := onlyOne(len(perms) > 0, len(descriptions) > 0); err != nil {
		return err
	}

	return s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := findGroup(tx, scope, id, name)
		if err != nil {
			return err
		}

		rules := g.Ingress
		if egress {
			rules = g.Egress
		}

		expanded, err := expand(tx, scope, perms)
		if err != nil {
			return err
		}

		for i := range expanded {
			want := &expanded[i]
			at := slices.IndexFunc(rules, func(r rule) bool { return r.same(want) })
			if at < 0 {
				return awserror.New("InvalidPermission.NotFound", "Rule does not exist")
			}

			rules[at].Description = want.Description
		}

		for _, d := range descriptions {
			at := slices.IndexFunc(rules, func(r rule) bool { return r.ID == common.Value(d.SecurityGroupRuleId) })
			if at < 0 {
				return ruleNotFound(common.Value(d.SecurityGroupRuleId))
			}

			if len(common.Value(d.Description)) > maxDescription {
				return invalidParameter("Description exceeds 255 characters")
			}

			rules[at].Description = common.Value(d.Description)
		}

		return storage.Write(tx, scope.Key("ec2", kindSecurityGroup, g.ID), g)
	})
}

func (s *Service) UpdateSecurityGroupRuleDescriptionsIngress(
	ctx context.Context,
	in *api.UpdateSecurityGroupRuleDescriptionsIngressRequest,
) (
	*api.UpdateSecurityGroupRuleDescriptionsIngressResult, error,
) {
	err := s.updateRuleDescriptions(ctx, in.DryRun, common.Value(in.GroupId), common.Value(in.GroupName), false,
		in.IpPermissions, in.SecurityGroupRuleDescriptions)
	if err != nil {
		return nil, err
	}

	return &api.UpdateSecurityGroupRuleDescriptionsIngressResult{Return: new(true)}, nil
}

func (s *Service) UpdateSecurityGroupRuleDescriptionsEgress(
	ctx context.Context,
	in *api.UpdateSecurityGroupRuleDescriptionsEgressRequest,
) (
	*api.UpdateSecurityGroupRuleDescriptionsEgressResult, error,
) {
	err := s.updateRuleDescriptions(ctx, in.DryRun, common.Value(in.GroupId), common.Value(in.GroupName), true,
		in.IpPermissions, in.SecurityGroupRuleDescriptions)
	if err != nil {
		return nil, err
	}

	return &api.UpdateSecurityGroupRuleDescriptionsEgressResult{Return: new(true)}, nil
}

func activePeeringBetween(peers []*peeringConnection, a, b string) string {
	for _, p := range peers {
		if p.State == routeActive && ((common.Value(p.Requester.VpcId) == a && common.Value(p.Accepter.VpcId) == b) ||
			(common.Value(p.Accepter.VpcId) == a && common.Value(p.Requester.VpcId) == b)) {
			return p.ID
		}
	}

	return ""
}

func (s *Service) DescribeSecurityGroupReferences(ctx context.Context, in *api.DescribeSecurityGroupReferencesRequest) (
	*api.DescribeSecurityGroupReferencesResult, error,
) {
	out := &api.DescribeSecurityGroupReferencesResult{SecurityGroupReferenceSet: []api.SecurityGroupReference{}}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := storage.Records[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, ""))
		if err != nil {
			return err
		}

		peers, err := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
		if err != nil {
			return err
		}

		for _, id := range in.GroupId {
			target, err := findGroup(tx, scope, id, "")
			if err != nil {
				return err
			}

			seen := map[string]bool{}
			for _, g := range all {
				if g.VPCID == target.VPCID || seen[g.VPCID] {
					continue
				}

				if !slices.ContainsFunc(
					append(slices.Clone(g.Ingress), g.Egress...),
					func(r rule) bool { return r.Group == id },
				) {
					continue
				}

				peerID := activePeeringBetween(peers, g.VPCID, target.VPCID)
				if peerID == "" {
					continue
				}

				seen[g.VPCID] = true
				out.SecurityGroupReferenceSet = append(out.SecurityGroupReferenceSet, api.SecurityGroupReference{
					GroupId: new(id), ReferencingVpcId: new(g.VPCID), VpcPeeringConnectionId: new(peerID),
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeStaleSecurityGroups(ctx context.Context, in *api.DescribeStaleSecurityGroupsRequest) (
	*api.DescribeStaleSecurityGroupsResult, error,
) {
	out := &api.DescribeStaleSecurityGroupsResult{StaleSecurityGroupSet: []api.StaleSecurityGroup{}}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		v, err := loadVPC(tx, scope, common.Value(in.VpcId))
		if err != nil {
			return err
		}

		all, err := storage.Records[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, ""))
		if err != nil {
			return err
		}

		peers, err := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
		if err != nil {
			return err
		}

		for _, g := range all {
			if g.VPCID != v.ID {
				continue
			}

			item := api.StaleSecurityGroup{
				GroupId:     new(g.ID),
				GroupName:   new(g.Name),
				Description: new(g.Description),
				VpcId:       new(g.VPCID),
			}

			for direction, rules := range [][]rule{g.Ingress, g.Egress} {
				for i := range rules {
					r := &rules[i]
					if r.Group == "" {
						continue
					}

					target, ok, readErr := storage.Read[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, r.Group))
					if readErr != nil {
						return readErr
					}

					if ok && (target.VPCID == g.VPCID || activePeeringBetween(peers, g.VPCID, target.VPCID) != "") {
						continue
					}

					permission := api.StaleIpPermission{
						IpProtocol: new(r.Protocol), FromPort: common.Clone(r.FromPort), ToPort: common.Clone(r.ToPort),
						UserIdGroupPairs: []api.UserIdGroupPair{{GroupId: new(r.Group), UserId: new(scope.AccountID)}},
					}

					if direction == 0 {
						item.StaleIpPermissions = append(item.StaleIpPermissions, permission)
					} else {
						item.StaleIpPermissionsEgress = append(item.StaleIpPermissionsEgress, permission)
					}
				}
			}

			if len(item.StaleIpPermissions)+len(item.StaleIpPermissionsEgress) > 0 {
				out.StaleSecurityGroupSet = append(out.StaleSecurityGroupSet, item)
			}
		}

		limit := len(out.StaleSecurityGroupSet) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		out.StaleSecurityGroupSet, out.NextToken, err = common.Page(
			out.StaleSecurityGroupSet,
			func(g api.StaleSecurityGroup) string { return common.Value(g.GroupId) },
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

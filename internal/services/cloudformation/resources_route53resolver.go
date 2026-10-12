package cloudformation

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	resolverDirection     = "Direction"
	resolverService       = "route53resolver"
	resolverEndpointID    = "ResolverEndpointId"
	resolverRuleID        = "ResolverRuleId"
	resolverAssociationID = "ResolverRuleAssociationId"
	resolverVPCID         = "VPCId"
	resolverIPs           = "IpAddresses"
	resolverTargets       = "TargetIps"
)

func resolverAttributes(out map[string]any, kind, idMember string) (id string, attrs map[string]any) {
	data, _ := out[kind].(map[string]any)
	id = outText(data, "Id")
	attrs = map[string]any{idMember: id, memberName: outText(data, memberName)}
	for _, key := range []string{
		memberArn, memberName, resolverDirection, "HostVPCId", "IpAddressCount", memberDomainName,
		resolverEndpointID, resolverRuleID, resolverVPCID, resolverTargets,
	} {
		if value, ok := data[key]; ok {
			attrs[key] = value
		}
	}

	return id, attrs
}

func resolverParams(p props, keys ...string) (map[string]any, error) {
	for key, value := range p {
		if value == nil {
			return nil, validation("Route 53 Resolver property %s cannot be null", key)
		}
	}

	return pick(map[string]any{}, p, keys...), nil
}

func resolverTagger(arn string) *tagger {
	return &tagger{
		service: resolverService, tag: opTagResource, untag: opUntagResource,
		target: map[string]any{memberResourceArn: arn}, key: memberKey, value: memberValue,
	}
}

func init() {
	register("AWS::Route53Resolver::ResolverEndpoint", &resourceType{
		properties: []string{memberName, resolverDirection, mwaaSecurityGroups, resolverIPs, memberTags},
		replace:    []string{resolverDirection, mwaaSecurityGroups},
		attributes: []string{
			memberArn,
			resolverDirection,
			"HostVPCId",
			"IpAddressCount",
			memberName,
			resolverEndpointID,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := resolverParams(p, memberName, resolverDirection, mwaaSecurityGroups, resolverIPs, memberTags)
			if err != nil {
				return "", nil, err
			}

			if direction, ok := params[resolverDirection].(string); ok {
				params[resolverDirection] = strings.ToUpper(direction)
			}

			params["CreatorRequestId"] = common.RandomID(32)
			out, err := c.call(ctx, resolverService, "CreateResolverEndpoint", params)
			if err != nil {
				return "", nil, err
			}

			id, attrs := resolverAttributes(out, "ResolverEndpoint", resolverEndpointID)
			return id, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			for _, key := range []string{resolverIPs, memberTags} {
				if p.changed(props(r.Properties), key) {
					return nil, validation("Updating endpoint %s is unsupported", key)
				}
			}

			if _, ok := r.Properties[memberName]; ok && !p.has(memberName) {
				return nil, validation("Removing endpoint Name is unsupported")
			}

			params, err := resolverParams(p, memberName)
			if err != nil {
				return nil, err
			}

			params[resolverEndpointID] = r.Physical
			out, err := c.call(ctx, resolverService, "UpdateResolverEndpoint", params)
			if err != nil {
				return nil, err
			}

			_, attrs := resolverAttributes(out, "ResolverEndpoint", resolverEndpointID)
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				resolverService,
				"DeleteResolverEndpoint",
				map[string]any{resolverEndpointID: r.Physical},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Route53Resolver::ResolverRule", &resourceType{
		properties: []string{memberName, memberDomainName, "RuleType", resolverEndpointID, resolverTargets, memberTags},
		replace:    []string{"RuleType", memberDomainName},
		attributes: []string{
			memberArn,
			memberDomainName,
			memberName,
			resolverEndpointID,
			resolverRuleID,
			resolverTargets,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := resolverParams(
				p,
				memberName,
				memberDomainName,
				"RuleType",
				resolverEndpointID,
				resolverTargets,
				memberTags,
			)
			if err != nil {
				return "", nil, err
			}

			params["CreatorRequestId"] = common.RandomID(32)
			out, err := c.call(ctx, resolverService, "CreateResolverRule", params)
			if err != nil {
				return "", nil, err
			}

			id, attrs := resolverAttributes(out, "ResolverRule", resolverRuleID)
			return id, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			for _, key := range []string{memberName, resolverEndpointID, resolverTargets} {
				if _, ok := r.Properties[key]; ok && !p.has(key) {
					return nil, validation("Removing rule %s is unsupported", key)
				}
			}

			params, err := resolverParams(p, memberName, resolverEndpointID, resolverTargets)
			if err != nil {
				return nil, err
			}

			out, err := c.call(
				ctx,
				resolverService,
				"UpdateResolverRule",
				map[string]any{resolverRuleID: r.Physical, "Config": params},
			)
			if err != nil {
				return nil, err
			}

			_, attrs := resolverAttributes(out, "ResolverRule", resolverRuleID)
			if err := retag(ctx, c, r.Properties, p, resolverTagger(outText(attrs, memberArn))); err != nil {
				return nil, err
			}

			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, resolverService, "DeleteResolverRule", map[string]any{resolverRuleID: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Route53Resolver::ResolverRuleAssociation", &resourceType{
		properties: []string{memberName, resolverRuleID, resolverVPCID},
		replace:    []string{memberName, resolverRuleID, resolverVPCID},
		attributes: []string{memberName, resolverAssociationID, resolverRuleID, resolverVPCID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := resolverParams(p, memberName, resolverRuleID, resolverVPCID)
			if err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, resolverService, "AssociateResolverRule", params)
			if err != nil {
				return "", nil, err
			}

			id, attrs := resolverAttributes(out, "ResolverRuleAssociation", resolverAssociationID)
			return id, attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				resolverService,
				"DisassociateResolverRule",
				pick(map[string]any{}, props(r.Properties), resolverRuleID, resolverVPCID),
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

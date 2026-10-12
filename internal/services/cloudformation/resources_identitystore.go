package cloudformation

import (
	"context"
	"maps"
	"strings"
)

const (
	identityStoreService   = "identitystore"
	identityStoreID        = "IdentityStoreId"
	identityGroupID        = "GroupId"
	identityMembershipID   = "MembershipId"
	identityDisplayName    = "DisplayName"
	identityAttributeValue = "AttributeValue"
)

func identityProperties(p props, required ...string) error {
	for k, v := range p {
		if v == nil {
			return validation("Identity Store property %s cannot be null", k)
		}
	}

	for _, k := range required {
		if !p.has(k) {
			return validation("Identity Store property %s is required", k)
		}
	}

	return nil
}

func init() {
	register("AWS::IdentityStore::Group", &resourceType{
		properties: []string{identityStoreID, identityDisplayName, memberDescription},
		replace:    []string{identityStoreID}, attributes: []string{identityGroupID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := identityProperties(p, identityStoreID, identityDisplayName); err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, identityStoreService, "CreateGroup", pick(map[string]any{}, p,
				identityStoreID, identityDisplayName, memberDescription))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, identityGroupID)
			return id + "|" + p.str(identityStoreID), map[string]any{identityGroupID: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := identityProperties(p, identityStoreID, identityDisplayName); err != nil {
				return nil, err
			}

			ops := []any{
				map[string]any{"AttributePath": "displayName", identityAttributeValue: p[identityDisplayName]},
				map[string]any{"AttributePath": "description", identityAttributeValue: p[memberDescription]},
			}

			_, err := c.call(ctx, identityStoreService, "UpdateGroup", map[string]any{
				identityStoreID: p[identityStoreID], identityGroupID: r.Attributes[identityGroupID], "Operations": ops,
			})
			return maps.Clone(r.Attributes), err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			id, _, _ := strings.Cut(r.Physical, "|")
			_, err := c.call(ctx, identityStoreService, "DeleteGroup", map[string]any{
				identityStoreID: r.Properties[identityStoreID], identityGroupID: id,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::IdentityStore::GroupMembership", &resourceType{
		properties: []string{identityStoreID, identityGroupID, "MemberId"},
		replace:    []string{identityStoreID, identityGroupID, "MemberId"}, attributes: []string{identityMembershipID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := identityProperties(p, identityStoreID, identityGroupID, "MemberId"); err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, identityStoreService, "CreateGroupMembership", pick(map[string]any{}, p,
				identityStoreID, identityGroupID, "MemberId"))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, identityMembershipID)
			return id + "|" + p.str(identityStoreID), map[string]any{identityMembershipID: id}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			id, _, _ := strings.Cut(r.Physical, "|")
			_, err := c.call(ctx, identityStoreService, "DeleteGroupMembership", map[string]any{
				identityStoreID: r.Properties[identityStoreID], identityMembershipID: id,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

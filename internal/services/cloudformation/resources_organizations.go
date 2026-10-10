package cloudformation

import (
	"context"
	"encoding/json"
)

func init() {
	register("AWS::Organizations::OrganizationalUnit", &resourceType{
		name:       memberName,
		properties: []string{memberName, propertyParentID, memberTags},
		attributes: []string{"Id", memberArn},
		replace:    []string{memberName, propertyParentID, memberTags},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberName:       c.nameOf(p, memberName, 128),
				propertyParentID: p.str(propertyParentID),
			}
			if ts := p.tags(); len(ts) > 0 {
				params[memberTags] = tagList(ts, memberKey, memberValue)
			}

			out, err := c.call(ctx, "organizations", "CreateOrganizationalUnit", params)
			if err != nil {
				return "", nil, err
			}

			ou, _ := out["OrganizationalUnit"].(map[string]any)
			id, _ := ou["Id"].(string)
			return id, map[string]any{"Id": id, memberArn: ou[memberArn]}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				"organizations",
				"DeleteOrganizationalUnit",
				map[string]any{"OrganizationalUnitId": r.Physical},
			)
			if gone(err, "OrganizationalUnitNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Organizations::Policy", &resourceType{
		name:       memberName,
		properties: []string{memberName, memberDescription, memberType, memberContent, "TargetIds", memberTags},
		attributes: []string{"Id", memberArn},
		replace:    []string{memberName, memberDescription, memberType, memberContent, "TargetIds", memberTags},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			content := p[memberContent]
			if _, ok := content.(string); !ok {
				data, err := json.Marshal(content)
				if err != nil {
					return "", nil, err
				}

				content = string(data)
			}

			params := map[string]any{
				memberName:        c.nameOf(p, memberName, 128),
				memberType:        p.str(memberType),
				memberContent:     content,
				memberDescription: p.str(memberDescription),
			}

			if ts := p.tags(); len(ts) > 0 {
				params[memberTags] = tagList(ts, memberKey, memberValue)
			}

			out, err := c.call(ctx, "organizations", "CreatePolicy", params)
			if err != nil {
				return "", nil, err
			}

			policy, _ := out["Policy"].(map[string]any)
			summary, _ := policy["PolicySummary"].(map[string]any)
			id, _ := summary["Id"].(string)
			for _, target := range p.list("TargetIds") {
				if _, err = c.call(
					ctx,
					"organizations",
					"AttachPolicy",
					map[string]any{memberPolicyID: id, "TargetId": target},
				); err != nil {
					return "", nil, err
				}
			}

			return id, map[string]any{"Id": id, memberArn: summary[memberArn]}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			for _, target := range props(r.Properties).list("TargetIds") {
				_, err := c.call(
					ctx,
					"organizations",
					"DetachPolicy",
					map[string]any{memberPolicyID: r.Physical, "TargetId": target},
				)
				if err != nil && !gone(err, "PolicyNotAttachedException", "PolicyNotFoundException") {
					return err
				}
			}

			_, err := c.call(ctx, "organizations", "DeletePolicy", map[string]any{memberPolicyID: r.Physical})
			if gone(err, "PolicyNotFoundException") {
				return nil
			}

			return err
		},
	})
}

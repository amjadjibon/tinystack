package cloudformation

import "context"

const (
	inspector2FilterType = "AWS::InspectorV2::Filter"
	inspector2Service    = "inspector2"
	inspector2Criteria   = "FilterCriteria"
	inspector2Action     = "FilterAction"
	inspector2Arn        = "FilterArn"
)

func inspector2Params(p props) map[string]any {
	params := map[string]any{
		memberName:         p[memberName],
		"Action":           p[inspector2Action],
		inspector2Criteria: p[inspector2Criteria],
	}

	for _, field := range []string{memberDescription} {
		if p.has(field) {
			params[field] = p[field]
		}
	}

	return params
}

// inspector2FilterReplaces replaces a filter only when its description is removed: UpdateFilter changes one in place
// but cannot clear it, as the model requires 1 to 512 characters.
func inspector2FilterReplaces(old, p props) bool {
	return old.has(memberDescription) && !p.has(memberDescription)
}

func init() {
	register(inspector2FilterType, &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberDescription,
			inspector2Action,
			inspector2Criteria,
			memberTags,
		},
		attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := inspector2Params(p)
			params[memberTags] = tagMap(p.tags())
			out, err := c.call(ctx, inspector2Service, "CreateFilter", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, memberArn)
			return arn, map[string]any{memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := inspector2Params(p)
			params[inspector2Arn] = r.Physical

			if _, err := c.call(ctx, inspector2Service, "UpdateFilter", params); err != nil {
				return nil, err
			}

			err := retag(ctx, c, r.Properties, p, &tagger{
				service: inspector2Service, tag: opTagResource, untag: opUntagResource,
				target: map[string]any{memberResourceArn: r.Physical},
			})
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, inspector2Service, "DeleteFilter", map[string]any{memberArn: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

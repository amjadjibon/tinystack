package cloudformation

import (
	"context"
	"maps"
)

const (
	xrayGroupType        = "AWS::XRay::Group"
	xraySamplingRuleType = "AWS::XRay::SamplingRule"
	memberGroupARN       = "GroupARN"
	memberRuleARN        = "RuleARN"
)

func xrayTagger(arn string) *tagger {
	return &tagger{
		service: "xray",
		tag:     "TagResource",
		untag:   "UntagResource",
		target:  map[string]any{memberResourceARN: arn},
		key:     memberKey,
		value:   memberValue,
	}
}

func samplingRuleName(p props) string {
	if r, ok := p["SamplingRule"].(map[string]any); ok {
		if name, ok := r["RuleName"].(string); ok {
			return name
		}
	}

	if r, ok := p["SamplingRuleRecord"].(map[string]any); ok {
		if inner, ok := r["SamplingRule"].(map[string]any); ok {
			if name, ok := inner["RuleName"].(string); ok {
				return name
			}
		}
	}

	if r, ok := p["SamplingRuleUpdate"].(map[string]any); ok {
		if name, ok := r["RuleName"].(string); ok {
			return name
		}
	}

	return ""
}

func xraySamplingRuleReplaces(old, p props) bool {
	return samplingRuleName(old) != samplingRuleName(p)
}

func init() {
	register(xrayGroupType, &resourceType{
		name: memberGroupName,
		properties: []string{
			memberGroupName,
			"FilterExpression",
			"InsightsConfiguration",
			memberTags,
		},
		replace:    []string{memberGroupName},
		attributes: []string{memberGroupARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberGroupName, "FilterExpression", "InsightsConfiguration")
			if p.has(memberTags) {
				params[memberTags] = p[memberTags]
			}

			res, err := c.call(ctx, "xray", "CreateGroup", params)
			if err != nil {
				return "", nil, err
			}

			grp, _ := res["Group"].(map[string]any)
			arn, _ := grp[memberGroupARN].(string)
			return arn, map[string]any{memberGroupARN: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{}, p, "FilterExpression", "InsightsConfiguration")
			params[memberGroupARN] = r.Physical
			if _, err := c.call(ctx, "xray", "UpdateGroup", params); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, xrayTagger(r.Physical)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "xray", "DeleteGroup", map[string]any{memberGroupARN: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register(xraySamplingRuleType, &resourceType{
		properties: []string{
			"SamplingRule",
			"SamplingRuleRecord",
			"SamplingRuleUpdate",
			memberTags,
		},
		attributes: []string{memberRuleARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{}
			if r, ok := p["SamplingRule"].(map[string]any); ok {
				params["SamplingRule"] = r
			} else if r, ok := p["SamplingRuleRecord"].(map[string]any); ok {
				if inner, ok := r["SamplingRule"].(map[string]any); ok {
					params["SamplingRule"] = inner
				}
			}

			if p.has(memberTags) {
				params[memberTags] = p[memberTags]
			}

			res, err := c.call(ctx, "xray", "CreateSamplingRule", params)
			if err != nil {
				return "", nil, err
			}

			rec, _ := res["SamplingRuleRecord"].(map[string]any)
			rule, _ := rec["SamplingRule"].(map[string]any)
			arn, _ := rule[memberRuleARN].(string)
			return arn, map[string]any{memberRuleARN: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			update := map[string]any{}
			if r, ok := p["SamplingRule"].(map[string]any); ok {
				update = maps.Clone(r)
			} else if r, ok := p["SamplingRuleUpdate"].(map[string]any); ok {
				update = maps.Clone(r)
			} else if r, ok := p["SamplingRuleRecord"].(map[string]any); ok {
				if inner, ok := r["SamplingRule"].(map[string]any); ok {
					update = maps.Clone(inner)
				}
			}

			delete(update, "Version")
			delete(update, "CreatedAt")
			delete(update, "ModifiedAt")
			update[memberRuleARN] = r.Physical
			if _, err := c.call(
				ctx,
				"xray",
				"UpdateSamplingRule",
				map[string]any{"SamplingRuleUpdate": update},
			); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, xrayTagger(r.Physical)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "xray", "DeleteSamplingRule", map[string]any{memberRuleARN: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

package cloudformation

import "context"

const (
	securityHubService         = "securityhub"
	securityHubFilters         = "Filters"
	securityHubGroup           = "GroupByAttribute"
	securityHubAutoControls    = "AutoEnableControls"
	securityHubSubscribed      = "SubscribedAt"
	securityHubCFNArn          = "ARN"
	securityHubArn             = "HubArn"
	securityHubGenerator       = "ControlFindingGenerator"
	securityHubDefaults        = "EnableDefaultStandards"
	securityHubStandardArn     = "StandardsArn"
	securityHubSubscriptionArn = "StandardsSubscriptionArn"
	securityHubInsightArn      = "InsightArn"
)

func init() {
	register("AWS::SecurityHub::Hub", &resourceType{
		properties: []string{securityHubDefaults, securityHubGenerator, securityHubAutoControls, memberTags},
		attributes: []string{securityHubCFNArn, securityHubSubscribed},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{}
			for _, field := range []string{securityHubDefaults, securityHubGenerator} {
				if p.has(field) {
					params[field] = p[field]
				}
			}

			if p.has(memberTags) {
				params[memberTags] = tagMap(p.tags())
			}

			if _, err := c.call(ctx, securityHubService, "EnableSecurityHub", params); err != nil {
				return "", nil, err
			}

			if p.has(securityHubAutoControls) {
				if _, err := c.call(
					ctx,
					securityHubService,
					"UpdateSecurityHubConfiguration",
					map[string]any{securityHubAutoControls: p[securityHubAutoControls]},
				); err != nil {
					return "", nil, err
				}
			}

			out, err := c.call(ctx, securityHubService, "DescribeHub", nil)
			if err != nil {
				return "", nil, err
			}

			value := outText(out, securityHubArn)
			return value, map[string]any{
				securityHubCFNArn:     value,
				securityHubSubscribed: out[securityHubSubscribed],
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{securityHubGenerator: "SECURITY_CONTROL", securityHubAutoControls: true}
			for _, field := range []string{securityHubGenerator, securityHubAutoControls} {
				if p.has(field) {
					params[field] = p[field]
				}
			}

			old := props(r.Properties)
			before, after := !old.has(securityHubDefaults) ||
				old.truth(securityHubDefaults), !p.has(securityHubDefaults) ||
				p.truth(securityHubDefaults)
			if before != after {
				if err := securityHubChangeDefaults(ctx, c, after); err != nil {
					return nil, err
				}
			}

			if _, err := c.call(ctx, securityHubService, "UpdateSecurityHubConfiguration", params); err != nil {
				return nil, err
			}

			err := retag(
				ctx,
				c,
				r.Properties,
				p,
				&tagger{
					service: securityHubService,
					tag:     opTagResource,
					untag:   opUntagResource,
					target:  map[string]any{memberResourceArn: r.Physical},
				},
			)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, _ *resource) error {
			_, err := c.call(ctx, securityHubService, "DisableSecurityHub", nil)
			if gone(err, "InvalidAccessException") {
				return nil
			}

			return err
		},
	})

	register("AWS::SecurityHub::Standard", &resourceType{
		properties: []string{securityHubStandardArn},
		attributes: []string{securityHubSubscriptionArn},
		replace:    []string{securityHubStandardArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				securityHubService,
				"BatchEnableStandards",
				map[string]any{
					"StandardsSubscriptionRequests": []any{
						map[string]any{securityHubStandardArn: p.str(securityHubStandardArn)},
					},
				},
			)
			if err != nil {
				return "", nil, err
			}

			subs, _ := out["StandardsSubscriptions"].([]any)
			value := ""
			if len(subs) > 0 {
				if sub, ok := subs[0].(map[string]any); ok {
					value = outText(sub, securityHubSubscriptionArn)
				}
			}

			return value, map[string]any{securityHubSubscriptionArn: value}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				securityHubService,
				"BatchDisableStandards",
				map[string]any{"StandardsSubscriptionArns": []any{r.Physical}},
			)
			if gone(err, "ResourceNotFoundException", "InvalidAccessException") {
				return nil
			}

			return err
		},
	})

	register("AWS::SecurityHub::Insight", &resourceType{
		name:       memberName,
		properties: []string{memberName, securityHubFilters, securityHubGroup},
		attributes: []string{securityHubInsightArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				securityHubService,
				"CreateInsight",
				map[string]any{
					memberName:         p.str(memberName),
					securityHubFilters: p[securityHubFilters],
					securityHubGroup:   p.str(securityHubGroup),
				},
			)
			if err != nil {
				return "", nil, err
			}

			value := outText(out, securityHubInsightArn)
			return value, map[string]any{securityHubInsightArn: value}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			_, err := c.call(
				ctx,
				securityHubService,
				"UpdateInsight",
				map[string]any{
					securityHubInsightArn: r.Physical,
					memberName:            p.str(memberName),
					securityHubFilters:    p[securityHubFilters],
					securityHubGroup:      p.str(securityHubGroup),
				},
			)
			return map[string]any{securityHubInsightArn: r.Physical}, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				securityHubService,
				"DeleteInsight",
				map[string]any{securityHubInsightArn: r.Physical},
			)
			if gone(err, "ResourceNotFoundException", "InvalidAccessException") {
				return nil
			}

			return err
		},
	})
}

// securityHubChangeDefaults uses the same catalogue as EnableSecurityHub, without
// replacing the hub or touching findings, insights, actions or nondefault standards.
func securityHubChangeDefaults(ctx context.Context, c *client, enabled bool) error {
	catalogue, err := c.call(ctx, securityHubService, "DescribeStandards", nil)
	if err != nil {
		return err
	}

	standards, _ := catalogue["Standards"].([]any)
	defaults := map[string]bool{}
	requests := []any{}
	for _, value := range standards {
		standard, _ := value.(map[string]any)
		if standard["EnabledByDefault"] == true {
			value := outText(standard, securityHubStandardArn)
			defaults[value] = true
			requests = append(requests, map[string]any{securityHubStandardArn: value})
		}
	}

	if enabled {
		_, err = c.call(
			ctx,
			securityHubService,
			"BatchEnableStandards",
			map[string]any{"StandardsSubscriptionRequests": requests},
		)
		return err
	}

	subscriptions, err := c.call(ctx, securityHubService, "GetEnabledStandards", nil)
	if err != nil {
		return err
	}

	values, _ := subscriptions["StandardsSubscriptions"].([]any)
	arns := []any{}
	for _, value := range values {
		sub, _ := value.(map[string]any)
		if defaults[outText(sub, securityHubStandardArn)] {
			arns = append(arns, outText(sub, securityHubSubscriptionArn))
		}
	}

	if len(arns) == 0 {
		return nil
	}

	_, err = c.call(ctx, securityHubService, "BatchDisableStandards", map[string]any{"StandardsSubscriptionArns": arns})
	return err
}

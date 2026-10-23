package cloudformation

import (
	"context"
)

const (
	accessAnalyzerType    = "AWS::AccessAnalyzer::Analyzer"
	serviceAccessAnalyzer = "accessanalyzer"

	propAnalyzerName   = "AnalyzerName"
	propArchiveRules   = "ArchiveRules"
	propAnalyzerConfig = "AnalyzerConfiguration"
	propRuleName       = "RuleName"
	propFilter         = "Filter"
	propProperty       = "Property"
	propEq             = "Eq"
	propNeq            = "Neq"
	propContains       = "Contains"
	propExists         = "Exists"
)

func accessAnalyzerTagger(arn string) *tagger {
	return &tagger{
		service: serviceAccessAnalyzer,
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: arn},
	}
}

func formatArchiveRules(raw any) []any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}

	var rules []any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}

		ruleName, _ := m[propRuleName].(string)
		filterMap := map[string]any{}

		switch f := m[propFilter].(type) {
		case []any:
			for _, entry := range f {
				fe, ok := entry.(map[string]any)
				if !ok {
					continue
				}

				prop, _ := fe[propProperty].(string)
				if prop == "" {
					continue
				}

				criterion := map[string]any{}
				if eq, ok := fe[propEq]; ok {
					criterion[propEq] = eq
				}

				if neq, ok := fe[propNeq]; ok {
					criterion[propNeq] = neq
				}

				if c, ok := fe[propContains]; ok {
					criterion[propContains] = c
				}

				if ex, ok := fe[propExists]; ok {
					criterion[propExists] = ex
				}

				filterMap[prop] = criterion
			}
		case map[string]any:
			filterMap = f
		}

		rules = append(rules, map[string]any{
			propRuleName: ruleName,
			propFilter:   filterMap,
		})
	}

	return rules
}

func init() {
	register(accessAnalyzerType, &resourceType{
		name: propAnalyzerName,
		properties: []string{
			propAnalyzerName,
			memberType,
			propArchiveRules,
			propAnalyzerConfig,
			memberTags,
		},
		attributes: []string{
			memberArn,
		},
		replace: []string{
			propAnalyzerName,
			memberType,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, propAnalyzerName, 255)
			params := map[string]any{
				propAnalyzerName: name,
				memberType:       p.str(memberType),
			}

			if p.has(propAnalyzerConfig) {
				params["Configuration"] = p[propAnalyzerConfig]
			}

			if p.has(propArchiveRules) {
				if rules := formatArchiveRules(p[propArchiveRules]); len(rules) > 0 {
					params[propArchiveRules] = rules
				}
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			out, err := c.call(ctx, serviceAccessAnalyzer, "CreateAnalyzer", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, memberArn)
			return name, map[string]any{memberArn: arn}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, serviceAccessAnalyzer, "DeleteAnalyzer", map[string]any{
				propAnalyzerName: r.Physical,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, propAnalyzerConfig) && p.has(propAnalyzerConfig) {
				_, err := c.call(ctx, serviceAccessAnalyzer, "UpdateAnalyzer", map[string]any{
					propAnalyzerName: r.Physical,
					"Configuration":  p[propAnalyzerConfig],
				})
				if err != nil {
					return nil, err
				}
			}

			if p.changed(r.Properties, propArchiveRules) {
				newRules := formatArchiveRules(p[propArchiveRules])
				oldRules := formatArchiveRules(r.Properties[propArchiveRules])

				for _, nr := range newRules {
					m := nr.(map[string]any)
					rName := m[propRuleName].(string)
					filter := m[propFilter]

					_, err := c.call(ctx, serviceAccessAnalyzer, "UpdateArchiveRule", map[string]any{
						propAnalyzerName: r.Physical,
						propRuleName:     rName,
						propFilter:       filter,
					})
					if gone(err, "ResourceNotFoundException") {
						_, err = c.call(ctx, serviceAccessAnalyzer, "CreateArchiveRule", map[string]any{
							propAnalyzerName: r.Physical,
							propRuleName:     rName,
							propFilter:       filter,
						})
					}

					if err != nil {
						return nil, err
					}
				}

				for _, or := range oldRules {
					m := or.(map[string]any)
					oName := m[propRuleName].(string)
					stillExists := false
					for _, nr := range newRules {
						nm := nr.(map[string]any)
						if nm[propRuleName].(string) == oName {
							stillExists = true
							break
						}
					}

					if !stillExists {
						_, err := c.call(ctx, serviceAccessAnalyzer, "DeleteArchiveRule", map[string]any{
							propAnalyzerName: r.Physical,
							propRuleName:     oName,
						})
						if err != nil && !gone(err, "ResourceNotFoundException") {
							return nil, err
						}
					}
				}
			}

			arn := outText(r.Attributes, memberArn)
			if err := retag(ctx, c, props(r.Properties), p, accessAnalyzerTagger(arn)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
	})
}

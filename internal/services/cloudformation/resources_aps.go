package cloudformation

import (
	"context"
	"strings"
)

const (
	apsWorkspaceType           = "AWS::APS::Workspace"
	apsRuleGroupsNamespaceType = "AWS::APS::RuleGroupsNamespace"
	serviceAPS                 = "amp"

	propAPSWorkspace            = "Workspace"
	propAPSWorkspaceID          = "WorkspaceId"
	propAPSAlias                = "Alias"
	propAPSKmsKeyArn            = "KmsKeyArn"
	propAPSLoggingConfiguration = "LoggingConfiguration"
	propAPSLogGroupArn          = "LogGroupArn"
	propAPSPrometheusEndpoint   = "PrometheusEndpoint"
	propAPSData                 = "Data"
)

func apsTagger(arn string) *tagger {
	return &tagger{
		service: serviceAPS,
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: arn},
	}
}

func init() {
	register(apsWorkspaceType, &resourceType{
		name: propAPSAlias,
		properties: []string{
			propAPSAlias,
			propAPSKmsKeyArn,
			propAPSLoggingConfiguration,
			memberTags,
		},
		attributes: []string{
			memberArn,
			propAPSWorkspaceID,
			propAPSPrometheusEndpoint,
		},
		replace: []string{
			propAPSKmsKeyArn,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{}
			if p.has(propAPSAlias) {
				params[propAPSAlias] = p.str(propAPSAlias)
			}

			if p.has(propAPSKmsKeyArn) {
				params[propAPSKmsKeyArn] = p.str(propAPSKmsKeyArn)
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			out, err := c.call(ctx, serviceAPS, "CreateWorkspace", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, "Arn")
			wsID := outText(out, propAPSWorkspaceID)

			if p.has(propAPSLoggingConfiguration) {
				logConfig, _ := p[propAPSLoggingConfiguration].(map[string]any)
				if logArn, ok := logConfig[propAPSLogGroupArn].(string); ok && logArn != "" {
					_, logErr := c.call(ctx, serviceAPS, "CreateLoggingConfiguration", map[string]any{
						propAPSWorkspaceID: wsID,
						propAPSLogGroupArn: logArn,
					})
					if logErr != nil {
						return "", nil, logErr
					}
				}
			}

			desc, err := c.call(ctx, serviceAPS, "DescribeWorkspace", map[string]any{
				propAPSWorkspaceID: wsID,
			})
			if err != nil {
				return "", nil, err
			}

			endpoint := outText(desc, "Workspace", propAPSPrometheusEndpoint)
			attrs := map[string]any{
				memberArn:                 arn,
				propAPSWorkspaceID:        wsID,
				propAPSPrometheusEndpoint: endpoint,
			}

			return arn, attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			wsID := outText(r.Attributes, propAPSWorkspaceID)
			if wsID == "" {
				wsID = r.Physical[strings.LastIndex(r.Physical, "/")+1:]
			}

			_, err := c.call(ctx, serviceAPS, "DeleteWorkspace", map[string]any{
				propAPSWorkspaceID: wsID,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			wsID := outText(r.Attributes, propAPSWorkspaceID)
			if wsID == "" {
				wsID = r.Physical[strings.LastIndex(r.Physical, "/")+1:]
			}

			if p.changed(props(r.Properties), propAPSAlias) {
				_, err := c.call(ctx, serviceAPS, "UpdateWorkspaceAlias", map[string]any{
					propAPSWorkspaceID: wsID,
					propAPSAlias:       p.str(propAPSAlias),
				})
				if err != nil {
					return nil, err
				}
			}

			if p.changed(props(r.Properties), propAPSLoggingConfiguration) {
				oldLog, _ := props(r.Properties)[propAPSLoggingConfiguration].(map[string]any)
				newLog, _ := p[propAPSLoggingConfiguration].(map[string]any)
				switch {
				case newLog != nil && oldLog == nil:
					_, err := c.call(ctx, serviceAPS, "CreateLoggingConfiguration", map[string]any{
						propAPSWorkspaceID: wsID,
						propAPSLogGroupArn: newLog[propAPSLogGroupArn],
					})
					if err != nil {
						return nil, err
					}
				case newLog != nil && oldLog != nil:
					_, err := c.call(ctx, serviceAPS, "UpdateLoggingConfiguration", map[string]any{
						propAPSWorkspaceID: wsID,
						propAPSLogGroupArn: newLog[propAPSLogGroupArn],
					})
					if err != nil {
						return nil, err
					}
				case newLog == nil && oldLog != nil:
					_, err := c.call(ctx, serviceAPS, "DeleteLoggingConfiguration", map[string]any{
						propAPSWorkspaceID: wsID,
					})
					if err != nil && !gone(err, "ResourceNotFoundException") {
						return nil, err
					}
				}
			}

			if err := retag(ctx, c, props(r.Properties), p, apsTagger(r.Physical)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
	})

	register(apsRuleGroupsNamespaceType, &resourceType{
		name: memberName,
		properties: []string{
			propAPSWorkspace,
			memberName,
			propAPSData,
			memberTags,
		},
		attributes: []string{
			memberArn,
		},
		replace: []string{
			propAPSWorkspace,
			memberName,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			ws := p.str(propAPSWorkspace)
			wsID := ws
			if strings.HasPrefix(ws, "arn:") {
				wsID = ws[strings.LastIndex(ws, "/")+1:]
			}

			name := p.str(memberName)
			params := map[string]any{
				propAPSWorkspaceID: wsID,
				memberName:         name,
				propAPSData:        p.str(propAPSData),
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			out, err := c.call(ctx, serviceAPS, "CreateRuleGroupsNamespace", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, "Arn")
			attrs := map[string]any{
				memberArn: arn,
			}

			return arn, attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			ws := props(r.Properties).str(propAPSWorkspace)
			wsID := ws
			if strings.HasPrefix(ws, "arn:") {
				wsID = ws[strings.LastIndex(ws, "/")+1:]
			}

			name := props(r.Properties).str(memberName)
			_, err := c.call(ctx, serviceAPS, "DeleteRuleGroupsNamespace", map[string]any{
				propAPSWorkspaceID: wsID,
				memberName:         name,
			})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			ws := props(r.Properties).str(propAPSWorkspace)
			wsID := ws
			if strings.HasPrefix(ws, "arn:") {
				wsID = ws[strings.LastIndex(ws, "/")+1:]
			}

			name := props(r.Properties).str(memberName)
			if p.changed(props(r.Properties), propAPSData) {
				_, err := c.call(ctx, serviceAPS, "PutRuleGroupsNamespace", map[string]any{
					propAPSWorkspaceID: wsID,
					memberName:         name,
					propAPSData:        p.str(propAPSData),
				})
				if err != nil {
					return nil, err
				}
			}

			if err := retag(ctx, c, props(r.Properties), p, apsTagger(r.Physical)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
	})
}

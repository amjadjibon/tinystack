package cloudformation

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

const (
	verifiedpermissionsPolicyStoreType = "AWS::VerifiedPermissions::PolicyStore"
	verifiedpermissionsPolicyType      = "AWS::VerifiedPermissions::Policy"

	memberPolicyStoreId      = "PolicyStoreId"
	memberPolicyId           = "PolicyId"
	memberPolicyType         = "PolicyType"
	memberSchema             = "Schema"
	memberCedarJson          = "CedarJson"
	memberStatic             = "Static"
	memberTemplateLinked     = "TemplateLinked"
	memberValidationSettings = "ValidationSettings"
)

func init() {
	register(verifiedpermissionsPolicyStoreType, &resourceType{
		properties: []string{
			memberValidationSettings,
			memberDescription,
			memberSchema,
			memberTags,
		},
		attributes: []string{memberArn, memberPolicyStoreId},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{}
			pick(params, p, memberValidationSettings, memberDescription)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			res, err := c.call(ctx, "verifiedpermissions", "CreatePolicyStore", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(res, memberPolicyStoreId)
			arn := outText(res, memberArn)

			if p.has(memberSchema) {
				schemaMap, _ := p[memberSchema].(map[string]any)
				if cedarJSON, ok := schemaMap[memberCedarJson].(string); ok && cedarJSON != "" {
					_, err := c.call(ctx, "verifiedpermissions", "PutSchema", map[string]any{
						memberPolicyStoreId: id,
						memberDefinition: map[string]any{
							memberCedarJson: cedarJSON,
						},
					})
					if err != nil {
						return "", nil, err
					}
				}
			}

			return id, map[string]any{memberPolicyStoreId: id, memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.changed(r.Properties, memberValidationSettings) || p.changed(r.Properties, memberDescription) {
				params := map[string]any{memberPolicyStoreId: r.Physical}
				pick(params, p, memberValidationSettings, memberDescription)
				if _, err := c.call(ctx, "verifiedpermissions", "UpdatePolicyStore", params); err != nil {
					return nil, err
				}
			}

			if p.changed(r.Properties, memberSchema) && p.has(memberSchema) {
				schemaMap, _ := p[memberSchema].(map[string]any)
				if cedarJSON, ok := schemaMap[memberCedarJson].(string); ok && cedarJSON != "" {
					_, err := c.call(ctx, "verifiedpermissions", "PutSchema", map[string]any{
						memberPolicyStoreId: r.Physical,
						memberDefinition: map[string]any{
							memberCedarJson: cedarJSON,
						},
					})
					if err != nil {
						return nil, err
					}
				}
			}

			arn := outText(r.Attributes, memberArn)
			if err := retag(ctx, c, props(r.Properties), p, &tagger{
				target:  map[string]any{memberResourceArn: arn},
				service: "verifiedpermissions",
				tag:     opTagResource,
				untag:   opUntagResource,
			}); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				"verifiedpermissions",
				"DeletePolicyStore",
				map[string]any{memberPolicyStoreId: r.Physical},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register(verifiedpermissionsPolicyType, &resourceType{
		properties: []string{
			memberPolicyStoreId,
			memberDefinition,
		},
		replace: []string{
			memberPolicyStoreId,
		},
		attributes: []string{memberPolicyId, memberPolicyStoreId, memberPolicyType},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			def, _ := p[memberDefinition].(map[string]any)
			if def == nil {
				return "", nil, awserror.Validation("Definition is required")
			}

			if def[memberTemplateLinked] != nil {
				return "", nil, fmt.Errorf("template-linked policies are not supported")
			}

			if def[memberStatic] == nil {
				return "", nil, fmt.Errorf("static policy definition is required")
			}

			params := map[string]any{
				memberPolicyStoreId: p.str(memberPolicyStoreId),
				memberDefinition:    def,
			}

			res, err := c.call(ctx, "verifiedpermissions", "CreatePolicy", params)
			if err != nil {
				return "", nil, err
			}

			policyID := outText(res, memberPolicyId)
			storeID := outText(res, memberPolicyStoreId)
			polType := outText(res, memberPolicyType)

			return policyID, map[string]any{
				memberPolicyId:      policyID,
				memberPolicyStoreId: storeID,
				memberPolicyType:    polType,
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			def, _ := p[memberDefinition].(map[string]any)
			if def == nil {
				return nil, awserror.Validation("Definition is required")
			}

			if def[memberTemplateLinked] != nil {
				return nil, fmt.Errorf("template-linked policies are not supported")
			}

			if def[memberStatic] == nil {
				return nil, fmt.Errorf("static policy definition is required")
			}

			if p.changed(r.Properties, memberDefinition) {
				storeID := outText(r.Attributes, memberPolicyStoreId)
				if storeID == "" {
					storeID = p.str(memberPolicyStoreId)
				}

				params := map[string]any{
					memberPolicyStoreId: storeID,
					memberPolicyId:      r.Physical,
					memberDefinition:    def,
				}

				if _, err := c.call(ctx, "verifiedpermissions", "UpdatePolicy", params); err != nil {
					return nil, err
				}
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			storeID := outText(r.Attributes, memberPolicyStoreId)
			if storeID == "" {
				storeID = props(r.Properties).str(memberPolicyStoreId)
			}

			_, err := c.call(
				ctx,
				"verifiedpermissions",
				"DeletePolicy",
				map[string]any{
					memberPolicyStoreId: storeID,
					memberPolicyId:      r.Physical,
				},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

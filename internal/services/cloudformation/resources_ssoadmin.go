package cloudformation

import (
	"context"
	"maps"
	"slices"
	"strings"
)

const (
	ssoService       = "ssoadmin"
	ssoInstanceARN   = "InstanceArn"
	ssoPermissionARN = "PermissionSetArn"
	ssoRelayState    = "RelayStateType"
	ssoDuration      = "SessionDuration"
	ssoManaged       = "ManagedPolicies"
	ssoInline        = "InlinePolicy"
	ssoTargetID      = "TargetId"
)

func ssoProperties(p props, required ...string) error {
	for _, k := range []string{"CustomerManagedPolicyReferences", "PermissionsBoundary"} {
		if p.has(k) {
			return validation("%s is not supported in the records-only permission set slice", k)
		}
	}

	for k, v := range p {
		if v == nil {
			return validation("SSO property %s cannot be null", k)
		}
	}

	for _, k := range required {
		if !p.has(k) {
			return validation("SSO property %s is required", k)
		}
	}

	if p.has(ssoManaged) {
		entries, ok := p[ssoManaged].([]any)
		if !ok || len(entries) > 25 {
			return validation("ManagedPolicies must be a list of at most 25 ARNs")
		}

		seen := map[string]bool{}
		for _, entry := range entries {
			arn, ok := entry.(string)
			if !ok || seen[arn] {
				return validation("ManagedPolicies must contain unique ARN strings")
			}

			seen[arn] = true
		}
	}

	return nil
}

func ssoTarget(instance, permission string) map[string]any {
	return map[string]any{ssoInstanceARN: instance, ssoPermissionARN: permission}
}

func configureSSOPermission(ctx context.Context, c *client, p props, arn string) error {
	target := ssoTarget(p.str(ssoInstanceARN), arn)
	if p.has(ssoInline) {
		params := maps.Clone(target)
		params[ssoInline] = p.document(ssoInline)
		if _, err := c.call(ctx, ssoService, "PutInlinePolicyToPermissionSet", params); err != nil {
			return err
		}
	} else if _, err := c.call(ctx, ssoService, "DeleteInlinePolicyFromPermissionSet", target); err != nil {
		return err
	}

	out, err := c.call(ctx, ssoService, "ListManagedPoliciesInPermissionSet", target)
	if err != nil {
		return err
	}

	existing := []string{}
	entries, _ := out["AttachedManagedPolicies"].([]any)
	for _, entry := range entries {
		policy, _ := entry.(map[string]any)
		existing = append(existing, outText(policy, "Arn"))
	}

	desired := p.strings(ssoManaged)
	for _, policy := range existing {
		if !slices.Contains(desired, policy) {
			params := maps.Clone(target)
			params["ManagedPolicyArn"] = policy
			if _, err = c.call(ctx, ssoService, "DetachManagedPolicyFromPermissionSet", params); err != nil {
				return err
			}
		}
	}

	for _, policy := range desired {
		if !slices.Contains(existing, policy) {
			params := maps.Clone(target)
			params["ManagedPolicyArn"] = policy
			if _, err = c.call(ctx, ssoService, "AttachManagedPolicyToPermissionSet", params); err != nil {
				return err
			}
		}
	}

	return nil
}

func init() {
	register("AWS::SSO::PermissionSet", &resourceType{
		properties: []string{
			ssoInstanceARN, "Name", memberDescription, ssoDuration, ssoRelayState, ssoInline, ssoManaged,
			memberTags, "CustomerManagedPolicyReferences", "PermissionsBoundary",
		},
		replace: []string{ssoInstanceARN, "Name"}, attributes: []string{ssoPermissionARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := ssoProperties(p, ssoInstanceARN, "Name"); err != nil {
				return "", nil, err
			}

			params := pick(map[string]any{}, p, ssoInstanceARN, "Name", memberDescription, ssoDuration, memberTags)
			if p.has(ssoRelayState) {
				params["RelayState"] = p[ssoRelayState]
			}

			out, err := c.call(ctx, ssoService, "CreatePermissionSet", params)
			if err != nil {
				return "", nil, err
			}

			permission, _ := out["PermissionSet"].(map[string]any)
			arn := outText(permission, ssoPermissionARN)
			if err = configureSSOPermission(ctx, c, p, arn); err != nil {
				// A failed compound create must not strand a permission set outside the stack.
				_, _ = c.call(ctx, ssoService, "DeletePermissionSet", ssoTarget(p.str(ssoInstanceARN), arn))
				return "", nil, err
			}

			return arn + "|" + p.str(ssoInstanceARN), map[string]any{ssoPermissionARN: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := ssoProperties(p, ssoInstanceARN, "Name"); err != nil {
				return nil, err
			}

			for _, k := range []string{memberDescription, ssoRelayState} {
				if _, was := r.Properties[k]; was && !p.has(k) {
					return nil, validation("Removing %s is unsupported; supply an explicit value", k)
				}
			}

			arn, _ := r.Attributes[ssoPermissionARN].(string)
			params := pick(ssoTarget(p.str(ssoInstanceARN), arn), p, memberDescription, ssoDuration)
			if !p.has(ssoDuration) {
				params[ssoDuration] = "PT1H"
			}

			if p.has(ssoRelayState) {
				params["RelayState"] = p[ssoRelayState]
			}

			if _, err := c.call(ctx, ssoService, "UpdatePermissionSet", params); err != nil {
				return nil, err
			}

			if err := configureSSOPermission(ctx, c, p, arn); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{
				service: ssoService, tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
				target: map[string]any{ssoInstanceARN: p[ssoInstanceARN], "ResourceArn": arn},
			}); err != nil {
				return nil, err
			}

			return maps.Clone(r.Attributes), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			arn, _, _ := strings.Cut(r.Physical, "|")
			_, err := c.call(
				ctx,
				ssoService,
				"DeletePermissionSet",
				ssoTarget(props(r.Properties).str(ssoInstanceARN), arn),
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	assignmentProperties := []string{
		ssoInstanceARN,
		ssoPermissionARN,
		"PrincipalId",
		"PrincipalType",
		ssoTargetID,
		"TargetType",
	}

	register("AWS::SSO::Assignment", &resourceType{
		properties: assignmentProperties, replace: assignmentProperties,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := ssoProperties(p, assignmentProperties...); err != nil {
				return "", nil, err
			}

			_, err := c.call(
				ctx,
				ssoService,
				"CreateAccountAssignment",
				pick(map[string]any{}, p, assignmentProperties...),
			)
			if err != nil {
				return "", nil, err
			}

			parts := make([]string, 0, len(assignmentProperties))
			for _, k := range assignmentProperties {
				parts = append(parts, p.str(k))
			}

			return strings.Join(parts, "|"), map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				ssoService,
				"DeleteAccountAssignment",
				pick(map[string]any{}, props(r.Properties), assignmentProperties...),
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

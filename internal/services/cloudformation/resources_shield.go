package cloudformation

import (
	"context"
	"slices"
	"strings"
)

const (
	shieldGroupID      = "ProtectionGroupId"
	shieldService      = "shield"
	shieldProtectionID = "ProtectionId"
	shieldPattern      = "Pattern"
	shieldAccountID    = "AccountId"
	shieldLogBucket    = "LogBucket"
)

func shieldTagger(value string) *tagger {
	return &tagger{
		service: shieldService,
		key:     memberKey, value: "Value",
		tag:    opTagResource,
		untag:  opUntagResource,
		target: map[string]any{"ResourceARN": value},
	}
}

func shieldHealth(ctx context.Context, c *client, id string, old, p props) error {
	before, after := old.strings("HealthCheckArns"), p.strings("HealthCheckArns")
	for _, h := range before {
		if !slices.Contains(after, h) {
			if _, err := c.call(
				ctx,
				shieldService,
				"DisassociateHealthCheck",
				map[string]any{shieldProtectionID: id, "HealthCheckArn": h},
			); err != nil {
				return err
			}
		}
	}

	for _, h := range after {
		if !slices.Contains(before, h) {
			if _, err := c.call(
				ctx,
				shieldService,
				"AssociateHealthCheck",
				map[string]any{shieldProtectionID: id, "HealthCheckArn": h},
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func shieldGroupParams(p props) map[string]any {
	out := map[string]any{}
	for _, field := range []string{shieldGroupID, "Aggregation", shieldPattern, "Members", wireResourceType} {
		if p.has(field) {
			out[field] = p[field]
		}
	}

	return out
}

func init() {
	register("AWS::Shield::Protection", &resourceType{
		properties: []string{memberName, memberResourceArn, "HealthCheckArns", memberTags},
		attributes: []string{"ProtectionArn", shieldProtectionID}, replace: []string{memberName, memberResourceArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				shieldService,
				"CreateProtection",
				map[string]any{
					memberName:        p.str(memberName),
					memberResourceArn: p.str(memberResourceArn),
					memberTags:        tagList(p.tags(), memberKey, "Value"),
				},
			)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, shieldProtectionID)
			if healthErr := shieldHealth(ctx, c, id, nil, p); healthErr != nil {
				return "", nil, healthErr
			}

			out, err = c.call(ctx, shieldService, "DescribeProtection", map[string]any{shieldProtectionID: id})
			if err != nil {
				return "", nil, err
			}

			protection, _ := out["Protection"].(map[string]any)
			value := outText(protection, "ProtectionArn")
			return value, map[string]any{"ProtectionArn": value, shieldProtectionID: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := shieldHealth(ctx, c, outText(r.Attributes, shieldProtectionID), r.Properties, p); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, shieldTagger(r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				shieldService,
				"DeleteProtection",
				map[string]any{shieldProtectionID: outText(r.Attributes, shieldProtectionID)},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Shield::ProtectionGroup", &resourceType{
		properties: []string{
			shieldGroupID,
			"Aggregation",
			shieldPattern,
			"Members",
			wireResourceType,
			memberTags,
		},
		attributes: []string{"ProtectionGroupArn"},
		replace:    []string{shieldGroupID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := shieldGroupParams(p)
			params[memberTags] = tagList(p.tags(), memberKey, "Value")
			if _, err := c.call(ctx, shieldService, "CreateProtectionGroup", params); err != nil {
				return "", nil, err
			}

			out, err := c.call(
				ctx,
				shieldService,
				"DescribeProtectionGroup",
				map[string]any{shieldGroupID: p.str(shieldGroupID)},
			)
			if err != nil {
				return "", nil, err
			}

			group, _ := out["ProtectionGroup"].(map[string]any)
			value := outText(group, "ProtectionGroupArn")
			return value, map[string]any{"ProtectionGroupArn": value}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, err := c.call(ctx, shieldService, "UpdateProtectionGroup", shieldGroupParams(p)); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, shieldTagger(r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				shieldService,
				"DeleteProtectionGroup",
				map[string]any{shieldGroupID: r.Properties[shieldGroupID]},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Shield::DRTAccess", &resourceType{
		properties: []string{memberRoleArn, "LogBucketList"}, attributes: []string{shieldAccountID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := shieldDRT(ctx, c, nil, p); err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, shieldService, "DescribeSubscription", nil)
			if err != nil {
				return "", nil, err
			}

			sub, _ := out["Subscription"].(map[string]any)
			account := strings.Split(outText(sub, "SubscriptionArn"), ":")[4]
			return account, map[string]any{shieldAccountID: account}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, shieldDRT(ctx, c, r.Properties, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			for _, b := range props(r.Properties).strings("LogBucketList") {
				if _, err := c.call(
					ctx,
					shieldService,
					"DisassociateDRTLogBucket",
					map[string]any{shieldLogBucket: b},
				); err != nil {
					return err
				}
			}

			_, err := c.call(ctx, shieldService, "DisassociateDRTRole", nil)
			return err
		},
	})
	register("AWS::Shield::ProactiveEngagement", &resourceType{
		properties: []string{
			"EmergencyContactList",
			"ProactiveEngagementStatus",
		},
		attributes: []string{shieldAccountID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := shieldProactive(ctx, c, p, true); err != nil {
				return "", nil, err
			}

			out, err := c.call(ctx, shieldService, "DescribeSubscription", nil)
			if err != nil {
				return "", nil, err
			}

			sub, _ := out["Subscription"].(map[string]any)
			account := strings.Split(outText(sub, "SubscriptionArn"), ":")[4]
			return account, map[string]any{shieldAccountID: account}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, shieldProactive(ctx, c, p, false)
		},
		remove: func(ctx context.Context, c *client, _ *resource) error {
			_, err := c.call(ctx, shieldService, "DisableProactiveEngagement", nil)
			if gone(err, "InvalidOperationException") {
				return nil
			}

			return err
		},
	})
}

func shieldDRT(ctx context.Context, c *client, old, p props) error {
	if _, err := c.call(
		ctx,
		shieldService,
		"AssociateDRTRole",
		map[string]any{memberRoleArn: p.str(memberRoleArn)},
	); err != nil {
		return err
	}

	before, after := old.strings("LogBucketList"), p.strings("LogBucketList")
	for _, b := range before {
		if !slices.Contains(after, b) {
			if _, err := c.call(
				ctx,
				shieldService,
				"DisassociateDRTLogBucket",
				map[string]any{shieldLogBucket: b},
			); err != nil {
				return err
			}
		}
	}

	for _, b := range after {
		if !slices.Contains(before, b) {
			if _, err := c.call(
				ctx,
				shieldService,
				"AssociateDRTLogBucket",
				map[string]any{shieldLogBucket: b},
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func shieldProactive(ctx context.Context, c *client, p props, create bool) error {
	status := p.str("ProactiveEngagementStatus")
	if status != "ENABLED" && status != batchDisabled {
		return validation("ProactiveEngagementStatus must be ENABLED or DISABLED")
	}

	op := "UpdateEmergencyContactSettings"
	if create {
		op = "AssociateProactiveEngagementDetails"
	}

	if !create && status == batchDisabled {
		if _, err := c.call(
			ctx,
			shieldService,
			"DisableProactiveEngagement",
			nil,
		); err != nil &&
			!gone(err, "InvalidOperationException") {
			return err
		}
	}

	if _, err := c.call(
		ctx,
		shieldService,
		op,
		map[string]any{"EmergencyContactList": p["EmergencyContactList"]},
	); err != nil {
		return err
	}

	op = "EnableProactiveEngagement"
	if status == batchDisabled {
		op = "DisableProactiveEngagement"
	}

	_, err := c.call(ctx, shieldService, op, nil)
	if gone(err, "InvalidOperationException") {
		return nil
	}

	return err
}

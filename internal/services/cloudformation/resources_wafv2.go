package cloudformation

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	serviceWAF    = "wafv2"
	wafScope      = "Scope"
	wafID         = "Id"
	wafLock       = "LockToken"
	wafRules      = "Rules"
	wafVisibility = "VisibilityConfig"
	wafDefault    = "DefaultAction"
	wafFamily     = "IPAddressVersion"
	wafAddresses  = "Addresses"
	wafCapacity   = "Capacity"
	wafACLArn     = "WebACLArn"
)

func init() {
	register("AWS::WAFv2::WebACL", wafResourceType("WebACL"))
	register("AWS::WAFv2::IPSet", wafResourceType("IPSet"))
	register("AWS::WAFv2::RuleGroup", wafResourceType("RuleGroup"))
	register("AWS::WAFv2::WebACLAssociation", &resourceType{
		properties: []string{memberResourceArn, wafACLArn}, replace: []string{memberResourceArn, wafACLArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberResourceArn, wafACLArn)
			if _, err := c.call(ctx, serviceWAF, "AssociateWebACL", params); err != nil {
				return "", nil, err
			}
			// Keep the target in the physical identity so replacing a target
			// with the same ACL still schedules cleanup of the old association.
			return p.str(memberResourceArn) + "|" + p.str(wafACLArn), map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			p := props(r.Properties)
			out, err := c.call(
				ctx,
				serviceWAF,
				"GetWebACLForResource",
				map[string]any{memberResourceArn: p[memberResourceArn]},
			)
			if gone(err, "WAFNonexistentItemException") {
				return nil
			}
			if err != nil {
				return err
			}
			acl, _ := out["WebACL"].(map[string]any)
			// Replacement can associate a new ACL with the same target before cleanup.
			// Never remove the replacement's association while deleting the old record.
			if outText(acl, "ARN") != p.str(wafACLArn) {
				return nil
			}
			_, err = c.call(
				ctx,
				serviceWAF,
				"DisassociateWebACL",
				map[string]any{memberResourceArn: p[memberResourceArn]},
			)
			return err
		},
	})
}

func wafResourceType(kind string) *resourceType {
	properties := []string{memberName, wafScope, memberDescription, memberTags}
	immutable := []string{memberName, wafScope}
	mutable := []string{memberDescription}
	if kind == "IPSet" {
		properties = append(properties, wafFamily, wafAddresses)
		immutable = append(immutable, wafFamily) // WAF has no API to change the family.
		mutable = append(mutable, wafAddresses)
	} else {
		properties = append(properties, wafRules, wafVisibility)
		mutable = append(mutable, wafRules, wafVisibility)
		if kind == "WebACL" {
			properties = append(properties, wafDefault)
			mutable = append(mutable, wafDefault)
		} else {
			properties = append(properties, wafCapacity)
			immutable = append(immutable, wafCapacity)
		}
	}
	return &resourceType{
		name: memberName, properties: properties, replace: immutable, attributes: []string{memberArn, wafID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if _, err := wafTags(p); err != nil {
				return "", nil, err
			}
			params := pick(map[string]any{}, p, properties...)
			if !p.has(memberName) {
				params[memberName] = c.generate(128)
			}
			out, err := c.call(ctx, serviceWAF, "Create"+kind, params)
			if err != nil {
				return "", nil, err
			}
			summary, _ := out["Summary"].(map[string]any)
			name, identifier := outText(summary, memberName), outText(summary, wafID)
			physical := name + "|" + identifier + "|" + p.str(wafScope)
			attributes := map[string]any{memberArn: summary["ARN"], wafID: identifier}
			return physical, attributes, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, err := wafTags(p); err != nil {
				return nil, err
			}
			identity, err := wafIdentity(r.Physical)
			if err != nil {
				return nil, err
			}
			before, err := c.call(ctx, serviceWAF, "Get"+kind, identity)
			if err != nil {
				return nil, err
			}
			params := pick(maps.Clone(identity), p, mutable...)
			params[wafLock] = before[wafLock]
			if _, err = c.call(ctx, serviceWAF, "Update"+kind, params); err != nil {
				return nil, err
			}
			if err = wafRetag(ctx, c, r, p); err != nil {
				// A failed handler is not published by the runner. Restore the live
				// configuration we changed, after fetching its new optimistic token.
				restoreErr := wafRestore(context.WithoutCancel(ctx), c, kind, identity, before, mutable)
				return nil, wafCompensation(err, restoreErr, "configuration")
			}
			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			identity, err := wafIdentity(r.Physical)
			if err != nil {
				return err
			}
			current, err := c.call(ctx, serviceWAF, "Get"+kind, identity)
			if gone(err, "WAFNonexistentItemException") {
				return nil
			}
			if err != nil {
				return err
			}
			identity[wafLock] = current[wafLock]
			_, err = c.call(ctx, serviceWAF, "Delete"+kind, identity)
			if gone(err, "WAFNonexistentItemException") {
				return nil
			}
			return err
		},
	}
}

func wafIdentity(physical string) (map[string]any, error) {
	parts := strings.Split(physical, "|")
	if len(parts) != 3 {
		return nil, validation("Invalid WAF physical identifier")
	}
	return map[string]any{memberName: parts[0], wafID: parts[1], wafScope: parts[2]}, nil
}

func wafRestore(ctx context.Context, c *client, kind string, identity, before map[string]any, mutable []string) error {
	current, err := c.call(ctx, serviceWAF, "Get"+kind, identity)
	if err != nil {
		return err
	}
	model, _ := before[kind].(map[string]any)
	params := pick(maps.Clone(identity), props(model), mutable...)
	params[wafLock] = current[wafLock]
	_, err = c.call(ctx, serviceWAF, "Update"+kind, params)
	return err
}

func wafCompensation(err, restoreErr error, what string) error {
	if restoreErr == nil {
		return err
	}
	e := awserror.From(err)
	e.Message += "; restoring WAF " + what + " failed: " + restoreErr.Error()
	return e
}

// CloudFormation tags are strictly a list. Preserve unsupported members for
// explicit rejection instead of silently converting them into successful tags.
func wafTags(p props) (map[string]string, error) {
	out := map[string]string{}
	if !p.has(memberTags) {
		return out, nil
	}
	values, ok := p[memberTags].([]any)
	if !ok {
		return nil, validation("WAF Tags must be a Key/Value array")
	}
	for _, v := range values {
		t, ok := v.(map[string]any)
		key, keyOK := t[memberKey].(string)
		value, valueOK := t[memberValue].(string)
		if !ok || len(t) != 2 || !keyOK || !valueOK {
			return nil, validation("WAF Tags require only string Key and Value")
		}
		if _, exists := out[key]; exists {
			return nil, validation("WAF Tags require unique keys")
		}
		out[key] = value
	}
	if err := tagging.CheckSet(out); err != nil {
		return nil, validation("%s", err.Error())
	}
	return out, nil
}

func wafRetag(ctx context.Context, c *client, r *resource, p props) error {
	next, err := wafTags(p)
	if err != nil {
		return err
	}
	old, err := wafTags(props(r.Properties))
	if err != nil {
		return err
	}
	set, removed := tagChanges(old, next)
	target := r.Attributes[memberArn]
	restore := map[string]string{}
	if len(removed) > 0 && len(set) > 0 {
		live, e := c.call(ctx, serviceWAF, "ListTagsForResource", map[string]any{memberResourceARN: target})
		if e != nil {
			return e
		}
		info, _ := live["TagInfoForResource"].(map[string]any)
		values, _ := info["TagList"].([]any)
		tags := props{memberTags: values}.tags()
		for _, key := range removed {
			k, _ := key.(string)
			if v, exists := tags[k]; exists {
				restore[k] = v
			}
		}
	}
	if len(removed) > 0 {
		if _, err = c.call(
			ctx,
			serviceWAF,
			opUntagResource,
			map[string]any{memberResourceARN: target, "TagKeys": removed},
		); err != nil {
			return err
		}
	}
	if len(set) > 0 {
		_, err = c.call(
			ctx,
			serviceWAF,
			opTagResource,
			map[string]any{memberResourceARN: target, memberTags: tagList(set, memberKey, memberValue)},
		)
		if err != nil && len(restore) > 0 {
			_, restoreErr := c.call(
				context.WithoutCancel(ctx),
				serviceWAF,
				opTagResource,
				map[string]any{memberResourceARN: target, memberTags: tagList(restore, memberKey, memberValue)},
			)
			return wafCompensation(err, restoreErr, "removed tags")
		}
	}
	return err
}

// Ref differs from this association's physical lifecycle identity. The ARN was
// validated by AssociateWebACL before the resource was published.
func wafAssociationRef(p props) string {
	parts := strings.Split(p.str(wafACLArn), "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2] + "|" + parts[len(parts)-1] + "|REGIONAL"
}

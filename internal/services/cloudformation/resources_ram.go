package cloudformation

import (
	"context"
	"errors"
)

const (
	ramResourceShareType = "AWS::RAM::ResourceShare"
	propResourceShareArn = "ResourceShareArn"
	propPrincipals       = "Principals"
	propResourceArns     = "ResourceArns"
	propAllowExternal    = "AllowExternalPrincipals"
	propPermissionArns   = "PermissionArns"
	propSources          = "Sources"
)

func ramTagger(arn string) *tagger {
	return &tagger{
		service: "ram",
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{propResourceShareArn: arn},
		key:     memberKey,
		value:   memberValue,
	}
}

func diffStrings(oldList, newList []string) (toAdd, toRemove []string) {
	oldSet := make(map[string]struct{}, len(oldList))
	for _, s := range oldList {
		oldSet[s] = struct{}{}
	}

	newSet := make(map[string]struct{}, len(newList))
	for _, s := range newList {
		newSet[s] = struct{}{}
	}

	for _, s := range newList {
		if _, ok := oldSet[s]; !ok {
			toAdd = append(toAdd, s)
		}
	}

	for _, s := range oldList {
		if _, ok := newSet[s]; !ok {
			toRemove = append(toRemove, s)
		}
	}

	return toAdd, toRemove
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}

	return out
}

func init() {
	register(ramResourceShareType, &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			propAllowExternal,
			propPrincipals,
			propResourceArns,
			propPermissionArns,
			propSources,
			memberTags,
		},
		attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if p.has(propPermissionArns) {
				return "", nil, errors.New("permissionArns is not supported")
			}

			if p.has(propSources) {
				return "", nil, errors.New("sources is not supported")
			}

			name := c.nameOf(p, memberName, 255)
			params := map[string]any{
				memberName: name,
			}

			if p.has(propAllowExternal) {
				params[propAllowExternal] = p.truth(propAllowExternal)
			}

			if p.has(propPrincipals) {
				params[propPrincipals] = p.list(propPrincipals)
			}

			if p.has(propResourceArns) {
				params[propResourceArns] = p.list(propResourceArns)
			}

			if p.has(memberTags) {
				params[memberTags] = p[memberTags]
			}

			res, err := c.call(ctx, "ram", "CreateResourceShare", params)
			if err != nil {
				return "", nil, err
			}

			share, _ := res["ResourceShare"].(map[string]any)
			arn, _ := share[propResourceShareArn].(string)
			return arn, map[string]any{memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.has(propPermissionArns) {
				return nil, errors.New("permissionArns is not supported")
			}

			if p.has(propSources) {
				return nil, errors.New("sources is not supported")
			}

			old := props(r.Properties)
			updateParams := map[string]any{
				propResourceShareArn: r.Physical,
			}

			hasUpdate := false
			if p.has(memberName) && p.str(memberName) != old.str(memberName) {
				updateParams[memberName] = p.str(memberName)
				hasUpdate = true
			}

			if p.has(propAllowExternal) {
				allow := p.truth(propAllowExternal)
				if !old.has(propAllowExternal) || allow != old.truth(propAllowExternal) {
					updateParams[propAllowExternal] = allow
					hasUpdate = true
				}
			}

			if hasUpdate {
				if _, err := c.call(ctx, "ram", "UpdateResourceShare", updateParams); err != nil {
					return nil, err
				}
			}

			oldPrincipals := old.strings(propPrincipals)
			newPrincipals := p.strings(propPrincipals)
			toAddPrincipals, toRemovePrincipals := diffStrings(oldPrincipals, newPrincipals)

			if len(toRemovePrincipals) > 0 {
				if _, err := c.call(ctx, "ram", "DisassociateResourceShare", map[string]any{
					propResourceShareArn: r.Physical,
					propPrincipals:       anySlice(toRemovePrincipals),
				}); err != nil {
					return nil, err
				}
			}

			if len(toAddPrincipals) > 0 {
				if _, err := c.call(ctx, "ram", "AssociateResourceShare", map[string]any{
					propResourceShareArn: r.Physical,
					propPrincipals:       anySlice(toAddPrincipals),
				}); err != nil {
					return nil, err
				}
			}

			oldResources := old.strings(propResourceArns)
			newResources := p.strings(propResourceArns)
			toAddResources, toRemoveResources := diffStrings(oldResources, newResources)

			if len(toRemoveResources) > 0 {
				if _, err := c.call(ctx, "ram", "DisassociateResourceShare", map[string]any{
					propResourceShareArn: r.Physical,
					propResourceArns:     anySlice(toRemoveResources),
				}); err != nil {
					return nil, err
				}
			}

			if len(toAddResources) > 0 {
				if _, err := c.call(ctx, "ram", "AssociateResourceShare", map[string]any{
					propResourceShareArn: r.Physical,
					propResourceArns:     anySlice(toAddResources),
				}); err != nil {
					return nil, err
				}
			}

			if err := retag(ctx, c, r.Properties, p, ramTagger(r.Physical)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ram", "DeleteResourceShare", map[string]any{
				propResourceShareArn: r.Physical,
			})
			if gone(err, "UnknownResourceException", "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

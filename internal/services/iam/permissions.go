package iam

import (
	"context"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// permissions are the inline policies and attached managed policies of a
// role, user, or group. They are stored in its record.
type permissions struct {
	Policies map[string]string `json:",omitempty"` // inline policy name to document
	Attached []string          `json:",omitempty"` // ARNs of attached managed policies
}

// deleteConflict reports the policies that keep a principal from being
// deleted.
func (p *permissions) deleteConflict() error {
	switch {
	case len(p.Policies) > 0:
		return deleteConflict("Cannot delete entity, must delete policies first.")
	case len(p.Attached) > 0:
		return deleteConflict("Cannot delete entity, must detach all policies first.")
	}

	return nil
}

// details adds the policies to a resource's details.
func (p *permissions) details(d map[string]any) map[string]any {
	d["InlinePolicies"], d["AttachedPolicies"] = slices.Sorted(maps.Keys(p.Policies)), p.Attached
	return d
}

// holder is one kind of principal whose permissions the policy operations
// change: kind is "role", "user", or "group", as IAM names them in messages.
type holder struct {
	update func(
		ctx context.Context,
		name *string,
		fn func(tx storage.WriteTx, account, owner string, p *permissions) error,
	) error
	view func(ctx context.Context, name *string, fn func(owner string, p *permissions) error) error
	kind string
}

func (s *Service) roles() holder {
	return holder{
		kind: kindRole,
		update: func(
			ctx context.Context,
			name *string,
			fn func(tx storage.WriteTx, account, owner string, p *permissions) error,
		) error {
			return s.updateRole(ctx, name, func(tx storage.WriteTx, account string, r *role) error {
				return fn(tx, account, r.Name, &r.permissions)
			})
		},
		view: func(ctx context.Context, name *string, fn func(owner string, p *permissions) error) error {
			return s.viewRole(ctx, name, func(_ string, r *role) error { return fn(r.Name, &r.permissions) })
		},
	}
}

func (s *Service) users() holder {
	return holder{
		kind: kindUser,
		update: func(
			ctx context.Context,
			name *string,
			fn func(tx storage.WriteTx, account, owner string, p *permissions) error,
		) error {
			return s.updateUser(ctx, name, func(tx storage.WriteTx, account string, u *user) error {
				return fn(tx, account, u.Name, &u.permissions)
			})
		},
		view: func(ctx context.Context, name *string, fn func(owner string, p *permissions) error) error {
			return s.viewUser(ctx, name, func(_ string, u *user) error { return fn(u.Name, &u.permissions) })
		},
	}
}

func (s *Service) groups() holder {
	return holder{
		kind: kindGroup,
		update: func(
			ctx context.Context,
			name *string,
			fn func(tx storage.WriteTx, account, owner string, p *permissions) error,
		) error {
			return s.updateGroup(ctx, name, func(tx storage.WriteTx, account string, g *group) error {
				return fn(tx, account, g.Name, &g.permissions)
			})
		},
		view: func(ctx context.Context, name *string, fn func(owner string, p *permissions) error) error {
			return s.viewGroup(ctx, name, func(_ string, g *group) error { return fn(g.Name, &g.permissions) })
		},
	}
}

// inline returns the stored name of an inline policy, matched
// case-insensitively.
func (h holder) inline(p *permissions, policy string) (string, error) {
	for name := range p.Policies {
		if strings.EqualFold(name, policy) {
			return name, nil
		}
	}

	return "", noSuchEntity("The %s policy with name %s cannot be found.", h.kind, policy)
}

func (h holder) putPolicy(ctx context.Context, name, policy, document *string) error {
	if err := checkName("policyName", policy, policyName); err != nil {
		return err
	}

	if err := checkDocument("policyDocument", document); err != nil {
		return err
	}

	return h.update(ctx, name, func(_ storage.WriteTx, _, _ string, p *permissions) error {
		// Inline policy names are case-insensitive: a new case replaces the old.
		if stored, findErr := h.inline(p, *policy); findErr == nil {
			delete(p.Policies, stored)
		}

		if p.Policies == nil {
			p.Policies = map[string]string{}
		}

		p.Policies[*policy] = *document
		return nil
	})
}

// getPolicy returns the principal's name, the inline policy's stored name, and
// its encoded document.
func (h holder) getPolicy(ctx context.Context, name, policy *string) (owner, stored string, doc *string, err error) {
	if nameErr := checkName("policyName", policy, policyName); nameErr != nil {
		return "", "", nil, nameErr
	}

	err = h.view(ctx, name, func(o string, p *permissions) error {
		var findErr error
		if stored, findErr = h.inline(p, *policy); findErr != nil {
			return findErr
		}

		owner, doc = o, encodeDocument(p.Policies[stored])
		return nil
	})
	return owner, stored, doc, err
}

func (h holder) deletePolicy(ctx context.Context, name, policy *string) error {
	if err := checkName("policyName", policy, policyName); err != nil {
		return err
	}

	return h.update(ctx, name, func(_ storage.WriteTx, _, _ string, p *permissions) error {
		stored, err := h.inline(p, *policy)
		if err != nil {
			return err
		}

		delete(p.Policies, stored)
		return nil
	})
}

func (h holder) listPolicies(
	ctx context.Context,
	name, marker *string,
	maxItems *int64,
) (names []string, truncated *bool, next *string, err error) {
	err = h.view(ctx, name, func(_ string, p *permissions) error {
		var pageErr error
		names, truncated, next, pageErr = page(
			slices.Sorted(maps.Keys(p.Policies)), func(n string) string { return n }, marker, maxItems)
		return pageErr
	})
	return names, truncated, next, err
}

func (h holder) attachPolicy(ctx context.Context, name, policyArn *string) error {
	ref, err := requiredPolicyARN(policyArn)
	if err != nil {
		return err
	}

	return h.update(ctx, name, func(tx storage.WriteTx, account, _ string, p *permissions) error {
		// Attaching an attached policy changes nothing, including its count.
		if slices.ContainsFunc(p.Attached, func(a string) bool { return strings.EqualFold(a, ref.arn()) }) {
			return nil
		}

		if len(p.Attached) >= maxAttached {
			return limitExceeded("Cannot exceed quota for PoliciesPer%s%s: %d",
				strings.ToUpper(h.kind[:1]), h.kind[1:], maxAttached)
		}

		canonical, countErr := countAttachment(tx, account, ref, 1)
		if countErr != nil {
			return countErr
		}

		p.Attached = append(p.Attached, canonical)
		return nil
	})
}

func (h holder) detachPolicy(ctx context.Context, name, policyArn *string) error {
	ref, err := requiredPolicyARN(policyArn)
	if err != nil {
		return err
	}

	return h.update(ctx, name, func(tx storage.WriteTx, account, _ string, p *permissions) error {
		i := slices.IndexFunc(p.Attached, func(a string) bool { return strings.EqualFold(a, ref.arn()) })
		if i < 0 {
			return noSuchEntity("Policy %s was not found.", *policyArn)
		}

		// Count against the ARN as attached, whatever case the request used.
		attached, parseErr := parsePolicyARN(p.Attached[i])
		if parseErr != nil {
			return parseErr
		}

		if _, countErr := countAttachment(tx, account, attached, -1); countErr != nil {
			return countErr
		}

		p.Attached = slices.Delete(p.Attached, i, i+1)
		return nil
	})
}

func (h holder) listAttached(
	ctx context.Context,
	name, pathPrefix, marker *string,
	maxItems *int64,
) (policies []api.AttachedPolicy, truncated *bool, next *string, err error) {
	prefix, err := checkPath(pathPrefix, policyPath)
	if err != nil {
		return nil, nil, nil, err
	}

	policies = []api.AttachedPolicy{}
	err = h.view(ctx, name, func(_ string, p *permissions) error {
		var refs []policyRef
		for _, a := range p.Attached {
			if ref, parseErr := parsePolicyARN(a); parseErr == nil && strings.HasPrefix(ref.path, prefix) {
				refs = append(refs, ref)
			}
		}

		slices.SortFunc(refs, func(a, b policyRef) int { return strings.Compare(a.arn(), b.arn()) })
		selected, more, after, pageErr := page(refs, policyRef.arn, marker, maxItems)
		if pageErr != nil {
			return pageErr
		}

		for _, ref := range selected {
			policies = append(policies, api.AttachedPolicy{PolicyName: new(ref.name), PolicyArn: new(ref.arn())})
		}

		truncated, next = more, after
		return nil
	})
	return policies, truncated, next, err
}

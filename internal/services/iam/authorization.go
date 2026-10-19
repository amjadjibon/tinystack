package iam

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Authorization is a snapshot of the identity's current policy sources and tags.
type Authorization struct {
	Principal   request.Principal
	Documents   []iampolicy.Document
	Boundary    []iampolicy.Document
	Session     []iampolicy.Document
	Tags        map[string]string
	Unsupported []string
}

// Authorization reads user/group or role policy documents in one transaction.
// Stable IDs prevent old sessions from acquiring a recreated identity's policies.
func (s *Service) Authorization(ctx context.Context, principal *request.Principal) (Authorization, error) {
	out := Authorization{Principal: *principal}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		switch principal.Kind {
		case request.PrincipalUser:
			users, err := list[user](tx, principal.Account, kindUser)
			if err != nil {
				return err
			}

			for _, u := range users {
				if u.ID != principal.ID {
					continue
				}

				out.Principal.Name, out.Principal.ARN = u.Name, userARN(principal.Account, u)
				out.Principal.PolicyARN = out.Principal.ARN
				out.Tags = common.CloneTags(u.Tags)
				if err := collectLimits(tx, principal.Account, u.Boundary, &out); err != nil {
					return err
				}

				if err := collectPolicies(
					tx,
					principal.Account,
					out.Principal.PolicyARN,
					&u.permissions,
					&out,
				); err != nil {
					return err
				}

				groups, err := groupsOf(tx, principal.Account, u.Name)
				if err != nil {
					return err
				}

				for _, g := range groups {
					if err := collectPolicies(
						tx,
						principal.Account,
						groupARN(principal.Account, g),
						&g.permissions,
						&out,
					); err != nil {
						return err
					}
				}

				return nil
			}
		case request.PrincipalRole:
			roles, err := list[role](tx, principal.Account, kindRole)
			if err != nil {
				return err
			}

			for _, r := range roles {
				if r.ID != principal.ID {
					continue
				}

				out.Principal.Name, out.Principal.PolicyARN = r.Name, roleARN(principal.Account, r)
				out.Tags = common.CloneTags(r.Tags)
				if err := collectLimits(tx, principal.Account, r.Boundary, &out); err != nil {
					return err
				}

				return collectPolicies(tx, principal.Account, out.Principal.PolicyARN, &r.permissions, &out)
			}
		}

		out.Unsupported = append(out.Unsupported, "IAM identity no longer exists: "+principal.PolicyARN)
		return nil
	})
	return out, err
}

func collectPolicies(tx storage.ReadTx, account, owner string, p *permissions, out *Authorization) error {
	parts := strings.SplitN(owner, ":", 6)
	sourceType, _, _ := strings.Cut(parts[5], "/")

	for _, name := range slices.Sorted(maps.Keys(p.Policies)) {
		out.Documents = append(
			out.Documents,
			iampolicy.Document{Name: owner + "/inline/" + name, JSON: p.Policies[name], SourceType: sourceType},
		)
	}

	for _, attached := range p.Attached {
		ref, err := parsePolicyARN(attached)
		if err != nil {
			return err
		}

		document, err := managedDocument(tx, account, attached)
		if err != nil {
			if !ref.awsManaged() {
				return err
			}

			out.Unsupported = append(out.Unsupported, "AWS-managed policy document unavailable: "+attached)
			continue
		}

		out.Documents = append(out.Documents, document)
	}

	return nil
}

package iam

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Resources lists every role and managed policy, for the resource browser. IAM
// is global, so resources have no region.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			parts := strings.SplitN(k, "/", 4)
			if len(parts) != 4 || parts[1] != "iam" {
				continue
			}

			account := parts[0]
			switch parts[2] {
			case kindRole:
				r, _, err := read[role](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: kindRole, Account: account, Name: r.Name, ARN: roleARN(account, r), Tags: r.Tags,
					Details: map[string]any{
						"Path":                     r.Path,
						"AssumeRolePolicyDocument": r.AssumeRolePolicy,
						"InlinePolicies":           slices.Sorted(maps.Keys(r.Policies)),
						"AttachedPolicies":         r.Attached,
					},
				})
			case kindPolicy:
				p, _, err := read[policy](tx, k)
				if err != nil {
					return err
				}

				versions := make([]string, 0, len(p.Versions))
				document := ""
				for _, v := range p.Versions {
					versions = append(versions, v.ID)
					if v.ID == p.DefaultVersion {
						document = v.Document
					}
				}

				out = append(out, catalog.Resource{
					Type: kindPolicy, Account: account, Name: p.Name, ARN: policyARN(account, p), Tags: p.Tags,
					Details: map[string]any{
						"Path": p.Path, "DefaultVersionId": p.DefaultVersion, "Versions": versions,
						"AttachmentCount": p.Attachments, "Document": document,
					},
				})
			}
		}

		return nil
	})

	return out, err
}

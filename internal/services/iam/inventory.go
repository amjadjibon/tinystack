package iam

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// detailPath is the resource detail that holds an entity's IAM path.
const detailPath = "Path"

// Resources lists every role, managed policy, user, and group, for the resource
// browser. IAM is global, so resources have no region. Access key secrets are
// never listed.
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
					Details: r.details(map[string]any{
						detailPath: r.Path, "AssumeRolePolicyDocument": r.AssumeRolePolicy,
					}),
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
						detailPath: p.Path, "DefaultVersionId": p.DefaultVersion, "Versions": versions,
						"AttachmentCount": p.Attachments, "Document": document,
					},
				})
			case kindUser:
				u, _, err := read[user](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: kindUser, Account: account, Name: u.Name, ARN: userARN(account, u), Tags: u.Tags,
					Details: u.details(map[string]any{detailPath: u.Path, "UserId": u.ID, "AccessKeyIds": u.Keys}),
				})
			case kindGroup:
				g, _, err := read[group](tx, k)
				if err != nil {
					return err
				}

				out = append(out, catalog.Resource{
					Type: kindGroup, Account: account, Name: g.Name, ARN: groupARN(account, g),
					Details: g.details(map[string]any{detailPath: g.Path, "GroupId": g.ID, "Users": g.Users}),
				})
			}
		}

		return nil
	})

	return out, err
}

package iam

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	kind, name := "", ""
	for _, candidate := range []struct{ kind, field string }{
		{kindUser, "UserName"}, {kindRole, "RoleName"}, {kindGroup, "GroupName"},
	} {
		if value := catalog.InputString(input, candidate.field); value != "" {
			kind, name = candidate.kind, value
			break
		}
	}

	if operation == "AddUserToGroup" || operation == "RemoveUserFromGroup" {
		kind, name = kindGroup, catalog.InputString(input, "GroupName")
	}

	if operation == "CreatePolicy" {
		kind, name = kindPolicy, catalog.InputString(input, "PolicyName")
	}

	if kind == "" && m.Principal != nil && m.Principal.Kind == request.PrincipalUser &&
		(operation == "GetUser" || strings.Contains(operation, "AccessKey")) {
		kind, name = kindUser, m.Principal.Name
	}

	if kind == "" {
		if policyARN := catalog.InputString(input, "PolicyArn"); policyARN != "" {
			target := catalog.AuthorizationResource{ARN: policyARN, TagsKnown: true}
			ref, parseErr := parsePolicyARN(policyARN)
			if parseErr != nil {
				return nil, parseErr
			}

			readErr := s.store.View(ctx, func(tx storage.ReadTx) error {
				stored, _, readErr := storage.Read[policy](tx, key(m.AccountID, kindPolicy, ref.name))
				if stored != nil {
					target.Tags = common.CloneTags(stored.Tags)
				}

				return readErr
			})
			return []catalog.AuthorizationResource{target}, readErr
		}

		return nil, nil
	}

	target := catalog.AuthorizationResource{ARN: arn(m.AccountID, kind+"/"+name)}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		switch kind {
		case kindUser:
			u, _, readErr := storage.Read[user](tx, key(m.AccountID, kind, name))
			if readErr != nil {
				return readErr
			}

			if u != nil {
				target.ARN, target.Tags = userARN(m.AccountID, u), common.CloneTags(u.Tags)
			}

			target.TagsKnown = true
		case kindRole:
			r, _, readErr := storage.Read[role](tx, key(m.AccountID, kind, name))
			if readErr != nil {
				return readErr
			}

			if r != nil {
				target.ARN, target.Tags = roleARN(m.AccountID, r), common.CloneTags(r.Tags)
			}

			target.TagsKnown = true
		case kindPolicy:
			target.TagsKnown = true
		case kindGroup:
			g, _, readErr := storage.Read[group](tx, key(m.AccountID, kind, name))
			if readErr != nil {
				return readErr
			}

			if g != nil {
				target.ARN = groupARN(m.AccountID, g)
			}
		}

		return nil
	})
	if strings.HasPrefix(operation, "Create") {
		if path := catalog.InputString(input, "Path"); path != "" {
			target.ARN = arn(m.AccountID, kind+path+name)
		}
	}

	return []catalog.AuthorizationResource{target}, err
}

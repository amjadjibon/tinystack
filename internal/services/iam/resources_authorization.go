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
		{kindUser, "UserName"},
		{kindRole, "RoleName"},
		{kindGroup, "GroupName"},
		{kindProfile, "InstanceProfileName"},
		{kindServerCert, "ServerCertificateName"},
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

	if operation == "CreateSAMLProvider" {
		kind, name = kindSAML, catalog.InputString(input, "Name")
	}

	if operation == "CreateVirtualMFADevice" {
		kind, name = kindMFA, catalog.InputString(input, "VirtualMFADeviceName")
	}

	if kind == "" && m.Principal != nil && m.Principal.Kind == request.PrincipalUser &&
		(operation == "GetUser" || operation == "ChangePassword" ||
			strings.Contains(operation, "AccessKey") ||
			strings.Contains(operation, "MFADevice") ||
			strings.Contains(operation, "SSHPublicKey") ||
			strings.Contains(operation, "ServiceSpecificCredential")) {
		kind, name = kindUser, m.Principal.Name
	}

	if kind == "" {
		if res, ok, authErr := s.authorizationByARN(ctx, m.AccountID, input); ok || authErr != nil {
			return res, authErr
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
		case kindPolicy, kindSAML, kindMFA:
			target.TagsKnown = true
		case kindProfile:
			p, _, readErr := storage.Read[profile](tx, key(m.AccountID, kind, name))
			if readErr != nil {
				return readErr
			}

			if p != nil {
				target.ARN, target.Tags = profileARN(m.AccountID, p), common.CloneTags(p.Tags)
			}

			target.TagsKnown = true
		case kindServerCert:
			c, _, readErr := storage.Read[serverCertificate](tx, key(m.AccountID, kind, name))
			if readErr != nil {
				return readErr
			}

			if c != nil {
				target.ARN, target.Tags = serverCertARN(m.AccountID, c), common.CloneTags(c.Tags)
			}

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
	if strings.HasPrefix(operation, "Create") || operation == "UploadServerCertificate" {
		if path := catalog.InputString(input, "Path"); path != "" {
			target.ARN = arn(m.AccountID, kind+path+name)
		}
	}

	return []catalog.AuthorizationResource{target}, err
}

func (s *Service) authorizationByARN(
	ctx context.Context,
	account string,
	input any,
) ([]catalog.AuthorizationResource, bool, error) {
	if policyARN := catalog.InputString(input, "PolicyArn"); policyARN != "" {
		target := catalog.AuthorizationResource{ARN: policyARN, TagsKnown: true}
		ref, parseErr := parsePolicyARN(policyARN)
		if parseErr != nil {
			return nil, true, parseErr
		}

		readErr := s.store.View(ctx, func(tx storage.ReadTx) error {
			stored, _, readErr := storage.Read[policy](tx, key(account, kindPolicy, ref.name))
			if stored != nil {
				target.Tags = common.CloneTags(stored.Tags)
			}

			return readErr
		})
		return []catalog.AuthorizationResource{target}, true, readErr
	}

	if samlARN := catalog.InputString(input, "SAMLProviderArn"); samlARN != "" {
		target := catalog.AuthorizationResource{ARN: samlARN, TagsKnown: true}
		readErr := s.store.View(ctx, func(tx storage.ReadTx) error {
			if p, _, loadErr := loadSAMLProvider(tx, account, &samlARN); loadErr == nil && p != nil {
				target.Tags = common.CloneTags(p.Tags)
			}

			return nil
		})
		return []catalog.AuthorizationResource{target}, true, readErr
	}

	if oidcARN := catalog.InputString(input, "OpenIDConnectProviderArn"); oidcARN != "" {
		target := catalog.AuthorizationResource{ARN: oidcARN, TagsKnown: true}
		readErr := s.store.View(ctx, func(tx storage.ReadTx) error {
			if p, _, loadErr := loadProvider(tx, account, &oidcARN); loadErr == nil && p != nil {
				target.Tags = common.CloneTags(p.Tags)
			}

			return nil
		})
		return []catalog.AuthorizationResource{target}, true, readErr
	}

	if serial := catalog.InputString(input, "SerialNumber"); serial != "" {
		target := catalog.AuthorizationResource{ARN: serial, TagsKnown: true}
		readErr := s.store.View(ctx, func(tx storage.ReadTx) error {
			if d, _, loadErr := loadVirtualMFA(tx, account, &serial); loadErr == nil && d != nil {
				target.Tags = common.CloneTags(d.Tags)
			}

			return nil
		})
		return []catalog.AuthorizationResource{target}, true, readErr
	}

	return nil, false, nil
}

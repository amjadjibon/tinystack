package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ssoadmin"
)

const (
	rolePath         = "/aws-reserved/sso.amazonaws.com/"
	inlinePolicyName = "AwsSSOInlinePolicy"
)

// roleName is the IAM role Identity Center provisions for a permission set: AWSReservedSSO_<name>_<suffix>, where
// the suffix is stable for the permission set's ARN.
func roleName(permissionSet, permissionSetARN string) string {
	sum := sha256.Sum256([]byte(permissionSetARN))
	return "AWSReservedSSO_" + permissionSet + "_" + hex.EncodeToString(sum[:8])
}

// provision creates the permission set's role in the target account if it is missing and makes its session limit
// and policies match the permission set. It returns the role's ARN.
func (s *Service) provision(ctx context.Context, e *ssoadmin.Entitlement) (string, error) {
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: request.Scope{AccountID: e.Account}})
	name := new(roleName(e.PermissionSet, e.ARN))
	role, err := s.ensureRole(ctx, name, e)
	if err != nil {
		return "", err
	}

	if *role.MaxSessionDuration != e.SessionSeconds {
		if _, err = s.roles.UpdateRole(ctx, &iamapi.UpdateRoleRequest{
			RoleName: name, MaxSessionDuration: new(e.SessionSeconds),
		}); err != nil {
			return "", err
		}
	}

	if e.InlinePolicy != "" {
		_, err = s.roles.PutRolePolicy(ctx, &iamapi.PutRolePolicyRequest{
			RoleName: name, PolicyName: new(inlinePolicyName), PolicyDocument: new(e.InlinePolicy),
		})
	} else {
		_, err = s.roles.DeleteRolePolicy(ctx, &iamapi.DeleteRolePolicyRequest{
			RoleName: name, PolicyName: new(inlinePolicyName),
		})
	}

	if err != nil && awserror.From(err).Code != "NoSuchEntityException" {
		return "", err
	}

	attached, err := s.roles.ListAttachedRolePolicies(ctx, &iamapi.ListAttachedRolePoliciesRequest{
		RoleName: name, MaxItems: new(int64(1000)),
	})
	if err != nil {
		return "", err
	}

	var current []string
	for _, p := range attached.AttachedPolicies {
		current = append(current, *p.PolicyArn)
		if !slices.Contains(e.ManagedPolicies, *p.PolicyArn) {
			if _, err = s.roles.DetachRolePolicy(ctx, &iamapi.DetachRolePolicyRequest{
				RoleName: name, PolicyArn: p.PolicyArn,
			}); err != nil {
				return "", err
			}
		}
	}

	for _, arn := range e.ManagedPolicies {
		if !slices.Contains(current, arn) {
			if _, err = s.roles.AttachRolePolicy(ctx, &iamapi.AttachRolePolicyRequest{
				RoleName: name, PolicyArn: new(arn),
			}); err != nil {
				return "", err
			}
		}
	}

	return *role.Arn, nil
}

// ensureRole returns the permission set's role, creating it when it does not exist. Two sign-ins may race to create
// it; the loser reads the winner's role.
func (s *Service) ensureRole(ctx context.Context, name *string, e *ssoadmin.Entitlement) (*iamapi.Role, error) {
	got, err := s.roles.GetRole(ctx, &iamapi.GetRoleRequest{RoleName: name})
	if err == nil {
		return got.Role, nil
	}

	if awserror.From(err).Code != "NoSuchEntityException" {
		return nil, err
	}

	provider := identityProvider(e.Account)
	trust := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",`+
		`"Principal":{"Federated":%q},"Action":["sts:AssumeRoleWithSAML","sts:TagSession"],`+
		`"Condition":{"StringEquals":{"SAML:aud":"https://signin.aws.amazon.com/saml"}}}]}`, provider)
	created, err := s.roles.CreateRole(ctx, &iamapi.CreateRoleRequest{
		RoleName:                 name,
		Path:                     new(rolePath),
		AssumeRolePolicyDocument: new(trust),
		Description: new(
			"Identity Center permission set " + e.PermissionSet,
		),
		MaxSessionDuration: new(e.SessionSeconds),
	})
	switch {
	case err == nil:
		return created.Role, nil
	case awserror.From(err).Code != "EntityAlreadyExistsException":
		return nil, err
	}

	got, err = s.roles.GetRole(ctx, &iamapi.GetRoleRequest{RoleName: name})
	if err != nil {
		return nil, err
	}

	return got.Role, nil
}

// identityProvider names the SAML provider the provisioned roles trust. TinyStack issues their sessions in-process,
// so the provider is not created.
func identityProvider(account string) string {
	sum := sha256.Sum256([]byte(account))
	return "arn:aws:iam::" + account + ":saml-provider/AWSSSO_" + hex.EncodeToString(sum[:8]) + "_DO_NOT_DELETE"
}

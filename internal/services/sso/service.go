// Package sso implements the IAM Identity Center access portal API. An access token from package ssooidc lists the
// accounts and permission sets the user is assigned, directly or through a group, and exchanges one for role
// credentials. Each permission set is provisioned as an IAM role in the target account when its credentials are
// first requested, and the role's policies are brought in line with the permission set on every request.
//
// The service keeps no records of its own.
package sso

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sso"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/services/ssooidc"
	"github.com/amjadjibon/tinystack/internal/services/sts"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	oidc  *ssooidc.Service
	roles *iam.Service
	sts   *sts.Service
}

func New(store storage.Store, oidc *ssooidc.Service, roles *iam.Service, tokens *sts.Service) *Service {
	return &Service{store: store, oidc: oidc, roles: roles, sts: tokens}
}

func unauthorized() error {
	return &awserror.Error{
		Code: "UnauthorizedException", Message: "Session token not found or invalid", StatusCode: 401,
	}
}

// entitlements returns the signed-in user and what the user may sign in to.
func (s *Service) entitlements(ctx context.Context, accessToken *string) (
	session ssooidc.Session, list []ssoadmin.Entitlement, err error,
) {
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var ok bool
		session, ok, err = s.oidc.Session(tx, common.Value(accessToken))
		switch {
		case err != nil:
			return err
		case !ok:
			return unauthorized()
		}

		list, err = ssoadmin.Entitlements(tx, session.Account, session.UserID)
		return err
	})

	return session, list, err
}

func pageLimit(maximum *int64) (int, error) {
	if maximum == nil {
		return 100, nil
	}

	if *maximum < 1 || *maximum > 100 {
		return 0, awserror.New("InvalidRequestException", "maxResults must be between 1 and 100")
	}

	return int(*maximum), nil
}

func (s *Service) ListAccounts(ctx context.Context, in *api.ListAccountsRequest) (*api.ListAccountsResponse, error) {
	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	_, entitlements, err := s.entitlements(ctx, in.AccessToken)
	if err != nil {
		return nil, err
	}

	accounts := []api.AccountInfo{}
	for _, e := range entitlements {
		if len(accounts) == 0 || *accounts[len(accounts)-1].AccountId != e.Account {
			accounts = append(accounts, api.AccountInfo{AccountId: new(e.Account), AccountName: new(e.Account)})
		}
	}

	page, next, err := common.Page(accounts, func(a api.AccountInfo) string { return *a.AccountId }, in.NextToken,
		limit, awserror.New("InvalidRequestException", "Invalid nextToken"))
	if err != nil {
		return nil, err
	}

	return &api.ListAccountsResponse{AccountList: page, NextToken: next}, nil
}

func (s *Service) ListAccountRoles(
	ctx context.Context,
	in *api.ListAccountRolesRequest,
) (*api.ListAccountRolesResponse, error) {
	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	_, entitlements, err := s.entitlements(ctx, in.AccessToken)
	if err != nil {
		return nil, err
	}

	roles := []api.RoleInfo{}
	for _, e := range entitlements {
		if e.Account == common.Value(in.AccountId) {
			roles = append(roles, api.RoleInfo{AccountId: new(e.Account), RoleName: new(e.PermissionSet)})
		}
	}

	page, next, err := common.Page(roles, func(r api.RoleInfo) string { return *r.RoleName }, in.NextToken, limit,
		awserror.New("InvalidRequestException", "Invalid nextToken"))
	if err != nil {
		return nil, err
	}

	return &api.ListAccountRolesResponse{RoleList: page, NextToken: next}, nil
}

func (s *Service) GetRoleCredentials(
	ctx context.Context,
	in *api.GetRoleCredentialsRequest,
) (*api.GetRoleCredentialsResponse, error) {
	session, entitlements, err := s.entitlements(ctx, in.AccessToken)
	if err != nil {
		return nil, err
	}

	var found *ssoadmin.Entitlement
	for i := range entitlements {
		e := &entitlements[i]
		if e.Account == common.Value(in.AccountId) && e.PermissionSet == common.Value(in.RoleName) {
			found = e
		}
	}

	if found == nil {
		return nil, awserror.New("ResourceNotFoundException", "No access")
	}

	roleARN, err := s.provision(ctx, found)
	if err != nil {
		return nil, err
	}

	issued, err := s.sts.AssumeRoleForIdentityCenter(ctx, roleARN, session.UserName, found.SessionSeconds)
	if err != nil {
		return nil, err
	}

	return &api.GetRoleCredentialsResponse{RoleCredentials: &api.RoleCredentials{
		AccessKeyId: new(issued.AccessKeyID), SecretAccessKey: new(issued.SecretAccessKey),
		SessionToken: new(issued.SessionToken), Expiration: new(issued.Expires.UnixMilli()),
	}}, nil
}

func (s *Service) Logout(ctx context.Context, in *api.LogoutRequest) (*api.LogoutOutput, error) {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, ok, err := s.oidc.Session(tx, common.Value(in.AccessToken))
		switch {
		case err != nil:
			return err
		case !ok:
			return unauthorized()
		}

		return s.oidc.Revoke(tx, *in.AccessToken)
	})
	if err != nil {
		return nil, err
	}

	return &api.LogoutOutput{}, nil
}

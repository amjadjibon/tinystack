package cognitoidp

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// writable reports whether a client may write an attribute for its users. A
// user cannot verify its own attributes unless the client allows it.
func writable(c *client, name string) bool {
	if len(c.Settings.WriteAttributes) > 0 {
		return slices.Contains(c.Settings.WriteAttributes, name)
	}

	return !strings.HasSuffix(name, verifiedSuffix)
}

// clientWrite applies attributes a user writes through c.
func clientWrite(p *pool, c *client, u *user, attrs []api.AttributeType) ([]string, error) {
	for _, a := range attrs {
		if !writable(c, common.Value(a.Name)) {
			return nil, notAuthorized("A client attempted to write unauthorized attribute")
		}
	}

	return setAttributes(p, u, attrs, false)
}

// asUser runs fn for the user an access token stands for and saves the user
// after it, recording the messages fn queued once the change commits.
func (s *Service) asUser(
	ctx context.Context,
	token *string,
	fn func(tx storage.WriteTx, a *accessToken, out *outbox) error,
) error {
	var out outbox
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, err := s.checkAccess(tx, token)
		if err != nil {
			return err
		}

		before := *a.user
		before.Attributes = cloneMap(a.user.Attributes)
		if fnErr := fn(tx, a, &out); fnErr != nil {
			return fnErr
		}

		a.user.Modified = s.now().UTC()
		return saveUser(tx, a.scope, a.pool, &before, a.user, false)
	})
	if err == nil {
		out.flush(s.deliveries)
	}

	return err
}

func (s *Service) GetUser(ctx context.Context, in *api.GetUserRequest) (*api.GetUserResponse, error) {
	var out *api.GetUserResponse
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		a, err := s.checkAccess(tx, in.AccessToken)
		if err != nil {
			return err
		}

		var attrs []api.AttributeType
		for _, attr := range a.user.attributes(nil) {
			if *attr.Name == attrSub || readable(a.client, *attr.Name) {
				attrs = append(attrs, attr)
			}
		}

		out = &api.GetUserResponse{
			Username: new(a.user.Username), UserAttributes: attrs,
			UserMFASettingList: a.user.mfaSettings(), MFAOptions: a.user.legacyMFA(),
		}

		if a.user.PreferredMFA != "" {
			out.PreferredMfaSetting = new(a.user.PreferredMFA)
		}

		return nil
	})

	return out, err
}

func (s *Service) UpdateUserAttributes(
	ctx context.Context,
	in *api.UpdateUserAttributesRequest,
) (*api.UpdateUserAttributesResponse, error) {
	var sentTo []sent
	err := s.asUser(ctx, in.AccessToken, func(_ storage.WriteTx, a *accessToken, out *outbox) error {
		changed, err := clientWrite(a.pool, a.client, a.user, in.UserAttributes)
		if err != nil {
			return err
		}

		sentTo = s.verifyChanged(a.scope, a.pool, a.user, changed, out)
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.UpdateUserAttributesResponse{CodeDeliveryDetailsList: []api.CodeDeliveryDetailsType{}}
	for _, d := range sentTo {
		out.CodeDeliveryDetailsList = append(out.CodeDeliveryDetailsList, *details(d))
	}

	return out, nil
}

func (s *Service) DeleteUserAttributes(
	ctx context.Context,
	in *api.DeleteUserAttributesRequest,
) (*api.DeleteUserAttributesResponse, error) {
	attrs := make([]api.AttributeType, 0, len(in.UserAttributeNames))
	for _, name := range in.UserAttributeNames {
		attrs = append(attrs, api.AttributeType{Name: new(name), Value: new("")})
	}

	err := s.asUser(ctx, in.AccessToken, func(_ storage.WriteTx, a *accessToken, _ *outbox) error {
		_, err := clientWrite(a.pool, a.client, a.user, attrs)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserAttributesResponse{}, nil
}

func checkVerifiable(u *user, name *string) error {
	if common.Value(name) != attrEmail && common.Value(name) != attrPhone {
		return invalidParameter("Only email and phone_number can be verified.")
	}

	if u.Attributes[*name] == "" {
		return invalidParameter("The user has no %s to verify.", *name)
	}

	return nil
}

func (s *Service) GetUserAttributeVerificationCode(
	ctx context.Context,
	in *api.GetUserAttributeVerificationCodeRequest,
) (*api.GetUserAttributeVerificationCodeResponse, error) {
	var d sent
	err := s.asUser(ctx, in.AccessToken, func(_ storage.WriteTx, a *accessToken, out *outbox) error {
		if err := checkVerifiable(a.user, in.AttributeName); err != nil {
			return err
		}

		attr := *in.AttributeName
		subject, template := verificationMessage(a.pool, mediumOf(attr))
		secret := a.user.setCode(purposeVerify+attr, attr, s.now().UTC(), codeValidity)
		d = out.send(a.scope, a.pool, a.user, mediumOf(attr), subject, template, secret)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetUserAttributeVerificationCodeResponse{CodeDeliveryDetails: details(d)}, nil
}

func (s *Service) VerifyUserAttribute(
	ctx context.Context,
	in *api.VerifyUserAttributeRequest,
) (*api.VerifyUserAttributeResponse, error) {
	err := s.asUser(ctx, in.AccessToken, func(_ storage.WriteTx, a *accessToken, _ *outbox) error {
		if err := checkVerifiable(a.user, in.AttributeName); err != nil {
			return err
		}

		if err := a.user.useCode(purposeVerify+*in.AttributeName, common.Value(in.Code), s.now().UTC()); err != nil {
			return err
		}

		a.user.Attributes[*in.AttributeName+verifiedSuffix] = valueTrue
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.VerifyUserAttributeResponse{}, nil
}

func (s *Service) ChangePassword(
	ctx context.Context,
	in *api.ChangePasswordRequest,
) (*api.ChangePasswordResponse, error) {
	err := s.asUser(ctx, in.AccessToken, func(_ storage.WriteTx, a *accessToken, _ *outbox) error {
		if !checkPassword(a.user.Password, common.Value(in.PreviousPassword)) {
			return notAuthorized("Incorrect username or password.")
		}

		if err := checkPolicy(a.pool, in.ProposedPassword); err != nil {
			return err
		}

		return setPassword(a.pool, a.user, *in.ProposedPassword, true, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	return &api.ChangePasswordResponse{}, nil
}

func (s *Service) DeleteUser(ctx context.Context, in *api.DeleteUserRequest) (*api.DeleteUserOutput, error) {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, err := s.checkAccess(tx, in.AccessToken)
		if err != nil {
			return err
		}

		if signOutErr := signOut(tx, a.scope, a.pool, a.user, ""); signOutErr != nil {
			return signOutErr
		}

		deleteUser(tx, a.scope, a.pool, a.user)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserOutput{}, nil
}

// GlobalSignOut ends every session of the signed-in user: its access tokens
// and refresh tokens stop working.
func (s *Service) GlobalSignOut(ctx context.Context, in *api.GlobalSignOutRequest) (*api.GlobalSignOutResponse, error) {
	err := s.asUser(ctx, in.AccessToken, func(tx storage.WriteTx, a *accessToken, _ *outbox) error {
		return signOut(tx, a.scope, a.pool, a.user, "")
	})
	if err != nil {
		return nil, err
	}

	return &api.GlobalSignOutResponse{}, nil
}

func (s *Service) AdminUserGlobalSignOut(
	ctx context.Context,
	in *api.AdminUserGlobalSignOutRequest,
) (*api.AdminUserGlobalSignOutResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(tx storage.WriteTx, scope request.Scope, p *pool, u *user, _ *outbox) error {
			return signOut(tx, scope, p, u, "")
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminUserGlobalSignOutResponse{}, nil
}

// RevokeToken ends the session a refresh token belongs to.
func (s *Service) RevokeToken(ctx context.Context, in *api.RevokeTokenRequest) (*api.RevokeTokenResponse, error) {
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		if !c.acceptsSecret(common.Value(in.ClientSecret)) {
			return notAuthorized("Client secret is invalid")
		}

		if !common.Value(c.Settings.EnableTokenRevocation) {
			return awserror.New("UnsupportedOperationException", "Token revocation is not enabled for this client.")
		}

		token := common.Value(in.Token)
		if strings.Count(token, ".") == 2 {
			return awserror.New("UnsupportedTokenTypeException", "Only refresh tokens can be revoked.")
		}

		r, ok, err := storage.Read[refresh](tx, refreshKey(scope, p.ID, token))
		if err != nil || !ok {
			// Revoking an unknown or revoked token succeeds.
			return err
		}

		if r.Client != c.ID {
			return notAuthorized("The token was not issued to this client.")
		}

		u, ok, err := storage.Read[user](tx, userKey(scope, p, r.Username))
		if err != nil || !ok {
			tx.Delete(refreshKey(scope, p.ID, token))
			return err
		}

		if signOutErr := signOut(tx, scope, p, u, r.Origin); signOutErr != nil {
			return signOutErr
		}

		return storage.Write(tx, userKey(scope, p, u.Username), u)
	})
	if err != nil {
		return nil, err
	}

	return &api.RevokeTokenResponse{}, nil
}

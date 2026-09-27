package cognitoidp

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const codeValidity = 24 * time.Hour

// changeUser runs fn on the user name names in the pool id names and saves the
// user after it, recording any messages fn queued once the change commits.
func (s *Service) changeUser(
	ctx context.Context,
	id, name *string,
	fn func(tx storage.WriteTx, scope request.Scope, p *pool, u *user, out *outbox) error,
) error {
	var out outbox
	err := s.update(ctx, id, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		u, err := findUser(tx, scope, p, name)
		if err != nil {
			return err
		}

		before := *u
		before.Attributes = cloneMap(u.Attributes)
		if fnErr := fn(tx, scope, p, u, &out); fnErr != nil {
			return fnErr
		}

		u.Modified = s.now().UTC()
		return saveUser(tx, scope, p, &before, u, false)
	})
	if err == nil {
		out.flush(s.deliveries)
	}

	return err
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}

	return out
}

func (s *Service) AdminCreateUser(
	ctx context.Context,
	in *api.AdminCreateUserRequest,
) (*api.AdminCreateUserResponse, error) {
	action := value(in.MessageAction)
	if !oneOf(in.MessageAction, "RESEND", "SUPPRESS") {
		return nil, invalidParameter("MessageAction must be RESEND or SUPPRESS.")
	}

	mediums := in.DesiredDeliveryMediums
	for _, m := range mediums {
		if m != mediumEmail && m != mediumSMS {
			return nil, invalidParameter("DesiredDeliveryMediums must be EMAIL or SMS.")
		}
	}

	if len(mediums) == 0 {
		mediums = []string{mediumSMS}
	}

	var (
		out     outbox
		created *user
	)
	now := s.now().UTC()
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		password := value(in.TemporaryPassword)
		if in.TemporaryPassword != nil {
			if err := checkPolicy(p, in.TemporaryPassword); err != nil {
				return err
			}
		} else {
			password = temporaryPassword()
		}

		u, before, err := s.invited(tx, scope, p, in, action == "RESEND", now)
		if err != nil {
			return err
		}

		if setErr := setPassword(p, u, password, false, now); setErr != nil {
			return setErr
		}

		if action != "SUPPRESS" {
			for _, medium := range mediums {
				if u.Attributes[attrOf(medium)] != "" {
					subject, template := inviteMessage(p, medium)
					out.send(scope, p, u, medium, subject, template, password)
				}
			}
		}

		created = u
		return saveUser(tx, scope, p, before, u, false)
	})
	if err != nil {
		return nil, err
	}

	out.flush(s.deliveries)
	result := created.output(nil)
	return &api.AdminCreateUserResponse{User: &result}, nil
}

// invited returns the user AdminCreateUser creates, or with resend the
// existing user whose invitation it sends again, and that user as it was.
func (s *Service) invited(
	tx storage.ReadTx, scope request.Scope, p *pool, in *api.AdminCreateUserRequest, resend bool, now time.Time,
) (u, before *user, err error) {
	if resend {
		if u, err = findUser(tx, scope, p, in.Username); err != nil {
			return nil, nil, err
		}

		if u.Status != statusForceChange {
			return nil, nil, awserror.New("UnsupportedUserStateException",
				"Resend not possible. "+u.Username+" status is not FORCE_CHANGE_PASSWORD.")
		}

		copied := *u
		return u, &copied, nil
	}

	if u, err = newUser(p, in.Username, in.UserAttributes, now); err != nil {
		return nil, nil, err
	}

	if _, exists := tx.Get(userKey(scope, p, u.Username)); exists {
		return nil, nil, awserror.New("UsernameExistsException", "User account already exists")
	}

	return u, nil, nil
}

func attrOf(medium string) string {
	if medium == mediumSMS {
		return attrPhone
	}

	return attrEmail
}

func (s *Service) AdminGetUser(ctx context.Context, in *api.AdminGetUserRequest) (*api.AdminGetUserResponse, error) {
	var out *api.AdminGetUserResponse
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		u, err := findUser(tx, scope, p, in.Username)
		if err != nil {
			return err
		}

		o := u.output(nil)
		out = &api.AdminGetUserResponse{
			Username: o.Username, UserAttributes: o.Attributes, UserCreateDate: o.UserCreateDate,
			UserLastModifiedDate: o.UserLastModifiedDate, Enabled: o.Enabled, UserStatus: o.UserStatus,
		}
		return nil
	})

	return out, err
}

func (s *Service) AdminDeleteUser(
	ctx context.Context,
	in *api.AdminDeleteUserRequest,
) (*api.AdminDeleteUserOutput, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		u, err := findUser(tx, scope, p, in.Username)
		if err != nil {
			return err
		}

		deleteUser(tx, scope, p, u)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AdminDeleteUserOutput{}, nil
}

func (s *Service) AdminUpdateUserAttributes(
	ctx context.Context,
	in *api.AdminUpdateUserAttributesRequest,
) (*api.AdminUpdateUserAttributesResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, scope request.Scope, p *pool, u *user, out *outbox) error {
			changed, err := setAttributes(p, u, in.UserAttributes, false)
			if err != nil {
				return err
			}

			s.verifyChanged(scope, p, u, changed, out)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminUpdateUserAttributesResponse{}, nil
}

// verifyChanged sends a verification code for each changed attribute the
// pool verifies automatically and that is not yet verified, and returns where
// the codes went.
func (s *Service) verifyChanged(scope request.Scope, p *pool, u *user, changed []string, out *outbox) []sent {
	var sentTo []sent
	for _, attr := range changed {
		if (attr == attrEmail || attr == attrPhone) && hasAutoVerify(p, attr) && u.Attributes[attr] != "" &&
			u.Attributes[attr+verifiedSuffix] != valueTrue {
			subject, template := verificationMessage(p, mediumOf(attr))
			secret := u.setCode(purposeVerify+attr, attr, s.now().UTC(), codeValidity)
			sentTo = append(sentTo, out.send(scope, p, u, mediumOf(attr), subject, template, secret))
		}
	}

	return sentTo
}

func (s *Service) AdminDeleteUserAttributes(
	ctx context.Context,
	in *api.AdminDeleteUserAttributesRequest,
) (*api.AdminDeleteUserAttributesResponse, error) {
	attrs := make([]api.AttributeType, 0, len(in.UserAttributeNames))
	for _, name := range in.UserAttributeNames {
		attrs = append(attrs, api.AttributeType{Name: new(name), Value: new("")})
	}

	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, p *pool, u *user, _ *outbox) error {
			_, err := setAttributes(p, u, attrs, false)
			return err
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminDeleteUserAttributesResponse{}, nil
}

func (s *Service) AdminSetUserPassword(
	ctx context.Context,
	in *api.AdminSetUserPasswordRequest,
) (*api.AdminSetUserPasswordResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, p *pool, u *user, _ *outbox) error {
			if err := checkPolicy(p, in.Password); err != nil {
				return err
			}

			return setPassword(p, u, *in.Password, value(in.Permanent), s.now().UTC())
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminSetUserPasswordResponse{}, nil
}

func (s *Service) AdminEnableUser(
	ctx context.Context,
	in *api.AdminEnableUserRequest,
) (*api.AdminEnableUserResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, _ *pool, u *user, _ *outbox) error {
			u.Disabled = false
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminEnableUserResponse{}, nil
}

// AdminDisableUser disables a user, which also signs it out everywhere.
func (s *Service) AdminDisableUser(
	ctx context.Context,
	in *api.AdminDisableUserRequest,
) (*api.AdminDisableUserResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, _ *pool, u *user, _ *outbox) error {
			u.Disabled, u.Sessions = true, nil
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminDisableUserResponse{}, nil
}

func (s *Service) AdminConfirmSignUp(
	ctx context.Context,
	in *api.AdminConfirmSignUpRequest,
) (*api.AdminConfirmSignUpResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, _ request.Scope, _ *pool, u *user, _ *outbox) error {
			if u.Status != statusUnconfirmed {
				return awserror.New("NotAuthorizedException",
					"User cannot be confirmed. Current status is "+u.Status)
			}

			u.Status = statusConfirmed
			delete(u.Codes, purposeSignUp)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminConfirmSignUpResponse{}, nil
}

// AdminResetUserPassword requires a new password: the user's password stops
// working, and a code for ConfirmForgotPassword goes to its verified email
// address or phone number.
func (s *Service) AdminResetUserPassword(
	ctx context.Context,
	in *api.AdminResetUserPasswordRequest,
) (*api.AdminResetUserPasswordResponse, error) {
	err := s.changeUser(ctx, in.UserPoolId, in.Username,
		func(_ storage.WriteTx, scope request.Scope, p *pool, u *user, out *outbox) error {
			if _, err := s.sendReset(scope, p, u, out); err != nil {
				return err
			}

			u.Status, u.Sessions = statusReset, nil
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AdminResetUserPasswordResponse{}, nil
}

// sendReset sends u a password reset code by the pool's recovery mechanisms.
func (s *Service) sendReset(scope request.Scope, p *pool, u *user, out *outbox) (sent, error) {
	for _, attr := range recoveryAttributes(p) {
		if u.Attributes[attr] != "" && u.Attributes[attr+verifiedSuffix] == valueTrue {
			subject, template := verificationMessage(p, mediumOf(attr))
			secret := u.setCode(purposePassword, attr, s.now().UTC(), time.Hour)
			return out.send(scope, p, u, mediumOf(attr), subject, template, secret), nil
		}
	}

	return sent{}, invalidParameter("Cannot reset password for the user as there is no registered/verified email or " +
		"phone_number")
}

// recoveryAttributes returns the attributes a pool sends password reset codes
// to, in priority order.
func recoveryAttributes(p *pool) []string {
	r := p.Settings.AccountRecoverySetting
	if r == nil {
		return []string{attrEmail, attrPhone}
	}

	mechanisms := slices.Clone(r.RecoveryMechanisms)
	slices.SortFunc(mechanisms, func(a, b api.RecoveryOptionType) int {
		return int(value(a.Priority) - value(b.Priority))
	})

	var out []string
	for _, m := range mechanisms {
		if attr, ok := strings.CutPrefix(value(m.Name), "verified_"); ok {
			out = append(out, attr)
		}
	}

	return out
}

var userFilter = regexp.MustCompile(`^\s*([\w:]+)\s*(=|\^=)\s*"(.*)"\s*$`)

// filterable are the attributes ListUsers filters by.
var filterable = []string{
	"username", attrEmail, attrPhone, "name", "given_name", "family_name", attrPreferred, "cognito:user_status",
	"status", attrSub,
}

// userMatcher compiles a ListUsers filter such as `email ^= "a"`.
func userMatcher(p *pool, filter *string) (func(*user) bool, error) {
	if value(filter) == "" {
		return func(*user) bool { return true }, nil
	}

	m := userFilter.FindStringSubmatch(*filter)
	if len(m) != 4 || !slices.Contains(filterable, m[1]) {
		return nil, invalidParameter("Error while parsing filter.")
	}

	attr, prefixMatch, want := m[1], m[2] == "^=", m[3]
	return func(u *user) bool {
		got := u.Attributes[attr]
		switch attr {
		case "username":
			got = u.Username
			if p.caseInsensitive() {
				got, want = strings.ToLower(got), strings.ToLower(want)
			}
		case "cognito:user_status":
			got = u.Status
		case "status":
			got = map[bool]string{false: "Enabled", true: "Disabled"}[u.Disabled]
		}

		if prefixMatch {
			return strings.HasPrefix(got, want)
		}

		return got == want
	}, nil
}

// users returns the pool's users that match, in username order.
func users(tx storage.ReadTx, scope request.Scope, p *pool, match func(*user) bool) ([]*user, error) {
	var out []*user
	for _, k := range tx.Keys(userPrefix(scope, p.ID)) {
		u, _, err := read[user](tx, k)
		if err != nil {
			return nil, err
		}

		if match(u) {
			out = append(out, u)
		}
	}

	return out, nil
}

func (s *Service) ListUsers(ctx context.Context, in *api.ListUsersRequest) (*api.ListUsersResponse, error) {
	n, err := limit(in.Limit, "Limit")
	if err != nil {
		return nil, err
	}

	var found []*user
	err = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		for _, name := range in.AttributesToGet {
			if p.attribute(name) == nil {
				return invalidParameter("Invalid attribute %s in AttributesToGet.", name)
			}
		}

		match, matchErr := userMatcher(p, in.Filter)
		if matchErr != nil {
			return matchErr
		}

		var listErr error
		found, listErr = users(tx, scope, p, match)
		return listErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(found, func(u *user) string { return u.Username }, in.PaginationToken, n)
	if err != nil {
		return nil, err
	}

	out := &api.ListUsersResponse{Users: []api.UserType{}, PaginationToken: next}
	for _, u := range selected {
		out.Users = append(out.Users, u.output(in.AttributesToGet))
	}

	return out, nil
}

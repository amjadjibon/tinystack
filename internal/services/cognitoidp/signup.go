package cognitoidp

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// details converts where a message went to its wire form.
func details(d sent) *api.CodeDeliveryDetailsType {
	return &api.CodeDeliveryDetailsType{
		Destination: new(d.Destination), DeliveryMedium: new(d.Medium), AttributeName: new(d.Attribute),
	}
}

// sendSignUpCode sends u a sign-up confirmation code to the first attribute
// the pool verifies, if any.
func (s *Service) sendSignUpCode(scope request.Scope, p *pool, u *user, out *outbox) *api.CodeDeliveryDetailsType {
	medium := verificationMedium(p, u)
	if medium == "" {
		return nil
	}

	subject, template := verificationMessage(p, medium)
	secret := u.setCode(purposeSignUp, attrOf(medium), s.now().UTC(), codeValidity)
	return details(out.send(scope, p, u, medium, subject, template, secret))
}

// byClientUser is byClient for the operations that name a user, and saves
// the user after fn.
func (s *Service) byClientUser(
	ctx context.Context,
	clientID, name, hash *string,
	fn func(tx storage.WriteTx, scope request.Scope, p *pool, u *user, out *outbox) error,
) error {
	var out outbox
	err := s.byClient(ctx, clientID, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		u, err := findSigningIn(tx, scope, p, c, value(name))
		if err != nil {
			return err
		}

		if hashErr := checkSecretHash(c, hash, value(name), u.Username); hashErr != nil {
			return hashErr
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

func (s *Service) SignUp(ctx context.Context, in *api.SignUpRequest) (*api.SignUpResponse, error) {
	var (
		out      outbox
		response *api.SignUpResponse
	)
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		if value(p.Settings.AdminCreateUserConfig.AllowAdminCreateUserOnly) {
			return notAuthorized("SignUp is not permitted for this user pool")
		}

		if hashErr := checkSecretHash(c, in.SecretHash, value(in.Username)); hashErr != nil {
			return hashErr
		}

		if in.Password == nil {
			return invalidParameter("TinyStack requires a Password: passwordless sign-up is not supported.")
		}

		for _, a := range in.UserAttributes {
			if !writable(c, value(a.Name)) {
				return notAuthorized("A client attempted to write unauthorized attribute")
			}
		}

		now := s.now().UTC()
		u, err := newUser(p, in.Username, in.UserAttributes, now)
		if err != nil {
			return err
		}

		if _, exists := tx.Get(userKey(scope, p, u.Username)); exists {
			return awserror.New("UsernameExistsException", "User already exists")
		}

		if policyErr := checkPolicy(p, in.Password); policyErr != nil {
			return policyErr
		}

		if setErr := setPassword(p, u, *in.Password, true, now); setErr != nil {
			return setErr
		}

		u.Status = statusUnconfirmed
		response = &api.SignUpResponse{
			UserConfirmed: new(false), UserSub: new(u.Attributes[attrSub]),
			CodeDeliveryDetails: s.sendSignUpCode(scope, p, u, &out),
		}
		return saveUser(tx, scope, p, nil, u, false)
	})
	if err != nil {
		return nil, err
	}

	out.flush(s.deliveries)
	return response, nil
}

// ConfirmSignUp confirms a user with the code sign-up sent, which also
// verifies the attribute the code went to.
func (s *Service) ConfirmSignUp(ctx context.Context, in *api.ConfirmSignUpRequest) (*api.ConfirmSignUpResponse, error) {
	var force bool
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		u, err := findSigningIn(tx, scope, p, c, value(in.Username))
		if err != nil {
			return err
		}

		if hashErr := checkSecretHash(c, in.SecretHash, value(in.Username), u.Username); hashErr != nil {
			return hashErr
		}

		if u.Status != statusUnconfirmed {
			return notAuthorized("User cannot be confirmed. Current status is " + u.Status)
		}

		before := *u
		before.Attributes = cloneMap(u.Attributes)
		attr := u.Codes[purposeSignUp].Attribute
		if codeErr := u.useCode(purposeSignUp, value(in.ConfirmationCode), s.now().UTC()); codeErr != nil {
			return codeErr
		}

		u.Status, u.Modified = statusConfirmed, s.now().UTC()
		if attr != "" {
			u.Attributes[attr+verifiedSuffix] = valueTrue
		}

		force = value(in.ForceAliasCreation)
		return saveUser(tx, scope, p, &before, u, force)
	})
	if err != nil {
		return nil, err
	}

	return &api.ConfirmSignUpResponse{}, nil
}

func (s *Service) ResendConfirmationCode(
	ctx context.Context,
	in *api.ResendConfirmationCodeRequest,
) (*api.ResendConfirmationCodeResponse, error) {
	var d *api.CodeDeliveryDetailsType
	err := s.byClientUser(ctx, in.ClientId, in.Username, in.SecretHash,
		func(_ storage.WriteTx, scope request.Scope, p *pool, u *user, out *outbox) error {
			if u.Status != statusUnconfirmed {
				return invalidParameter("User is already confirmed.")
			}

			if d = s.sendSignUpCode(scope, p, u, out); d == nil {
				return invalidParameter("Auto verification not turned on.")
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.ResendConfirmationCodeResponse{CodeDeliveryDetails: d}, nil
}

func (s *Service) ForgotPassword(
	ctx context.Context,
	in *api.ForgotPasswordRequest,
) (*api.ForgotPasswordResponse, error) {
	var d sent
	err := s.byClientUser(ctx, in.ClientId, in.Username, in.SecretHash,
		func(_ storage.WriteTx, scope request.Scope, p *pool, u *user, out *outbox) error {
			if u.Disabled {
				return notAuthorized("User is disabled.")
			}

			var sendErr error
			d, sendErr = s.sendReset(scope, p, u, out)
			return sendErr
		})
	if err != nil {
		return nil, err
	}

	return &api.ForgotPasswordResponse{CodeDeliveryDetails: details(d)}, nil
}

func (s *Service) ConfirmForgotPassword(
	ctx context.Context,
	in *api.ConfirmForgotPasswordRequest,
) (*api.ConfirmForgotPasswordResponse, error) {
	err := s.byClientUser(ctx, in.ClientId, in.Username, in.SecretHash,
		func(_ storage.WriteTx, _ request.Scope, p *pool, u *user, _ *outbox) error {
			now := s.now().UTC()
			if codeErr := u.useCode(purposePassword, value(in.ConfirmationCode), now); codeErr != nil {
				return codeErr
			}

			if policyErr := checkPolicy(p, in.Password); policyErr != nil {
				return policyErr
			}

			status := u.Status
			if setErr := setPassword(p, u, *in.Password, true, now); setErr != nil {
				return setErr
			}

			// A reset password does not confirm a user who never confirmed
			// signing up.
			if status == statusUnconfirmed {
				u.Status = statusUnconfirmed
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.ConfirmForgotPasswordResponse{}, nil
}

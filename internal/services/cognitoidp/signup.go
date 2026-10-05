package cognitoidp

import (
	"context"
	"errors"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
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
		u, err := findSigningIn(tx, scope, p, c, common.Value(name))
		if err != nil {
			return err
		}

		if hashErr := checkSecretHash(c, hash, common.Value(name), u.Username); hashErr != nil {
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

// SignUp creates an unconfirmed user. A pool with a pre sign-up trigger runs
// the transaction twice: the first stops before writing to call the trigger
// with the user it would create, and the second creates that user (same sub
// and username) as the trigger's response says.
func (s *Service) SignUp(ctx context.Context, in *api.SignUpRequest) (*api.SignUpResponse, error) {
	var (
		out      outbox
		response *api.SignUpResponse
		// pending is the pre sign-up call the first run asked for, and
		// decided the trigger's response.
		pending, confirmed *call
		decided            *preSignUp
	)
	run := func() error {
		return s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			out, confirmed = outbox{}, nil
			if common.Value(p.Settings.AdminCreateUserConfig.AllowAdminCreateUserOnly) {
				return notAuthorized("SignUp is not permitted for this user pool")
			}

			if hashErr := checkSecretHash(c, in.SecretHash, common.Value(in.Username)); hashErr != nil {
				return hashErr
			}

			if in.Password == nil {
				return invalidParameter("TinyStack requires a Password: passwordless sign-up is not supported.")
			}

			for _, a := range in.UserAttributes {
				if !writable(c, common.Value(a.Name)) {
					return notAuthorized("A client attempted to write unauthorized attribute")
				}
			}

			now := s.now().UTC()
			u, err := newUser(p, in.Username, in.UserAttributes, now)
			if err != nil {
				return err
			}

			if pending != nil {
				if u.Username == u.Attributes[attrSub] {
					u.Username = pending.event.UserName
				}

				u.Attributes[attrSub] = pending.event.Request["userAttributes"].(map[string]string)[attrSub]
			}

			if _, exists := tx.Get(userKey(scope, p, u.Username)); exists {
				return awserror.New("UsernameExistsException", "User already exists")
			}

			if policyErr := checkPolicy(p, in.Password); policyErr != nil {
				return policyErr
			}

			if function := common.Value(p.triggers().PreSignUp); function != "" && decided == nil {
				pending = newCall(scope, p, c.ID, function, "PreSignUp", "PreSignUp_SignUp", u, in.ClientMetadata)
				attrs := pending.event.Request["userAttributes"].(map[string]string)
				delete(attrs, attrUserStatus)
				pending.event.Request["validationData"] = attributeMap(in.ValidationData)
				return errTrigger
			}

			if setErr := setPassword(p, u, *in.Password, true, now); setErr != nil {
				return setErr
			}

			u.Status = statusUnconfirmed
			response = &api.SignUpResponse{UserConfirmed: new(false), UserSub: new(u.Attributes[attrSub])}
			if decided != nil {
				decided.applyTo(u)
			}

			if u.Status == statusConfirmed {
				response.UserConfirmed = new(true)
				confirmed = postConfirmation(scope, p, c.ID, "PostConfirmation_ConfirmSignUp", u, in.ClientMetadata)
			} else {
				response.CodeDeliveryDetails = s.sendSignUpCode(scope, p, u, &out)
			}

			return saveUser(tx, scope, p, nil, u, false)
		})
	}

	err := run()
	if errors.Is(err, errTrigger) {
		decided = new(preSignUp)
		if err = s.invoke(ctx, pending, decided); err == nil {
			err = run()
		}
	}

	if err != nil {
		return nil, err
	}

	out.flush(s.deliveries)
	if invokeErr := s.invoke(ctx, confirmed, nil); invokeErr != nil {
		return nil, invokeErr
	}

	return response, nil
}

// applyTo confirms and verifies a new user as a pre sign-up trigger decided.
func (r *preSignUp) applyTo(u *user) {
	if r.AutoConfirmUser {
		u.Status = statusConfirmed
	}

	if r.AutoVerifyEmail && u.Attributes[attrEmail] != "" {
		u.Attributes[attrEmail+verifiedSuffix] = valueTrue
	}

	if r.AutoVerifyPhone && u.Attributes[attrPhone] != "" {
		u.Attributes[attrPhone+verifiedSuffix] = valueTrue
	}
}

// attributeMap converts attributes to the map triggers receive.
func attributeMap(attrs []api.AttributeType) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, a := range attrs {
		out[common.Value(a.Name)] = common.Value(a.Value)
	}

	return out
}

// ConfirmSignUp confirms a user with the code sign-up sent, which also
// verifies the attribute the code went to.
func (s *Service) ConfirmSignUp(ctx context.Context, in *api.ConfirmSignUpRequest) (*api.ConfirmSignUpResponse, error) {
	var confirmed *call
	err := s.byClient(ctx, in.ClientId, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		u, err := findSigningIn(tx, scope, p, c, common.Value(in.Username))
		if err != nil {
			return err
		}

		if hashErr := checkSecretHash(c, in.SecretHash, common.Value(in.Username), u.Username); hashErr != nil {
			return hashErr
		}

		if u.Status != statusUnconfirmed {
			return notAuthorized("User cannot be confirmed. Current status is " + u.Status)
		}

		before := *u
		before.Attributes = cloneMap(u.Attributes)
		attr := u.Codes[purposeSignUp].Attribute
		if codeErr := u.useCode(purposeSignUp, common.Value(in.ConfirmationCode), s.now().UTC()); codeErr != nil {
			return codeErr
		}

		u.Status, u.Modified = statusConfirmed, s.now().UTC()
		if attr != "" {
			u.Attributes[attr+verifiedSuffix] = valueTrue
		}

		confirmed = postConfirmation(scope, p, c.ID, "PostConfirmation_ConfirmSignUp", u, in.ClientMetadata)
		return saveUser(tx, scope, p, &before, u, common.Value(in.ForceAliasCreation))
	})
	if err == nil {
		err = s.invoke(ctx, confirmed, nil)
	}

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
	var confirmed *call
	err := s.byClientUser(ctx, in.ClientId, in.Username, in.SecretHash,
		func(_ storage.WriteTx, scope request.Scope, p *pool, u *user, _ *outbox) error {
			now := s.now().UTC()
			if codeErr := u.useCode(purposePassword, common.Value(in.ConfirmationCode), now); codeErr != nil {
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

			confirmed = postConfirmation(
				scope,
				p,
				common.Value(in.ClientId),
				"PostConfirmation_ConfirmForgotPassword",
				u,
				in.ClientMetadata,
			)
			return nil
		})
	if err == nil {
		err = s.invoke(ctx, confirmed, nil)
	}

	if err != nil {
		return nil, err
	}

	return &api.ConfirmForgotPasswordResponse{}, nil
}

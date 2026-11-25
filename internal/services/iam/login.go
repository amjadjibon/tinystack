package iam

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	passwordSymbols    = `!@#$%^&*()_+-=[]{}|'`
	passwordIterations = 100000
	passwordKeyBytes   = 32
	passwordSaltBytes  = 16
)

type passwordHash struct {
	Salt []byte
	Hash []byte
}

type loginProfile struct {
	History               []passwordHash `json:",omitempty"`
	Salt                  []byte
	Hash                  []byte
	CreatedAt             int64
	UpdatedAt             int64
	PasswordResetRequired bool
}

func hashPassword(password string) (passwordHash, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return passwordHash{}, err
	}

	derived, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyBytes)
	if err != nil {
		return passwordHash{}, err
	}

	return passwordHash{Salt: salt, Hash: derived}, nil
}

func verifyPassword(password string, stored passwordHash) bool {
	if len(stored.Salt) == 0 || len(stored.Hash) == 0 {
		return false
	}

	derived, err := pbkdf2.Key(sha256.New, password, stored.Salt, passwordIterations, len(stored.Hash))
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(derived, stored.Hash) == 1
}

func passwordViolation(msg string) error {
	return awserror.New("PasswordPolicyViolationException", msg)
}

func checkPasswordPolicy(tx storage.ReadTx, account, password string, history []passwordHash) error {
	if password == "" || len(password) > maxPasswordLength {
		return validation("Password must be between 1 and %d characters long.", maxPasswordLength)
	}

	rec, ok, err := storage.Read[passwordPolicyRecord](tx, key(account, kindPasswordPolicy, "current"))
	if err != nil || !ok {
		return err
	}

	if int64(len(password)) < rec.MinimumPasswordLength {
		return passwordViolation("Password does not conform to the account password policy: minimum length.")
	}

	var hasUpper, hasLower, hasDigit, hasSymbol bool
	for _, r := range password {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune(passwordSymbols, r):
			hasSymbol = true
		}
	}

	switch {
	case rec.RequireUppercaseCharacters && !hasUpper:
		return passwordViolation("Password must contain at least one uppercase character.")
	case rec.RequireLowercaseCharacters && !hasLower:
		return passwordViolation("Password must contain at least one lowercase character.")
	case rec.RequireNumbers && !hasDigit:
		return passwordViolation("Password must contain at least one number.")
	case rec.RequireSymbols && !hasSymbol:
		return passwordViolation("Password must contain at least one symbol.")
	}

	if rec.PasswordReusePrevention > 0 && len(history) > 0 {
		limit := min(int(rec.PasswordReusePrevention), len(history))
		for _, prev := range history[:limit] {
			if verifyPassword(password, prev) {
				return passwordViolation("Password cannot reuse a recently used password.")
			}
		}
	}

	return nil
}

func loginProfileOutput(userName string, lp *loginProfile) *api.LoginProfile {
	return &api.LoginProfile{
		UserName:              new(userName),
		CreateDate:            millis(lp.CreatedAt),
		PasswordResetRequired: new(lp.PasswordResetRequired),
	}
}

func (s *Service) CreateLoginProfile(
	ctx context.Context,
	in *api.CreateLoginProfileRequest,
) (*api.CreateLoginProfileResponse, error) {
	if err := required("password", in.Password); err != nil {
		return nil, err
	}

	var out *api.LoginProfile
	err := s.updateUser(ctx, in.UserName, func(tx storage.WriteTx, account string, u *user) error {
		if u.LoginProfile != nil {
			return alreadyExists("Login Profile for User %s already exists.", u.Name)
		}

		if policyErr := checkPasswordPolicy(tx, account, *in.Password, nil); policyErr != nil {
			return policyErr
		}

		hashed, hashErr := hashPassword(*in.Password)
		if hashErr != nil {
			return hashErr
		}

		now := s.now().UnixMilli()
		u.LoginProfile = &loginProfile{
			Salt:                  hashed.Salt,
			Hash:                  hashed.Hash,
			History:               []passwordHash{hashed},
			CreatedAt:             now,
			UpdatedAt:             now,
			PasswordResetRequired: common.Value(in.PasswordResetRequired),
		}
		out = loginProfileOutput(u.Name, u.LoginProfile)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLoginProfileResponse{LoginProfile: out}, nil
}

func (s *Service) GetLoginProfile(
	ctx context.Context,
	in *api.GetLoginProfileRequest,
) (*api.GetLoginProfileResponse, error) {
	var out *api.LoginProfile
	err := s.viewUser(ctx, in.UserName, func(_ string, u *user) error {
		if u.LoginProfile == nil {
			return noSuchEntity("Login Profile for User %s cannot be found.", u.Name)
		}

		out = loginProfileOutput(u.Name, u.LoginProfile)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetLoginProfileResponse{LoginProfile: out}, nil
}

func (s *Service) UpdateLoginProfile(
	ctx context.Context,
	in *api.UpdateLoginProfileRequest,
) (*api.UpdateLoginProfileOutput, error) {
	err := s.updateUser(ctx, in.UserName, func(tx storage.WriteTx, account string, u *user) error {
		if u.LoginProfile == nil {
			return noSuchEntity("Login Profile for User %s cannot be found.", u.Name)
		}

		if in.Password != nil {
			if policyErr := checkPasswordPolicy(tx, account, *in.Password, u.LoginProfile.History); policyErr != nil {
				return policyErr
			}

			hashed, hashErr := hashPassword(*in.Password)
			if hashErr != nil {
				return hashErr
			}

			u.LoginProfile.Salt = hashed.Salt
			u.LoginProfile.Hash = hashed.Hash
			u.LoginProfile.History = prependPasswordHistory(u.LoginProfile.History, hashed)
			u.LoginProfile.UpdatedAt = s.now().UnixMilli()
		}

		if in.PasswordResetRequired != nil {
			u.LoginProfile.PasswordResetRequired = *in.PasswordResetRequired
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateLoginProfileOutput{}, nil
}

func (s *Service) DeleteLoginProfile(
	ctx context.Context,
	in *api.DeleteLoginProfileRequest,
) (*api.DeleteLoginProfileOutput, error) {
	err := s.updateUser(ctx, in.UserName, func(_ storage.WriteTx, _ string, u *user) error {
		if u.LoginProfile == nil {
			return noSuchEntity("Login Profile for User %s cannot be found.", u.Name)
		}

		u.LoginProfile = nil
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteLoginProfileOutput{}, nil
}

func (s *Service) ChangePassword(
	ctx context.Context,
	in *api.ChangePasswordRequest,
) (*api.ChangePasswordOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("oldPassword", in.OldPassword); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := required("newPassword", in.NewPassword); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		caller, isUser, callerErr := callerUser(tx, &m)
		if callerErr != nil {
			return callerErr
		}

		if !isUser {
			return awserror.New("InvalidUserTypeException", "Cannot change password for non-IAM user.")
		}

		u, uk, loadErr := loadUser(tx, m.AccountID, &caller)
		if loadErr != nil {
			return loadErr
		}

		if u.LoginProfile == nil {
			return noSuchEntity("Login Profile for User %s cannot be found.", u.Name)
		}

		if !verifyPassword(*in.OldPassword, passwordHash{Salt: u.LoginProfile.Salt, Hash: u.LoginProfile.Hash}) {
			return awserror.New("AccessDenied", "The old password is incorrect.")
		}

		if policyErr := checkPasswordPolicy(
			tx, m.AccountID, *in.NewPassword, u.LoginProfile.History,
		); policyErr != nil {
			return policyErr
		}

		hashed, hashErr := hashPassword(*in.NewPassword)
		if hashErr != nil {
			return hashErr
		}

		u.LoginProfile.Salt = hashed.Salt
		u.LoginProfile.Hash = hashed.Hash
		u.LoginProfile.History = prependPasswordHistory(u.LoginProfile.History, hashed)
		u.LoginProfile.PasswordResetRequired = false
		u.LoginProfile.UpdatedAt = s.now().UnixMilli()
		return storage.Write(tx, uk, u)
	})
	if err != nil {
		return nil, err
	}

	return &api.ChangePasswordOutput{}, nil
}

func prependPasswordHistory(history []passwordHash, entry passwordHash) []passwordHash {
	updated := slices.Insert(history, 0, entry)
	if len(updated) > maxPasswordReuse {
		updated = updated[:maxPasswordReuse]
	}

	return updated
}

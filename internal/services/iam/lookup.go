package iam

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/storage"
)

// Role is what STS needs to know to assume a role.
type Role struct {
	Account     string
	Name        string
	ID          string
	ARN         string
	TrustPolicy string
	// MaxSessionDuration is in seconds.
	MaxSessionDuration int64
}

// RoleByARN returns the role that arn names, in any account, or false when
// there is none. The path in arn must match the role's.
func (s *Service) RoleByARN(ctx context.Context, arn string) (Role, bool, error) {
	rest, ok := strings.CutPrefix(arn, "arn:aws:iam::")
	if !ok {
		return Role{}, false, nil
	}

	account, resource, ok := strings.Cut(rest, ":")
	if !ok || !accountID.MatchString(account) || !strings.HasPrefix(resource, kindRole+"/") {
		return Role{}, false, nil
	}

	i := strings.LastIndex(resource, "/")
	path, name := resource[len(kindRole):i+1], resource[i+1:]
	if !roleName.MatchString(name) {
		return Role{}, false, nil
	}

	var out Role
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		r, exists, err := read[role](tx, key(account, kindRole, name))
		if err != nil || !exists || r.Path != path {
			return err
		}

		found = true
		out = Role{
			Account:            account,
			Name:               r.Name,
			ID:                 r.ID,
			ARN:                roleARN(account, r),
			TrustPolicy:        r.AssumeRolePolicy,
			MaxSessionDuration: r.MaxSessionDuration,
		}

		return nil
	})

	return out, found && err == nil, err
}

// Key is an active access key of an IAM user: the secret the gateway verifies
// signatures with, and the user the key acts as.
type Key struct {
	Account  string
	Secret   string
	UserName string
	UserID   string
	UserARN  string
}

// AccessKey returns the active access key with ID id, or false when there is
// none or it is inactive.
func (s *Service) AccessKey(ctx context.Context, id string) (Key, bool, error) {
	var out Key
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		k, exists, err := read[accessKey](tx, accessKeyPrefix+id)
		if err != nil || !exists || k.Status != keyActive || id == "" {
			return err
		}

		u, _, err := loadUser(tx, k.Account, &k.UserName)
		if err != nil {
			return err
		}

		found = true
		out = Key{
			Account:  k.Account,
			Secret:   k.Secret,
			UserName: u.Name,
			UserID:   u.ID,
			UserARN:  userARN(k.Account, u),
		}

		return nil
	})

	return out, found && err == nil, err
}

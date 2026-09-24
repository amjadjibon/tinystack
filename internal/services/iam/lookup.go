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

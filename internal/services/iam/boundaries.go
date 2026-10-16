package iam

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func boundaryOutput(boundary string) *api.AttachedPermissionsBoundary {
	if boundary == "" {
		return nil
	}

	return &api.AttachedPermissionsBoundary{
		PermissionsBoundaryType: new("PermissionsBoundaryPolicy"), PermissionsBoundaryArn: new(boundary),
	}
}

// setBoundary validates before changing either the identity or usage counts.
// nil removes the boundary; Create callers use nil for an absent boundary.
func setBoundary(tx storage.WriteTx, account string, current, requested *string) error {
	next := common.Value(requested)
	if requested != nil {
		if _, err := managedDocument(tx, account, next); err != nil {
			return err
		}
	}

	if *current == next {
		return nil
	}

	for _, change := range []struct {
		arn   string
		delta int64
	}{{next, 1}, {*current, -1}} {
		if change.arn == "" {
			continue
		}

		ref, err := parsePolicyARN(change.arn)
		if err != nil {
			return err
		}

		if ref.awsManaged() {
			continue
		}

		p, pk, err := loadPolicy(tx, account, ref)
		if err != nil {
			return err
		}

		p.BoundaryUses += change.delta
		if err := storage.Write(tx, pk, p); err != nil {
			return err
		}
	}

	*current = next
	return nil
}

func (s *Service) PutUserPermissionsBoundary(ctx context.Context,
	in *api.PutUserPermissionsBoundaryRequest,
) (*api.PutUserPermissionsBoundaryOutput, error) {
	if err := required("permissionsBoundary", in.PermissionsBoundary); err != nil {
		return nil, err
	}

	err := s.updateUser(ctx, in.UserName, func(tx storage.WriteTx, account string, u *user) error {
		return setBoundary(tx, account, &u.Boundary, in.PermissionsBoundary)
	})
	return &api.PutUserPermissionsBoundaryOutput{}, err
}

func (s *Service) DeleteUserPermissionsBoundary(ctx context.Context,
	in *api.DeleteUserPermissionsBoundaryRequest,
) (*api.DeleteUserPermissionsBoundaryOutput, error) {
	err := s.updateUser(ctx, in.UserName, func(tx storage.WriteTx, account string, u *user) error {
		return setBoundary(tx, account, &u.Boundary, nil)
	})
	return &api.DeleteUserPermissionsBoundaryOutput{}, err
}

func (s *Service) PutRolePermissionsBoundary(ctx context.Context,
	in *api.PutRolePermissionsBoundaryRequest,
) (*api.PutRolePermissionsBoundaryOutput, error) {
	if err := required("permissionsBoundary", in.PermissionsBoundary); err != nil {
		return nil, err
	}

	err := s.updateRole(ctx, in.RoleName, func(tx storage.WriteTx, account string, r *role) error {
		return setBoundary(tx, account, &r.Boundary, in.PermissionsBoundary)
	})
	return &api.PutRolePermissionsBoundaryOutput{}, err
}

func (s *Service) DeleteRolePermissionsBoundary(ctx context.Context,
	in *api.DeleteRolePermissionsBoundaryRequest,
) (*api.DeleteRolePermissionsBoundaryOutput, error) {
	err := s.updateRole(ctx, in.RoleName, func(tx storage.WriteTx, account string, r *role) error {
		return setBoundary(tx, account, &r.Boundary, nil)
	})
	return &api.DeleteRolePermissionsBoundaryOutput{}, err
}

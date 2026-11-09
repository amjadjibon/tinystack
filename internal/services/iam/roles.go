package iam

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func roleARN(account string, r *role) string { return arn(account, kindRole+r.Path+r.Name) }

// loadRole returns the role named name and its storage key.
func loadRole(tx storage.ReadTx, account string, name *string) (*role, string, error) {
	if err := checkName("roleName", name, roleName); err != nil {
		return nil, "", err
	}

	rk := key(account, kindRole, *name)
	r, ok, err := storage.Read[role](tx, rk)
	if err != nil {
		return nil, "", err
	}

	if !ok {
		return nil, "", noSuchEntity("The role with name %s cannot be found.", *name)
	}

	return r, rk, nil
}

// viewRole runs fn on the named role.
func (s *Service) viewRole(ctx context.Context, name *string, fn func(account string, r *role) error) error {
	m, err := request.Global(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, loadErr := loadRole(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		return fn(m.AccountID, r)
	})
}

// updateRole runs fn on the named role and stores the result, all in one
// transaction.
func (s *Service) updateRole(
	ctx context.Context,
	name *string,
	fn func(tx storage.WriteTx, account string, r *role) error,
) error {
	m, err := request.Global(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, rk, loadErr := loadRole(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, m.AccountID, r); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, rk, r)
	})
}

// roleOutput describes a role. Tags are only in single-role responses, as in AWS.
func roleOutput(account string, r *role, tags bool) *api.Role {
	out := &api.Role{
		PermissionsBoundary:      boundaryOutput(r.Boundary),
		Path:                     new(r.Path),
		RoleName:                 new(r.Name),
		RoleId:                   new(r.ID),
		Arn:                      new(roleARN(account, r)),
		CreateDate:               millis(r.CreatedAt),
		AssumeRolePolicyDocument: encodeDocument(r.AssumeRolePolicy),
		MaxSessionDuration:       new(r.MaxSessionDuration),
	}
	if r.Description != "" {
		out.Description = new(r.Description)
	}

	if tags && len(r.Tags) > 0 {
		out.Tags = tagList(r.Tags)
	}

	return out
}

func checkRoleSettings(description *string, maxSession *int64) error {
	if len(common.Value(description)) > maxDescription {
		return validation("Description must be at most %d characters long", maxDescription)
	}

	if maxSession != nil && (*maxSession < defaultSessionTime || *maxSession > maxSessionTime) {
		return validation("The requested MaxSessionDuration is invalid. It must be between %d and %d seconds.",
			defaultSessionTime, maxSessionTime)
	}

	return nil
}

func (s *Service) CreateRole(ctx context.Context, in *api.CreateRoleRequest) (*api.CreateRoleResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("roleName", in.RoleName, roleName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, rolePath)
	if err != nil {
		return nil, err
	}

	if guardErr := checkDocument("assumeRolePolicyDocument", in.AssumeRolePolicyDocument); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkRoleSettings(in.Description, in.MaxSessionDuration); guardErr != nil {
		return nil, guardErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	r := &role{
		Name:               *in.RoleName,
		ID:                 newID("AROA"),
		Path:               path,
		Tags:               tags,
		AssumeRolePolicy:   *in.AssumeRolePolicyDocument,
		Description:        common.Value(in.Description),
		MaxSessionDuration: defaultSessionTime,
		CreatedAt:          s.now().UnixMilli(),
	}
	if in.MaxSessionDuration != nil {
		r.MaxSessionDuration = *in.MaxSessionDuration
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rk := key(m.AccountID, kindRole, r.Name)
		if _, exists := tx.Get(rk); exists {
			return alreadyExists("Role with name %s already exists.", r.Name)
		}

		if boundaryErr := setBoundary(tx, m.AccountID, &r.Boundary, in.PermissionsBoundary); boundaryErr != nil {
			return boundaryErr
		}

		return storage.Write(tx, rk, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRoleResponse{Role: roleOutput(m.AccountID, r, true)}, nil
}

func (s *Service) GetRole(ctx context.Context, in *api.GetRoleRequest) (*api.GetRoleResponse, error) {
	out := &api.GetRoleResponse{}
	err := s.viewRole(ctx, in.RoleName, func(account string, r *role) error {
		out.Role = roleOutput(account, r, true)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteRole(ctx context.Context, in *api.DeleteRoleRequest) (*api.DeleteRoleOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, rk, loadErr := loadRole(tx, m.AccountID, in.RoleName)
		if loadErr != nil {
			return loadErr
		}

		if guardErr := unmodifiableRole(r); guardErr != nil {
			return guardErr
		}

		if conflict := r.deleteConflict(); conflict != nil {
			return conflict
		}

		profiles, listErr := profilesWithRole(tx, m.AccountID, r.Name)
		switch {
		case listErr != nil:
			return listErr
		case len(profiles) > 0:
			return deleteConflict("Cannot delete entity, must remove roles from instance profile first.")
		}

		if boundaryErr := setBoundary(tx, m.AccountID, &r.Boundary, nil); boundaryErr != nil {
			return boundaryErr
		}

		tx.Delete(rk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRoleOutput{}, nil
}

func (s *Service) ListRoles(ctx context.Context, in *api.ListRolesRequest) (*api.ListRolesResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in.PathPrefix != nil && !pathPrefix.MatchString(*in.PathPrefix) {
		return nil, validation("The specified value for pathPrefix is invalid.")
	}

	var matched []*role
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := list[role](tx, m.AccountID, kindRole)
		for _, r := range all {
			if strings.HasPrefix(r.Path, common.Value(in.PathPrefix)) {
				matched = append(matched, r)
			}
		}

		return listErr
	})
	if err != nil {
		return nil, err
	}

	selected, truncated, next, err := page(matched, func(r *role) string { return strings.ToLower(r.Name) },
		in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListRolesResponse{Roles: []api.Role{}, IsTruncated: truncated, Marker: next}
	for _, r := range selected {
		out.Roles = append(out.Roles, *roleOutput(m.AccountID, r, false))
	}

	return out, nil
}

func (s *Service) UpdateRole(ctx context.Context, in *api.UpdateRoleRequest) (*api.UpdateRoleResponse, error) {
	if err := checkRoleSettings(in.Description, in.MaxSessionDuration); err != nil {
		return nil, err
	}

	err := s.updateRole(ctx, in.RoleName, func(_ storage.WriteTx, _ string, r *role) error {
		if in.MaxSessionDuration != nil {
			if err := unmodifiableRole(r); err != nil {
				return err
			}
		}

		if in.Description != nil {
			r.Description = *in.Description
		}

		if in.MaxSessionDuration != nil {
			r.MaxSessionDuration = *in.MaxSessionDuration
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateRoleResponse{}, nil
}

func (s *Service) UpdateRoleDescription(
	ctx context.Context,
	in *api.UpdateRoleDescriptionRequest,
) (*api.UpdateRoleDescriptionResponse, error) {
	if guardErr := required("description", in.Description); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkRoleSettings(in.Description, nil); guardErr != nil {
		return nil, guardErr
	}

	out := &api.UpdateRoleDescriptionResponse{}
	err := s.updateRole(ctx, in.RoleName, func(_ storage.WriteTx, account string, r *role) error {
		r.Description = *in.Description
		out.Role = roleOutput(account, r, true)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateAssumeRolePolicy(
	ctx context.Context,
	in *api.UpdateAssumeRolePolicyRequest,
) (*api.UpdateAssumeRolePolicyOutput, error) {
	if err := checkDocument("policyDocument", in.PolicyDocument); err != nil {
		return nil, err
	}

	err := s.updateRole(ctx, in.RoleName, func(_ storage.WriteTx, _ string, r *role) error {
		if guardErr := unmodifiableRole(r); guardErr != nil {
			return guardErr
		}

		r.AssumeRolePolicy = *in.PolicyDocument
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAssumeRolePolicyOutput{}, nil
}

func (s *Service) TagRole(ctx context.Context, in *api.TagRoleRequest) (*api.TagRoleOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.updateRole(ctx, in.RoleName, func(_ storage.WriteTx, _ string, r *role) error {
		merged, mergeErr := tagging.Merge(r.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		r.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagRoleOutput{}, nil
}

func (s *Service) UntagRole(ctx context.Context, in *api.UntagRoleRequest) (*api.UntagRoleOutput, error) {
	err := s.updateRole(ctx, in.RoleName, func(_ storage.WriteTx, _ string, r *role) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagRoleOutput{}, nil
}

func (s *Service) ListRoleTags(ctx context.Context, in *api.ListRoleTagsRequest) (*api.ListRoleTagsResponse, error) {
	out := &api.ListRoleTagsResponse{}
	err := s.viewRole(ctx, in.RoleName, func(_ string, r *role) error {
		var err error
		out.Tags, out.IsTruncated, out.Marker, err = page(
			tagList(r.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) PutRolePolicy(ctx context.Context, in *api.PutRolePolicyRequest) (*api.PutRolePolicyOutput, error) {
	if err := s.roles().putPolicy(ctx, in.RoleName, in.PolicyName, in.PolicyDocument); err != nil {
		return nil, err
	}

	return &api.PutRolePolicyOutput{}, nil
}

func (s *Service) GetRolePolicy(ctx context.Context, in *api.GetRolePolicyRequest) (*api.GetRolePolicyResponse, error) {
	owner, name, document, err := s.roles().getPolicy(ctx, in.RoleName, in.PolicyName)
	if err != nil {
		return nil, err
	}

	return &api.GetRolePolicyResponse{RoleName: &owner, PolicyName: &name, PolicyDocument: document}, nil
}

func (s *Service) DeleteRolePolicy(
	ctx context.Context,
	in *api.DeleteRolePolicyRequest,
) (*api.DeleteRolePolicyOutput, error) {
	if err := s.roles().deletePolicy(ctx, in.RoleName, in.PolicyName); err != nil {
		return nil, err
	}

	return &api.DeleteRolePolicyOutput{}, nil
}

func (s *Service) ListRolePolicies(
	ctx context.Context,
	in *api.ListRolePoliciesRequest,
) (*api.ListRolePoliciesResponse, error) {
	names, truncated, next, err := s.roles().listPolicies(ctx, in.RoleName, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListRolePoliciesResponse{PolicyNames: names, IsTruncated: truncated, Marker: next}, nil
}

func (s *Service) AttachRolePolicy(
	ctx context.Context,
	in *api.AttachRolePolicyRequest,
) (*api.AttachRolePolicyOutput, error) {
	if err := s.roles().attachPolicy(ctx, in.RoleName, in.PolicyArn); err != nil {
		return nil, err
	}

	return &api.AttachRolePolicyOutput{}, nil
}

func (s *Service) DetachRolePolicy(
	ctx context.Context,
	in *api.DetachRolePolicyRequest,
) (*api.DetachRolePolicyOutput, error) {
	if err := s.roles().detachPolicy(ctx, in.RoleName, in.PolicyArn); err != nil {
		return nil, err
	}

	return &api.DetachRolePolicyOutput{}, nil
}

func (s *Service) ListAttachedRolePolicies(
	ctx context.Context,
	in *api.ListAttachedRolePoliciesRequest,
) (*api.ListAttachedRolePoliciesResponse, error) {
	policies, truncated, next, err := s.roles().listAttached(ctx, in.RoleName, in.PathPrefix, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListAttachedRolePoliciesResponse{AttachedPolicies: policies, IsTruncated: truncated, Marker: next}, nil
}

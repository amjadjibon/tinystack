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

const kindUser = "user"

type user struct {
	Tags map[string]string `json:",omitempty"`
	permissions
	Name      string
	ID        string
	Path      string
	Keys      []string `json:",omitempty"` // IDs of the user's access keys, oldest first
	CreatedAt int64
}

// User names follow the same rules as role names.
var userName = roleName

func userARN(account string, u *user) string { return arn(account, kindUser+u.Path+u.Name) }

// loadUser returns the user named name and its storage key.
func loadUser(tx storage.ReadTx, account string, name *string) (*user, string, error) {
	if err := checkName("userName", name, userName); err != nil {
		return nil, "", err
	}

	uk := key(account, kindUser, *name)
	u, ok, err := storage.Read[user](tx, uk)
	if err != nil {
		return nil, "", err
	}

	if !ok {
		return nil, "", noSuchEntity("The user with name %s cannot be found.", *name)
	}

	return u, uk, nil
}

// callerUser returns the name of the IAM user whose access key signed the
// request, or false when the caller is not an IAM user of its account.
func callerUser(tx storage.ReadTx, m *request.Metadata) (name string, isUser bool, err error) {
	k, ok, err := storage.Read[accessKey](tx, accessKeyPrefix+m.AccessKey)
	if err != nil || !ok || m.AccessKey == "" || k.Account != m.AccountID {
		return "", false, err
	}

	return k.UserName, true, nil
}

// updateUser runs fn on the named user and stores the result, all in one
// transaction.
func (s *Service) updateUser(
	ctx context.Context,
	name *string,
	fn func(tx storage.WriteTx, account string, u *user) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, uk, loadErr := loadUser(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, m.AccountID, u); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, uk, u)
	})
}

// userOutput describes a user. Tags are only in single-user responses, as in AWS.
func userOutput(account string, u *user, tags bool) *api.User {
	out := &api.User{
		Path:       new(u.Path),
		UserName:   new(u.Name),
		UserId:     new(u.ID),
		Arn:        new(userARN(account, u)),
		CreateDate: millis(u.CreatedAt),
	}
	if tags && len(u.Tags) > 0 {
		out.Tags = tagList(u.Tags)
	}

	return out
}

func (s *Service) CreateUser(ctx context.Context, in *api.CreateUserRequest) (*api.CreateUserResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("userName", in.UserName, userName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, rolePath)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	u := &user{Name: *in.UserName, ID: newID("AIDA"), Path: path, Tags: tags, CreatedAt: s.now().UnixMilli()}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		uk := key(m.AccountID, kindUser, u.Name)
		if _, exists := tx.Get(uk); exists {
			return alreadyExists("User with name %s already exists.", u.Name)
		}

		return storage.Write(tx, uk, u)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateUserResponse{User: userOutput(m.AccountID, u, true)}, nil
}

// GetUser describes the named user; without a name, the IAM user that signed
// the request, or the account's root user.
func (s *Service) GetUser(ctx context.Context, in *api.GetUserRequest) (*api.GetUserResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetUserResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name := in.UserName
		if name == nil {
			caller, isUser, callerErr := callerUser(tx, &m)
			if callerErr != nil {
				return callerErr
			}

			if !isUser {
				out.User = &api.User{UserId: new(m.AccountID), Arn: new(arn(m.AccountID, "root"))}
				return nil
			}

			name = &caller
		}

		u, _, loadErr := loadUser(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		out.User = userOutput(m.AccountID, u, true)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListUsers(ctx context.Context, in *api.ListUsersRequest) (*api.ListUsersResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.PathPrefix != nil && !pathPrefix.MatchString(*in.PathPrefix) {
		return nil, validation("The specified value for pathPrefix is invalid.")
	}

	var matched []*user
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := list[user](tx, m.AccountID, kindUser)
		for _, u := range all {
			if strings.HasPrefix(u.Path, common.Value(in.PathPrefix)) {
				matched = append(matched, u)
			}
		}

		return listErr
	})
	if err != nil {
		return nil, err
	}

	selected, truncated, next, err := page(matched, func(u *user) string { return strings.ToLower(u.Name) },
		in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListUsersResponse{Users: []api.User{}, IsTruncated: truncated, Marker: next}
	for _, u := range selected {
		out.Users = append(out.Users, *userOutput(m.AccountID, u, false))
	}

	return out, nil
}

// UpdateUser changes a user's path or name; renaming keeps the user's ID,
// access keys, policies, tags, and groups.
func (s *Service) UpdateUser(ctx context.Context, in *api.UpdateUserRequest) (*api.UpdateUserOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.NewUserName != nil {
		if guardErr := checkName("newUserName", in.NewUserName, userName); guardErr != nil {
			return nil, guardErr
		}
	}

	var path string
	if in.NewPath != nil {
		if path, err = checkPath(in.NewPath, rolePath); err != nil {
			return nil, err
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, uk, loadErr := loadUser(tx, m.AccountID, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		if in.NewPath != nil {
			u.Path = path
		}

		if in.NewUserName != nil {
			target := key(m.AccountID, kindUser, *in.NewUserName)
			if _, exists := tx.Get(target); exists && target != uk {
				return alreadyExists("User with name %s already exists.", *in.NewUserName)
			}

			if renameErr := renameKeys(tx, u, *in.NewUserName); renameErr != nil {
				return renameErr
			}

			if renameErr := renameMember(tx, m.AccountID, u.Name, *in.NewUserName); renameErr != nil {
				return renameErr
			}

			tx.Delete(uk)
			u.Name, uk = *in.NewUserName, target
		}

		return storage.Write(tx, uk, u)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateUserOutput{}, nil
}

// DeleteUser deletes a user that has no access keys, policies, or groups left.
func (s *Service) DeleteUser(ctx context.Context, in *api.DeleteUserRequest) (*api.DeleteUserOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, uk, loadErr := loadUser(tx, m.AccountID, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		if len(u.Keys) > 0 {
			return deleteConflict("Cannot delete entity, must delete access keys first.")
		}

		if conflict := u.deleteConflict(); conflict != nil {
			return conflict
		}

		memberOf, groupsErr := groupsOf(tx, m.AccountID, u.Name)
		if groupsErr != nil {
			return groupsErr
		}

		if len(memberOf) > 0 {
			return deleteConflict("Cannot delete entity, must remove user from all groups first.")
		}

		tx.Delete(uk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserOutput{}, nil
}

func (s *Service) TagUser(ctx context.Context, in *api.TagUserRequest) (*api.TagUserOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.updateUser(ctx, in.UserName, func(_ storage.WriteTx, _ string, u *user) error {
		merged, mergeErr := tagging.Merge(u.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		u.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagUserOutput{}, nil
}

func (s *Service) UntagUser(ctx context.Context, in *api.UntagUserRequest) (*api.UntagUserOutput, error) {
	err := s.updateUser(ctx, in.UserName, func(_ storage.WriteTx, _ string, u *user) error {
		u.Tags = tagging.Remove(u.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagUserOutput{}, nil
}

// viewUser runs fn on the named user.
func (s *Service) viewUser(ctx context.Context, name *string, fn func(account string, u *user) error) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		u, _, loadErr := loadUser(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		return fn(m.AccountID, u)
	})
}

func (s *Service) ListUserTags(ctx context.Context, in *api.ListUserTagsRequest) (*api.ListUserTagsResponse, error) {
	out := &api.ListUserTagsResponse{}
	err := s.viewUser(ctx, in.UserName, func(_ string, u *user) error {
		var err error
		out.Tags, out.IsTruncated, out.Marker, err = page(
			tagList(u.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) PutUserPolicy(ctx context.Context, in *api.PutUserPolicyRequest) (*api.PutUserPolicyOutput, error) {
	if err := s.users().putPolicy(ctx, in.UserName, in.PolicyName, in.PolicyDocument); err != nil {
		return nil, err
	}

	return &api.PutUserPolicyOutput{}, nil
}

func (s *Service) GetUserPolicy(ctx context.Context, in *api.GetUserPolicyRequest) (*api.GetUserPolicyResponse, error) {
	owner, name, document, err := s.users().getPolicy(ctx, in.UserName, in.PolicyName)
	if err != nil {
		return nil, err
	}

	return &api.GetUserPolicyResponse{UserName: &owner, PolicyName: &name, PolicyDocument: document}, nil
}

func (s *Service) DeleteUserPolicy(
	ctx context.Context,
	in *api.DeleteUserPolicyRequest,
) (*api.DeleteUserPolicyOutput, error) {
	if err := s.users().deletePolicy(ctx, in.UserName, in.PolicyName); err != nil {
		return nil, err
	}

	return &api.DeleteUserPolicyOutput{}, nil
}

func (s *Service) ListUserPolicies(
	ctx context.Context,
	in *api.ListUserPoliciesRequest,
) (*api.ListUserPoliciesResponse, error) {
	names, truncated, next, err := s.users().listPolicies(ctx, in.UserName, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListUserPoliciesResponse{PolicyNames: names, IsTruncated: truncated, Marker: next}, nil
}

func (s *Service) AttachUserPolicy(
	ctx context.Context,
	in *api.AttachUserPolicyRequest,
) (*api.AttachUserPolicyOutput, error) {
	if err := s.users().attachPolicy(ctx, in.UserName, in.PolicyArn); err != nil {
		return nil, err
	}

	return &api.AttachUserPolicyOutput{}, nil
}

func (s *Service) DetachUserPolicy(
	ctx context.Context,
	in *api.DetachUserPolicyRequest,
) (*api.DetachUserPolicyOutput, error) {
	if err := s.users().detachPolicy(ctx, in.UserName, in.PolicyArn); err != nil {
		return nil, err
	}

	return &api.DetachUserPolicyOutput{}, nil
}

func (s *Service) ListAttachedUserPolicies(
	ctx context.Context,
	in *api.ListAttachedUserPoliciesRequest,
) (*api.ListAttachedUserPoliciesResponse, error) {
	policies, truncated, next, err := s.users().listAttached(ctx, in.UserName, in.PathPrefix, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListAttachedUserPoliciesResponse{AttachedPolicies: policies, IsTruncated: truncated, Marker: next}, nil
}

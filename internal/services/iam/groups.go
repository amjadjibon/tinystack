package iam

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindGroup = "group"
	// maxGroupsPerUser is IAM's GroupsPerUser quota.
	maxGroupsPerUser = 10
)

// group is an IAM group. Users holds the names of its members; a user's
// groups are found from the groups, so membership has one record.
type group struct {
	permissions
	Name      string
	ID        string
	Path      string
	Users     []string `json:",omitempty"`
	CreatedAt int64
}

// Group names can be as long as policy names.
var groupName = policyName

func groupARN(account string, g *group) string { return arn(account, kindGroup+g.Path+g.Name) }

func (g *group) member(name string) int {
	return slices.IndexFunc(g.Users, func(u string) bool { return strings.EqualFold(u, name) })
}

// loadGroup returns the group named name and its storage key.
func loadGroup(tx storage.ReadTx, account string, name *string) (*group, string, error) {
	if err := checkName("groupName", name, groupName); err != nil {
		return nil, "", err
	}

	gk := key(account, kindGroup, *name)
	g, ok, err := storage.Read[group](tx, gk)
	if err != nil {
		return nil, "", err
	}

	if !ok {
		return nil, "", noSuchEntity("The group with name %s cannot be found.", *name)
	}

	return g, gk, nil
}

// groupsOf returns the groups the named user belongs to, ordered by name.
func groupsOf(tx storage.ReadTx, account, name string) ([]*group, error) {
	all, err := list[group](tx, account, kindGroup)
	return slices.DeleteFunc(all, func(g *group) bool { return g.member(name) < 0 }), err
}

// renameMember renames a user in the groups it belongs to.
func renameMember(tx storage.WriteTx, account, from, to string) error {
	memberOf, err := groupsOf(tx, account, from)
	if err != nil {
		return err
	}

	for _, g := range memberOf {
		g.Users[g.member(from)] = to
		if writeErr := storage.Write(tx, key(account, kindGroup, g.Name), g); writeErr != nil {
			return writeErr
		}
	}

	return nil
}

// viewGroup runs fn on the named group.
func (s *Service) viewGroup(ctx context.Context, name *string, fn func(account string, g *group) error) error {
	m, err := request.Global(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		g, _, loadErr := loadGroup(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		return fn(m.AccountID, g)
	})
}

// updateGroup runs fn on the named group and stores the result, all in one
// transaction.
func (s *Service) updateGroup(
	ctx context.Context,
	name *string,
	fn func(tx storage.WriteTx, account string, g *group) error,
) error {
	m, err := request.Global(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, gk, loadErr := loadGroup(tx, m.AccountID, name)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, m.AccountID, g); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, gk, g)
	})
}

func groupOutput(account string, g *group) api.Group {
	return api.Group{
		Path:       new(g.Path),
		GroupName:  new(g.Name),
		GroupId:    new(g.ID),
		Arn:        new(groupARN(account, g)),
		CreateDate: millis(g.CreatedAt),
	}
}

func (s *Service) CreateGroup(ctx context.Context, in *api.CreateGroupRequest) (*api.CreateGroupResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("groupName", in.GroupName, groupName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, rolePath)
	if err != nil {
		return nil, err
	}

	g := &group{Name: *in.GroupName, ID: newID("AGPA"), Path: path, CreatedAt: s.now().UnixMilli()}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		gk := key(m.AccountID, kindGroup, g.Name)
		if _, exists := tx.Get(gk); exists {
			return alreadyExists("Group with name %s already exists.", g.Name)
		}

		return storage.Write(tx, gk, g)
	})
	if err != nil {
		return nil, err
	}

	out := groupOutput(m.AccountID, g)
	return &api.CreateGroupResponse{Group: &out}, nil
}

// GetGroup describes a group and pages through its users.
func (s *Service) GetGroup(ctx context.Context, in *api.GetGroupRequest) (*api.GetGroupResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var g *group
	var members []*user
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		if g, _, loadErr = loadGroup(tx, m.AccountID, in.GroupName); loadErr != nil {
			return loadErr
		}

		for _, name := range g.Users {
			u, _, userErr := loadUser(tx, m.AccountID, &name)
			if userErr != nil {
				return userErr
			}

			members = append(members, u)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sortKey := func(u *user) string { return strings.ToLower(u.Name) }
	slices.SortFunc(members, func(a, b *user) int { return strings.Compare(sortKey(a), sortKey(b)) })
	selected, truncated, next, err := page(members, sortKey, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	group := groupOutput(m.AccountID, g)
	out := &api.GetGroupResponse{Group: &group, Users: []api.User{}, IsTruncated: truncated, Marker: next}
	for _, u := range selected {
		out.Users = append(out.Users, *userOutput(m.AccountID, u, false))
	}

	return out, nil
}

func (s *Service) ListGroups(ctx context.Context, in *api.ListGroupsRequest) (*api.ListGroupsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in.PathPrefix != nil && !pathPrefix.MatchString(*in.PathPrefix) {
		return nil, validation("The specified value for pathPrefix is invalid.")
	}

	var all []*group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var listErr error
		all, listErr = list[group](tx, m.AccountID, kindGroup)
		return listErr
	})
	if err != nil {
		return nil, err
	}

	all = slices.DeleteFunc(all, func(g *group) bool { return !strings.HasPrefix(g.Path, common.Value(in.PathPrefix)) })
	return groupPage(m.AccountID, all, in.Marker, in.MaxItems)
}

// ListGroupsForUser lists the groups a user belongs to.
func (s *Service) ListGroupsForUser(
	ctx context.Context,
	in *api.ListGroupsForUserRequest,
) (*api.ListGroupsForUserResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var memberOf []*group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		u, _, loadErr := loadUser(tx, m.AccountID, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		memberOf, loadErr = groupsOf(tx, m.AccountID, u.Name)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out, err := groupPage(m.AccountID, memberOf, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListGroupsForUserResponse{Groups: out.Groups, IsTruncated: out.IsTruncated, Marker: out.Marker}, nil
}

// groupPage pages through groups, which are ordered by key.
func groupPage(account string, groups []*group, marker *string, maxItems *int64) (*api.ListGroupsResponse, error) {
	selected, truncated, next, err := page(groups, func(g *group) string { return strings.ToLower(g.Name) },
		marker, maxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListGroupsResponse{Groups: []api.Group{}, IsTruncated: truncated, Marker: next}
	for _, g := range selected {
		out.Groups = append(out.Groups, groupOutput(account, g))
	}

	return out, nil
}

// UpdateGroup changes a group's path or name; renaming keeps its ID, users,
// and policies.
func (s *Service) UpdateGroup(ctx context.Context, in *api.UpdateGroupRequest) (*api.UpdateGroupOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in.NewGroupName != nil {
		if guardErr := checkName("newGroupName", in.NewGroupName, groupName); guardErr != nil {
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
		g, gk, loadErr := loadGroup(tx, m.AccountID, in.GroupName)
		if loadErr != nil {
			return loadErr
		}

		if in.NewPath != nil {
			g.Path = path
		}

		if in.NewGroupName != nil {
			target := key(m.AccountID, kindGroup, *in.NewGroupName)
			if _, exists := tx.Get(target); exists && target != gk {
				return alreadyExists("Group with name %s already exists.", *in.NewGroupName)
			}

			tx.Delete(gk)
			g.Name, gk = *in.NewGroupName, target
		}

		return storage.Write(tx, gk, g)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateGroupOutput{}, nil
}

// DeleteGroup deletes a group that has no users or policies left.
func (s *Service) DeleteGroup(ctx context.Context, in *api.DeleteGroupRequest) (*api.DeleteGroupOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, gk, loadErr := loadGroup(tx, m.AccountID, in.GroupName)
		if loadErr != nil {
			return loadErr
		}

		if len(g.Users) > 0 {
			return deleteConflict("Cannot delete entity, must remove users from group first.")
		}

		if conflict := g.deleteConflict(); conflict != nil {
			return conflict
		}

		tx.Delete(gk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteGroupOutput{}, nil
}

// AddUserToGroup adds a user to a group; adding a member changes nothing.
func (s *Service) AddUserToGroup(
	ctx context.Context,
	in *api.AddUserToGroupRequest,
) (*api.AddUserToGroupOutput, error) {
	err := s.updateGroup(ctx, in.GroupName, func(tx storage.WriteTx, account string, g *group) error {
		u, _, err := loadUser(tx, account, in.UserName)
		if err != nil {
			return err
		}

		if g.member(u.Name) >= 0 {
			return nil
		}

		memberOf, err := groupsOf(tx, account, u.Name)
		if err != nil {
			return err
		}

		if len(memberOf) >= maxGroupsPerUser {
			return limitExceeded("Cannot exceed quota for GroupsPerUser: %d", maxGroupsPerUser)
		}

		g.Users = append(g.Users, u.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddUserToGroupOutput{}, nil
}

func (s *Service) RemoveUserFromGroup(
	ctx context.Context,
	in *api.RemoveUserFromGroupRequest,
) (*api.RemoveUserFromGroupOutput, error) {
	err := s.updateGroup(ctx, in.GroupName, func(tx storage.WriteTx, account string, g *group) error {
		u, _, err := loadUser(tx, account, in.UserName)
		if err != nil {
			return err
		}

		i := g.member(u.Name)
		if i < 0 {
			return noSuchEntity("The user with name %s cannot be found in group %s.", u.Name, g.Name)
		}

		g.Users = slices.Delete(g.Users, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveUserFromGroupOutput{}, nil
}

func (s *Service) PutGroupPolicy(
	ctx context.Context,
	in *api.PutGroupPolicyRequest,
) (*api.PutGroupPolicyOutput, error) {
	if err := s.groups().putPolicy(ctx, in.GroupName, in.PolicyName, in.PolicyDocument); err != nil {
		return nil, err
	}

	return &api.PutGroupPolicyOutput{}, nil
}

func (s *Service) GetGroupPolicy(
	ctx context.Context,
	in *api.GetGroupPolicyRequest,
) (*api.GetGroupPolicyResponse, error) {
	owner, name, document, err := s.groups().getPolicy(ctx, in.GroupName, in.PolicyName)
	if err != nil {
		return nil, err
	}

	return &api.GetGroupPolicyResponse{GroupName: &owner, PolicyName: &name, PolicyDocument: document}, nil
}

func (s *Service) DeleteGroupPolicy(
	ctx context.Context,
	in *api.DeleteGroupPolicyRequest,
) (*api.DeleteGroupPolicyOutput, error) {
	if err := s.groups().deletePolicy(ctx, in.GroupName, in.PolicyName); err != nil {
		return nil, err
	}

	return &api.DeleteGroupPolicyOutput{}, nil
}

func (s *Service) ListGroupPolicies(
	ctx context.Context,
	in *api.ListGroupPoliciesRequest,
) (*api.ListGroupPoliciesResponse, error) {
	names, truncated, next, err := s.groups().listPolicies(ctx, in.GroupName, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListGroupPoliciesResponse{PolicyNames: names, IsTruncated: truncated, Marker: next}, nil
}

func (s *Service) AttachGroupPolicy(
	ctx context.Context,
	in *api.AttachGroupPolicyRequest,
) (*api.AttachGroupPolicyOutput, error) {
	if err := s.groups().attachPolicy(ctx, in.GroupName, in.PolicyArn); err != nil {
		return nil, err
	}

	return &api.AttachGroupPolicyOutput{}, nil
}

func (s *Service) DetachGroupPolicy(
	ctx context.Context,
	in *api.DetachGroupPolicyRequest,
) (*api.DetachGroupPolicyOutput, error) {
	if err := s.groups().detachPolicy(ctx, in.GroupName, in.PolicyArn); err != nil {
		return nil, err
	}

	return &api.DetachGroupPolicyOutput{}, nil
}

func (s *Service) ListAttachedGroupPolicies(
	ctx context.Context,
	in *api.ListAttachedGroupPoliciesRequest,
) (*api.ListAttachedGroupPoliciesResponse, error) {
	policies, truncated, next, err := s.groups().listAttached(ctx, in.GroupName, in.PathPrefix, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	return &api.ListAttachedGroupPoliciesResponse{AttachedPolicies: policies, IsTruncated: truncated, Marker: next}, nil
}

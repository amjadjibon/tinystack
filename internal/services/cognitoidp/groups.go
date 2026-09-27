package cognitoidp

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxGroups = 10000

// group is a user group record. Members are found through their user records,
// which hold their group names.
type group struct {
	Created     time.Time
	Modified    time.Time
	Precedence  *int64 `json:",omitempty"`
	Name        string
	Description string `json:",omitempty"`
	RoleArn     string `json:",omitempty"`
}

func groupKey(scope request.Scope, pool, name string) string {
	return prefix(scope, "group") + pool + "/" + name
}

func groupNotFound() error { return notFound("Group not found.") }

func (g *group) output(pool string) *api.GroupType {
	out := &api.GroupType{
		GroupName: new(g.Name), UserPoolId: new(pool), Precedence: g.Precedence,
		CreationDate: seconds(g.Created), LastModifiedDate: seconds(g.Modified),
	}
	if g.Description != "" {
		out.Description = new(g.Description)
	}

	if g.RoleArn != "" {
		out.RoleArn = new(g.RoleArn)
	}

	return out
}

func checkGroup(name, description, role *string, precedence *int64) error {
	if name == nil || utf8.RuneCountInString(*name) > 128 || !usernamePattern.MatchString(*name) {
		return invalidParameter("GroupName must be 1 to 128 characters matching [\\p{L}\\p{M}\\p{S}\\p{N}\\p{P}]+.")
	}

	if utf8.RuneCountInString(value(description)) > 2048 {
		return invalidParameter("Description must be at most 2048 characters long.")
	}

	if role != nil && !strings.HasPrefix(*role, "arn:") {
		return invalidParameter("RoleArn must be an IAM role ARN.")
	}

	if precedence != nil && *precedence < 0 {
		return invalidParameter("Precedence must be at least 0.")
	}

	return nil
}

func loadGroup(tx storage.ReadTx, scope request.Scope, p *pool, name *string) (*group, error) {
	if name == nil || *name == "" {
		return nil, invalidParameter("GroupName is required.")
	}

	g, ok, err := read[group](tx, groupKey(scope, p.ID, *name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, groupNotFound()
	}

	return g, nil
}

func (s *Service) CreateGroup(ctx context.Context, in *api.CreateGroupRequest) (*api.CreateGroupResponse, error) {
	if err := checkGroup(in.GroupName, in.Description, in.RoleArn, in.Precedence); err != nil {
		return nil, err
	}

	var out *api.GroupType
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		k := groupKey(scope, p.ID, *in.GroupName)
		if _, exists := tx.Get(k); exists {
			return awserror.New("GroupExistsException", "A group with the name "+*in.GroupName+" already exists.")
		}

		if len(tx.Keys(prefix(scope, "group")+p.ID+"/")) >= maxGroups {
			return awserror.New("LimitExceededException", "The user pool has reached its limit of groups.")
		}

		now := s.now().UTC()
		g := &group{
			Name: *in.GroupName, Description: value(in.Description), RoleArn: value(in.RoleArn),
			Precedence: in.Precedence, Created: now, Modified: now,
		}
		out = g.output(p.ID)
		return write(tx, k, g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateGroupResponse{Group: out}, nil
}

func (s *Service) GetGroup(ctx context.Context, in *api.GetGroupRequest) (*api.GetGroupResponse, error) {
	var out *api.GroupType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		g, err := loadGroup(tx, scope, p, in.GroupName)
		if err != nil {
			return err
		}

		out = g.output(p.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetGroupResponse{Group: out}, nil
}

func (s *Service) ListGroups(ctx context.Context, in *api.ListGroupsRequest) (*api.ListGroupsResponse, error) {
	n, err := limit(in.Limit, "Limit")
	if err != nil {
		return nil, err
	}

	var groups []*api.GroupType
	err = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		for _, name := range ids(tx, prefix(scope, "group")+p.ID+"/") {
			g, _, readErr := read[group](tx, groupKey(scope, p.ID, name))
			if readErr != nil {
				return readErr
			}

			groups = append(groups, g.output(p.ID))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return groupPage(groups, in.NextToken, n, func(groups []api.GroupType, next *string) *api.ListGroupsResponse {
		return &api.ListGroupsResponse{Groups: groups, NextToken: next}
	})
}

// groupPage returns a page of groups as the response respond builds.
func groupPage[T any](
	groups []*api.GroupType,
	token *string,
	n int,
	respond func([]api.GroupType, *string) *T,
) (*T, error) {
	selected, next, err := page(groups, func(g *api.GroupType) string { return *g.GroupName }, token, n)
	if err != nil {
		return nil, err
	}

	out := make([]api.GroupType, 0, len(selected))
	for _, g := range selected {
		out = append(out, *g)
	}

	return respond(out, next), nil
}

func (s *Service) UpdateGroup(ctx context.Context, in *api.UpdateGroupRequest) (*api.UpdateGroupResponse, error) {
	if err := checkGroup(in.GroupName, in.Description, in.RoleArn, in.Precedence); err != nil {
		return nil, err
	}

	var out *api.GroupType
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		g, err := loadGroup(tx, scope, p, in.GroupName)
		if err != nil {
			return err
		}

		// Only the settings given change.
		if in.Description != nil {
			g.Description = *in.Description
		}

		if in.RoleArn != nil {
			g.RoleArn = *in.RoleArn
		}

		if in.Precedence != nil {
			g.Precedence = in.Precedence
		}

		g.Modified = s.now().UTC()
		out = g.output(p.ID)
		return write(tx, groupKey(scope, p.ID, g.Name), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateGroupResponse{Group: out}, nil
}

// DeleteGroup deletes a group and removes its members from it.
func (s *Service) DeleteGroup(ctx context.Context, in *api.DeleteGroupRequest) (*api.DeleteGroupOutput, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		g, err := loadGroup(tx, scope, p, in.GroupName)
		if err != nil {
			return err
		}

		members, err := users(tx, scope, p, func(u *user) bool { return slices.Contains(u.Groups, g.Name) })
		if err != nil {
			return err
		}

		for _, u := range members {
			u.Groups = slices.DeleteFunc(u.Groups, func(name string) bool { return name == g.Name })
			if writeErr := write(tx, userKey(scope, p, u.Username), u); writeErr != nil {
				return writeErr
			}
		}

		tx.Delete(groupKey(scope, p.ID, g.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteGroupOutput{}, nil
}

// membership adds the user to the group, or removes it.
func (s *Service) membership(ctx context.Context, poolID, username, name *string, add bool) error {
	return s.changeUser(ctx, poolID, username,
		func(tx storage.WriteTx, scope request.Scope, p *pool, u *user, _ *outbox) error {
			g, err := loadGroup(tx, scope, p, name)
			if err != nil {
				return err
			}

			u.Groups = slices.DeleteFunc(u.Groups, func(n string) bool { return n == g.Name })
			if add {
				u.Groups = append(u.Groups, g.Name)
				slices.Sort(u.Groups)
			}

			return nil
		})
}

func (s *Service) AdminAddUserToGroup(
	ctx context.Context,
	in *api.AdminAddUserToGroupRequest,
) (*api.AdminAddUserToGroupOutput, error) {
	if err := s.membership(ctx, in.UserPoolId, in.Username, in.GroupName, true); err != nil {
		return nil, err
	}

	return &api.AdminAddUserToGroupOutput{}, nil
}

func (s *Service) AdminRemoveUserFromGroup(
	ctx context.Context,
	in *api.AdminRemoveUserFromGroupRequest,
) (*api.AdminRemoveUserFromGroupOutput, error) {
	if err := s.membership(ctx, in.UserPoolId, in.Username, in.GroupName, false); err != nil {
		return nil, err
	}

	return &api.AdminRemoveUserFromGroupOutput{}, nil
}

func (s *Service) AdminListGroupsForUser(
	ctx context.Context,
	in *api.AdminListGroupsForUserRequest,
) (*api.AdminListGroupsForUserResponse, error) {
	n, err := limit(in.Limit, "Limit")
	if err != nil {
		return nil, err
	}

	var groups []*api.GroupType
	err = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		u, findErr := findUser(tx, scope, p, in.Username)
		if findErr != nil {
			return findErr
		}

		for _, name := range u.Groups {
			g, ok, readErr := read[group](tx, groupKey(scope, p.ID, name))
			if readErr != nil {
				return readErr
			}

			if ok {
				groups = append(groups, g.output(p.ID))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return groupPage(
		groups,
		in.NextToken,
		n,
		func(groups []api.GroupType, next *string) *api.AdminListGroupsForUserResponse {
			return &api.AdminListGroupsForUserResponse{Groups: groups, NextToken: next}
		},
	)
}

func (s *Service) ListUsersInGroup(
	ctx context.Context,
	in *api.ListUsersInGroupRequest,
) (*api.ListUsersInGroupResponse, error) {
	n, err := limit(in.Limit, "Limit")
	if err != nil {
		return nil, err
	}

	var members []*user
	err = s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		g, loadErr := loadGroup(tx, scope, p, in.GroupName)
		if loadErr != nil {
			return loadErr
		}

		var listErr error
		members, listErr = users(tx, scope, p, func(u *user) bool { return slices.Contains(u.Groups, g.Name) })
		return listErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(members, func(u *user) string { return u.Username }, in.NextToken, n)
	if err != nil {
		return nil, err
	}

	out := &api.ListUsersInGroupResponse{Users: []api.UserType{}, NextToken: next}
	for _, u := range selected {
		out.Users = append(out.Users, u.output(nil))
	}

	return out, nil
}

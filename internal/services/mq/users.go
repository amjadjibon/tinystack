package mq

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	userCreate = "CREATE"
	userUpdate = "UPDATE"
	userDelete = "DELETE"
)

func validateUser(name string, password *string, groups []string) error {
	if !usernamePattern.MatchString(name) {
		return invalid("Username requires 2-100 letters, digits, hyphens, periods, underscores or tildes")
	}

	if password != nil {
		pw := *password
		unique := map[rune]bool{}
		for _, c := range pw {
			if c < 32 || c > 126 {
				return invalid("Password requires printable ASCII")
			}

			unique[c] = true
		}

		if len(pw) < 12 || len(pw) > 250 || len(unique) < 4 || strings.ContainsAny(pw, ",:=") {
			return invalid("Password requires 12-250 characters, four unique characters and no comma, colon or equals")
		}
	}

	if len(groups) > 20 {
		return invalid("At most 20 groups are allowed")
	}

	seen := map[string]bool{}
	for _, g := range groups {
		if !usernamePattern.MatchString(g) || seen[g] {
			return invalid("Invalid or duplicate group")
		}

		seen[g] = true
	}

	return nil
}

func (s *Service) userBroker(
	tx storage.ReadTx,
	scope request.Scope,
	id, username string,
	mutation bool,
) (*record, error) {
	r, err := read(tx, scope, id)
	if err != nil {
		return nil, err
	}

	if *r.Broker.EngineType != activeMQ {
		return nil, invalid("RabbitMQ users must be managed through the RabbitMQ management API")
	}

	if mutation && s.runtime != nil {
		return nil, invalid("User mutations are unsupported in Docker mode; recreate the broker with initial users")
	}

	if username != "" && !usernamePattern.MatchString(username) {
		return nil, invalid("Invalid username")
	}

	return r, nil
}

func (s *Service) CreateUser(ctx context.Context, in *api.CreateUserRequest) (*api.CreateUserResponse, error) {
	if in.Password == nil {
		return nil, invalid("Password is required")
	}

	if err := validateUser(common.Value(in.Username), in.Password, in.Groups); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := s.userBroker(tx, scope, common.Value(in.BrokerId), common.Value(in.Username), true)
		if e != nil {
			return e
		}

		for _, u := range r.UserDetails {
			if common.Value(u.Username) == common.Value(in.Username) {
				return conflict("User already exists")
			}
		}

		if len(r.UserDetails) >= 250 {
			return invalid("Maximum 250 users")
		}

		r.UserDetails = append(
			r.UserDetails,
			api.DescribeUserResponse{
				BrokerId: in.BrokerId,
				Username: in.Username,
				Pending: &api.UserPendingChanges{
					PendingChange: new(userCreate),
					ConsoleAccess: new(common.Value(in.ConsoleAccess)),
					Groups:        in.Groups,
				},
			},
		)
		syncUsers(r)
		return save(tx, scope, r)
	})
	return &api.CreateUserResponse{}, err
}

func syncUsers(r *record) {
	r.Broker.Users = nil
	for _, u := range r.UserDetails {
		var change *string
		if u.Pending != nil {
			change = u.Pending.PendingChange
		}

		r.Broker.Users = append(r.Broker.Users, api.UserSummary{Username: u.Username, PendingChange: change})
	}
}

func (s *Service) DescribeUser(ctx context.Context, in *api.DescribeUserRequest) (*api.DescribeUserResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeUserResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := s.userBroker(tx, scope, common.Value(in.BrokerId), common.Value(in.Username), false)
		if e != nil {
			return e
		}

		for _, u := range r.UserDetails {
			if common.Value(u.Username) == common.Value(in.Username) {
				out = common.Clone(&u)
				return nil
			}
		}

		return missing()
	})
	return out, err
}

func (s *Service) UpdateUser(ctx context.Context, in *api.UpdateUserRequest) (*api.UpdateUserResponse, error) {
	if err := validateUser(common.Value(in.Username), in.Password, in.Groups); err != nil {
		return nil, err
	}

	err := s.changeUser(
		ctx,
		common.Value(in.BrokerId),
		common.Value(in.Username),
		func(u *api.DescribeUserResponse) error {
			if u.Pending != nil && common.Value(u.Pending.PendingChange) == userDelete {
				return conflict("User is pending deletion")
			}

			if u.Pending == nil {
				u.Pending = &api.UserPendingChanges{
					PendingChange: new(userUpdate),
					ConsoleAccess: u.ConsoleAccess,
					Groups:        u.Groups,
				}
			}

			if in.ConsoleAccess != nil {
				u.Pending.ConsoleAccess = in.ConsoleAccess
			}

			if in.Groups != nil {
				u.Pending.Groups = in.Groups
			}

			return nil
		},
	)
	return &api.UpdateUserResponse{}, err
}

func (s *Service) changeUser(ctx context.Context, id, name string, change func(*api.DescribeUserResponse) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := s.userBroker(tx, scope, id, name, true)
		if e != nil {
			return e
		}

		for i := range r.UserDetails {
			if common.Value(r.UserDetails[i].Username) == name {
				if e := change(&r.UserDetails[i]); e != nil {
					return e
				}

				syncUsers(r)
				return save(tx, scope, r)
			}
		}

		return missing()
	})
}

func (s *Service) DeleteUser(ctx context.Context, in *api.DeleteUserRequest) (*api.DeleteUserResponse, error) {
	err := s.changeUser(
		ctx,
		common.Value(in.BrokerId),
		common.Value(in.Username),
		func(u *api.DescribeUserResponse) error {
			u.Pending = &api.UserPendingChanges{PendingChange: new(userDelete)}
			return nil
		},
	)
	return &api.DeleteUserResponse{}, err
}

func (s *Service) ListUsers(ctx context.Context, in *api.ListUsersRequest) (*api.ListUsersResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListUsersResponse{BrokerId: in.BrokerId, MaxResults: common.OrElse(in.MaxResults, new(int64(20)))}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := s.userBroker(tx, scope, common.Value(in.BrokerId), "", false)
		if e != nil {
			return e
		}

		rows := slices.Clone(r.Broker.Users)
		slices.SortFunc(rows, func(a, b api.UserSummary) int { return strings.Compare(*a.Username, *b.Username) })
		out.Users, out.NextToken, e = mqPage(
			rows,
			func(u api.UserSummary) string { return *u.Username },
			in.MaxResults,
			in.NextToken,
			scope.Key(serviceName, "users", *in.BrokerId),
		)
		return e
	})
	return out, err
}

func applyPending(r *record) {
	users := r.UserDetails[:0]
	for _, u := range r.UserDetails {
		if u.Pending != nil {
			if common.Value(u.Pending.PendingChange) == userDelete {
				continue
			}

			u.ConsoleAccess = u.Pending.ConsoleAccess
			u.Groups = u.Pending.Groups
			u.Pending = nil
		}

		users = append(users, u)
	}

	r.UserDetails = users
	syncUsers(r)
	c := r.Broker.Configurations
	if c != nil && c.Pending != nil {
		if c.Current != nil {
			c.History = append(c.History, *c.Current)
		}

		c.Current = c.Pending
		c.Pending = nil
	}
}

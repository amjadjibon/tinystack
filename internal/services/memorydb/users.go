package memorydb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	authPassword   = "password"
	authIAM        = "iam"
	authNoPassword = "no-password"

	// Documented in the ACL guide: a user has up to two passwords of 16 to 128 printable characters.
	maxPasswords      = 2
	minPasswordLength = 16
	maxPasswordLength = 128
)

type userRecord struct {
	tagSet
	Name           string
	AccessString   string
	AuthType       string
	PasswordHashes []string
	PasswordCount  int
}

// defaultUser is the built-in user every account has. It is not stored, so it is identical on every read.
func defaultUser() *userRecord {
	return &userRecord{Name: defaultUserName, AccessString: defaultAccessString, AuthType: authNoPassword}
}

func userNotFound(name string) error {
	return fault("UserNotFoundFault", "User %s not found.", name)
}

// aclNames lists the ACLs a user belongs to, in name order.
func aclNames(tx storage.ReadTx, scope request.Scope, user string) ([]string, error) {
	acls, err := listACLs(tx, scope)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, a := range acls {
		if slices.Contains(a.UserNames, user) {
			names = append(names, a.Name)
		}
	}

	return names, nil
}

func (r *userRecord) output(scope request.Scope, acls []string) *api.User {
	return &api.User{
		Name:                 new(r.Name),
		Status:               new(statusActive),
		AccessString:         new(r.AccessString),
		ACLNames:             acls,
		MinimumEngineVersion: new(minimumEngineVersion),
		Authentication:       &api.Authentication{Type: new(r.AuthType), PasswordCount: new(int64(r.PasswordCount))},
		ARN:                  new(arn(scope, kindUser, r.Name)),
	}
}

// authentication reads AuthenticationMode into a type and password count. The passwords themselves are checked
// and then dropped: they are never stored.
func authentication(mode *api.AuthenticationMode) (authType string, passwords int, err error) {
	if mode == nil {
		return "", 0, invalidValue("AuthenticationMode is required.")
	}

	authType = authPassword
	if mode.Type != nil {
		authType = *mode.Type
	}

	switch authType {
	case authIAM:
		if len(mode.Passwords) > 0 {
			return "", 0, invalidCombination("Passwords cannot be set for an IAM user.")
		}
	case authPassword:
		if len(mode.Passwords) == 0 || len(mode.Passwords) > maxPasswords {
			return "", 0, invalidValue("A password user needs one or two passwords.")
		}

		for _, p := range mode.Passwords {
			if len(p) < minPasswordLength || len(p) > maxPasswordLength ||
				strings.ContainsAny(
					p,
					`,"/@`,
				) || strings.ContainsFunc(p, func(c rune) bool { return c < ' ' || c > '~' }) {
				return "", 0, invalidValue("Passwords must be 16 to 128 printable characters without , \" / or @.")
			}
		}

		passwords = len(mode.Passwords)
	default:
		return "", 0, invalidValue("AuthenticationMode Type must be password or iam.")
	}

	return authType, passwords, nil
}

// checkAccessString applies the one rule of the ACL guide that does not need the engine's ACL parser.
func checkAccessString(access string) error {
	if strings.TrimSpace(access) == "" {
		return invalidValue("AccessString is required.")
	}

	if slices.ContainsFunc(strings.Fields(access), func(rule string) bool { return strings.EqualFold(rule, "reset") }) {
		return invalidValue("AccessString cannot contain the reset command.")
	}

	return nil
}

func (s *Service) CreateUser(ctx context.Context, in *api.CreateUserRequest) (*api.CreateUserResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.UserName)
	err = checkName("UserName", name)
	if err != nil {
		return nil, err
	}

	authType, passwords, err := authentication(in.AuthenticationMode)
	if err != nil {
		return nil, err
	}

	access := common.Value(in.AccessString)
	err = checkAccessString(access)
	if err != nil {
		return nil, err
	}

	if accessErr := s.checkRuntimeAccess(ctx, scope, access); accessErr != nil {
		return nil, accessErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if s.runtime != nil && authType == authIAM {
		return nil, invalidValue("IAM authentication is not supported in Docker mode.")
	}

	r := &userRecord{
		Name:          name,
		AccessString:  access,
		AuthType:      authType,
		PasswordCount: passwords,
		tagSet:        tagSet{Tags: tags},
	}

	if s.runtime != nil {
		r.PasswordHashes = passwordHashes(in.AuthenticationMode.Passwords)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key(scope, kindUser, name)); exists || name == defaultUserName {
			return fault("UserAlreadyExistsFault", "User %s already exists.", name)
		}

		return storage.Write(tx, key(scope, kindUser, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateUserResponse{User: r.output(scope, nil)}, nil
}

func (s *Service) UpdateUser(ctx context.Context, in *api.UpdateUserRequest) (*api.UpdateUserResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.UserName)
	if in.AuthenticationMode == nil && in.AccessString == nil {
		return nil, invalidCombination("AuthenticationMode or AccessString is required.")
	}

	if in.AccessString != nil {
		if accessErr := s.checkRuntimeAccess(ctx, scope, *in.AccessString); accessErr != nil {
			return nil, accessErr
		}
	}

	var out *api.User
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if name == defaultUserName {
			return fault("InvalidUserStateFault", "The default user cannot be modified.")
		}

		r, found, e := storage.Read[userRecord](tx, key(scope, kindUser, name))
		if e != nil {
			return e
		}

		if !found {
			return userNotFound(name)
		}

		if in.AuthenticationMode != nil {
			if r.AuthType, r.PasswordCount, e = authentication(in.AuthenticationMode); e != nil {
				return e
			}
		}

		if s.runtime != nil && r.AuthType == authIAM {
			return invalidValue("IAM authentication is not supported in Docker mode.")
		}

		if s.runtime != nil && in.AuthenticationMode != nil {
			r.PasswordHashes = passwordHashes(in.AuthenticationMode.Passwords)
		}

		if in.AccessString != nil {
			e = checkAccessString(*in.AccessString)
			if e != nil {
				return e
			}

			r.AccessString = *in.AccessString
		}

		acls, e := aclNames(tx, scope, name)
		if e != nil {
			return e
		}

		out = r.output(scope, acls)
		return storage.Write(tx, key(scope, kindUser, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateUserResponse{User: out}, nil
}

// DeleteUser removes the user from every ACL it belongs to, as the API documents.
func (s *Service) DeleteUser(ctx context.Context, in *api.DeleteUserRequest) (*api.DeleteUserResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.UserName)
	var out *api.User
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if name == defaultUserName {
			return fault("InvalidUserStateFault", "The default user cannot be deleted.")
		}

		r, found, e := storage.Read[userRecord](tx, key(scope, kindUser, name))
		if e != nil {
			return e
		}

		if !found {
			return userNotFound(name)
		}

		acls, e := listACLs(tx, scope)
		if e != nil {
			return e
		}

		var memberOf []string
		for _, a := range acls {
			if !slices.Contains(a.UserNames, name) {
				continue
			}

			memberOf = append(memberOf, a.Name)
			a.UserNames = slices.DeleteFunc(a.UserNames, func(u string) bool { return u == name })
			e = storage.Write(tx, key(scope, kindACL, a.Name), a)
			if e != nil {
				return e
			}
		}

		out = r.output(scope, memberOf)
		out.Status = new(statusDeleting)
		tx.Delete(key(scope, kindUser, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserResponse{User: out}, nil
}

// filterNameUserName is the one DescribeUsers filter the model documents: "The property being filtered. For
// example, UserName."
const filterNameUserName = "UserName"

func (s *Service) DescribeUsers(ctx context.Context, in *api.DescribeUsersRequest) (*api.DescribeUsersResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if in.UserName != nil && len(in.Filters) > 0 {
		return nil, invalidCombination("UserName and Filters cannot be combined.")
	}

	var wanted []string
	for _, f := range in.Filters {
		if common.Value(f.Name) != filterNameUserName || len(f.Values) == 0 {
			return nil, invalidValue("Only a %s filter with at least one value is supported.", filterNameUserName)
		}

		for _, v := range f.Values {
			wanted = append(wanted, strings.ToLower(v))
		}
	}

	var items []*userRecord
	memberships := map[string][]string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		if items, e = storage.Records[userRecord](tx, scope.Prefix(serviceName, kindUser)); e != nil {
			return e
		}

		// The built-in user belongs between the stored ones in name order.
		at, _ := slices.BinarySearchFunc(items, defaultUserName, func(r *userRecord, n string) int {
			return strings.Compare(r.Name, n)
		})
		items = slices.Insert(items, at, defaultUser())

		if in.UserName != nil {
			name := normalize(in.UserName)
			if items = slices.DeleteFunc(items, func(r *userRecord) bool { return r.Name != name }); len(items) == 0 {
				return userNotFound(name)
			}
		} else if wanted != nil {
			items = slices.DeleteFunc(items, func(r *userRecord) bool { return !slices.Contains(wanted, r.Name) })
		}

		for _, r := range items {
			if memberships[r.Name], e = aclNames(tx, scope, r.Name); e != nil {
				return e
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(r *userRecord) string { return r.Name },
		in.NextToken,
		limit,
		invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeUsersResponse{NextToken: next, Users: []api.User{}}
	for _, r := range page {
		out.Users = append(out.Users, *r.output(scope, memberships[r.Name]))
	}

	return out, nil
}

func passwordHashes(passwords []string) []string {
	hashes := make([]string, len(passwords))
	for i, password := range passwords {
		hashes[i] = fmt.Sprintf("%x", sha256.Sum256([]byte(password)))
	}

	return hashes
}

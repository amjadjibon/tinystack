package identitystore

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func attributeValue(raw *any, out any) error {
	if raw == nil {
		return nil
	}

	data, err := json.Marshal(*raw)
	if err != nil {
		return awserror.Validation("Invalid attribute value: %v", err)
	}

	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return awserror.Validation("Invalid or unsupported attribute value: %v", err)
	}

	return nil
}

func checkOperations(ops []api.AttributeOperation) error {
	if len(ops) < 1 || len(ops) > 100 {
		return awserror.Validation("Operations must contain 1 to 100 items")
	}

	return nil
}

func (s *Service) CreateUser(ctx context.Context, in *api.CreateUserRequest) (*api.CreateUserResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	if in.Emails != nil && len(in.Emails) != 1 {
		return nil, awserror.Validation("Emails must contain one primary email")
	}

	in = common.Clone(in)
	r := &api.User{
		IdentityStoreId: in.IdentityStoreId,
		UserId:          new(newID()),
		UserName:        in.UserName,
		DisplayName:     in.DisplayName,
		Name:            in.Name,
		Emails:          in.Emails,
	}

	if validationErr := checkUser(r); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		id, name := userIdentity(r)
		if e := checkUnique(tx, scope, "user", id, name, userIdentity); e != nil {
			return e
		}

		return storage.Write(tx, scope.Key(serviceName, "user", id), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateUserResponse{IdentityStoreId: r.IdentityStoreId, UserId: r.UserId}, nil
}

func (s *Service) DescribeUser(ctx context.Context, in *api.DescribeUserRequest) (*api.DescribeUserResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeUserResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := load[api.User](tx, scope, "user", in.UserId)
		if e != nil {
			return e
		}

		out = &api.DescribeUserResponse{
			IdentityStoreId: r.IdentityStoreId,
			UserId:          r.UserId,
			UserName:        r.UserName,
			DisplayName:     r.DisplayName,
			Name:            r.Name,
			Emails:          r.Emails,
		}

		return nil
	})
	return out, err
}

func (s *Service) ListUsers(ctx context.Context, in *api.ListUsersRequest) (*api.ListUsersResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	filter, err := filterValue(in.Filters, "UserName")
	if err != nil {
		return nil, err
	}

	list := []api.User{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.User](tx, scope.Prefix(serviceName, "user"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if filter == "" || strings.EqualFold(*r.UserName, filter) {
				list = append(list, *r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(list, func(r api.User) string { return *r.UserId }, in.NextToken, limit,
		awserror.Validation("Invalid NextToken"))
	return &api.ListUsersResponse{Users: page, NextToken: next}, err
}

func (s *Service) UpdateUser(ctx context.Context, in *api.UpdateUserRequest) (*api.UpdateUserResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	if validationErr := checkOperations(in.Operations); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := load[api.User](tx, scope, "user", in.UserId)
		if e != nil {
			return e
		}

		for _, op := range in.Operations {
			if e := updateUser(r, op); e != nil {
				return e
			}
		}

		if e := checkUser(r); e != nil {
			return e
		}

		id, name := userIdentity(r)
		if e := checkUnique(tx, scope, "user", id, name, userIdentity); e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateUserResponse{}, nil
}

func updateUser(r *api.User, op api.AttributeOperation) error {
	switch strings.ToLower(common.Value(op.AttributePath)) {
	case "username":
		r.UserName = nil
		return attributeValue(op.AttributeValue, &r.UserName)
	case "displayname":
		r.DisplayName = nil
		return attributeValue(op.AttributeValue, &r.DisplayName)
	case "name":
		r.Name = nil
		return attributeValue(op.AttributeValue, &r.Name)
	case "emails":
		r.Emails = nil
		return attributeValue(op.AttributeValue, &r.Emails)
	case "name.givenname":
		if r.Name == nil {
			r.Name = &api.Name{}
		}

		r.Name.GivenName = nil
		return attributeValue(op.AttributeValue, &r.Name.GivenName)
	case "name.familyname":
		if r.Name == nil {
			r.Name = &api.Name{}
		}

		r.Name.FamilyName = nil
		return attributeValue(op.AttributeValue, &r.Name.FamilyName)
	default:
		return awserror.Validation("Unsupported AttributePath")
	}
}

func (s *Service) DeleteUser(ctx context.Context, in *api.DeleteUserRequest) (*api.DeleteUserResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := load[api.User](tx, scope, "user", in.UserId)
		if e != nil {
			return e
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, "membership")) {
			membership, _, readErr := storage.Read[api.GroupMembership](tx, k)
			if readErr != nil {
				return readErr
			}

			if *membership.MemberId.UserId == *r.UserId {
				tx.Delete(k)
			}
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserResponse{}, nil
}

func (s *Service) GetUserId(ctx context.Context, in *api.GetUserIdRequest) (*api.GetUserIdResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	value, err := uniqueValue(in.AlternateIdentifier, "userName")
	if err != nil {
		return nil, err
	}

	var out *api.GetUserIdResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.User](tx, scope.Prefix(serviceName, "user"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if strings.EqualFold(*r.UserName, value) {
				out = &api.GetUserIdResponse{IdentityStoreId: r.IdentityStoreId, UserId: r.UserId}
				return nil
			}
		}

		return missing()
	})
	return out, err
}

func (s *Service) CreateGroup(ctx context.Context, in *api.CreateGroupRequest) (*api.CreateGroupResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	in = common.Clone(in)
	r := &api.Group{
		IdentityStoreId: in.IdentityStoreId,
		GroupId:         new(newID()),
		DisplayName:     in.DisplayName,
		Description:     in.Description,
	}

	if validationErr := checkGroup(r); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		id, name := groupIdentity(r)
		if e := checkUnique(tx, scope, "group", id, name, groupIdentity); e != nil {
			return e
		}

		return storage.Write(tx, scope.Key(serviceName, "group", id), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateGroupResponse{IdentityStoreId: r.IdentityStoreId, GroupId: r.GroupId}, nil
}

func (s *Service) DescribeGroup(ctx context.Context, in *api.DescribeGroupRequest) (*api.DescribeGroupResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeGroupResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := load[api.Group](tx, scope, "group", in.GroupId)
		if e != nil {
			return e
		}

		out = &api.DescribeGroupResponse{
			IdentityStoreId: r.IdentityStoreId,
			GroupId:         r.GroupId,
			DisplayName:     r.DisplayName,
			Description:     r.Description,
		}

		return nil
	})
	return out, err
}

func (s *Service) ListGroups(ctx context.Context, in *api.ListGroupsRequest) (*api.ListGroupsResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	filter, err := filterValue(in.Filters, "DisplayName")
	if err != nil {
		return nil, err
	}

	list := []api.Group{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.Group](tx, scope.Prefix(serviceName, "group"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if filter == "" || strings.EqualFold(*r.DisplayName, filter) {
				list = append(list, *r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(list, func(r api.Group) string { return *r.GroupId }, in.NextToken, limit,
		awserror.Validation("Invalid NextToken"))
	return &api.ListGroupsResponse{Groups: page, NextToken: next}, err
}

func (s *Service) UpdateGroup(ctx context.Context, in *api.UpdateGroupRequest) (*api.UpdateGroupResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	if validationErr := checkOperations(in.Operations); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := load[api.Group](tx, scope, "group", in.GroupId)
		if e != nil {
			return e
		}

		for _, op := range in.Operations {
			if e := updateGroup(r, op); e != nil {
				return e
			}
		}

		if e := checkGroup(r); e != nil {
			return e
		}

		id, name := groupIdentity(r)
		if e := checkUnique(tx, scope, "group", id, name, groupIdentity); e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateGroupResponse{}, nil
}

func updateGroup(r *api.Group, op api.AttributeOperation) error {
	switch strings.ToLower(common.Value(op.AttributePath)) {
	case "displayname":
		r.DisplayName = nil
		return attributeValue(op.AttributeValue, &r.DisplayName)
	case "description":
		r.Description = nil
		return attributeValue(op.AttributeValue, &r.Description)
	default:
		return awserror.Validation("Unsupported AttributePath")
	}
}

func (s *Service) DeleteGroup(ctx context.Context, in *api.DeleteGroupRequest) (*api.DeleteGroupResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := load[api.Group](tx, scope, "group", in.GroupId)
		if e != nil {
			return e
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, "membership")) {
			membership, _, readErr := storage.Read[api.GroupMembership](tx, k)
			if readErr != nil {
				return readErr
			}

			if *membership.GroupId == *r.GroupId {
				tx.Delete(k)
			}
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteGroupResponse{}, nil
}

func (s *Service) GetGroupId(ctx context.Context, in *api.GetGroupIdRequest) (*api.GetGroupIdResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	value, err := uniqueValue(in.AlternateIdentifier, "displayName")
	if err != nil {
		return nil, err
	}

	var out *api.GetGroupIdResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.Group](tx, scope.Prefix(serviceName, "group"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if strings.EqualFold(*r.DisplayName, value) {
				out = &api.GetGroupIdResponse{IdentityStoreId: r.IdentityStoreId, GroupId: r.GroupId}
				return nil
			}
		}

		return missing()
	})
	return out, err
}

func uniqueValue(id *api.AlternateIdentifier, path string) (string, error) {
	if id == nil || id.UniqueAttribute == nil ||
		!strings.EqualFold(common.Value(id.UniqueAttribute.AttributePath), path) {
		return "", awserror.Validation("Specify the supported UniqueAttribute")
	}

	var value *string
	if err := attributeValue(id.UniqueAttribute.AttributeValue, &value); err != nil {
		return "", err
	}

	if err := checkText(value, true, 1024, textPattern); err != nil {
		return "", err
	}

	return *value, nil
}

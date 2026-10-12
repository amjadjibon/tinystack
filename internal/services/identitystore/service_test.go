package identitystore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func identityContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func raw(value string) *any {
	var out any
	_ = json.Unmarshal([]byte(value), &out)
	return &out
}

func TestUserAndGroupRecords(t *testing.T) {
	s := identitystore.New(memory.New(nil, nil))
	ctx := identityContext("123456789012", "us-east-1")
	arn, storeID := identitystore.Instance("123456789012")
	again, idAgain := identitystore.Instance("123456789012")
	if arn != again || storeID != idAgain {
		t.Fatal("instance identity changed")
	}

	if _, err := s.CreateUser(
		ctx,
		&api.CreateUserRequest{IdentityStoreId: new(storeID), UserName: new("empty-email"), Emails: []api.Email{}},
	); err == nil {
		t.Fatal("explicit empty Emails must fail before cloning")
	}

	user, err := s.CreateUser(ctx, &api.CreateUserRequest{
		IdentityStoreId: new(storeID), UserName: new("alice"),
		Name:   &api.Name{GivenName: new("Alice"), FamilyName: new("Smith")},
		Emails: []api.Email{{Value: new("alice@example.com"), Primary: new(true)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateUser(ctx, &api.CreateUserRequest{IdentityStoreId: new(storeID), UserName: new("ALICE")})
	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "ConflictException" {
		t.Fatal(err)
	}

	if _, err = s.UpdateUser(ctx, &api.UpdateUserRequest{
		IdentityStoreId: new(storeID), UserId: user.UserId,
		Operations: []api.AttributeOperation{
			{AttributePath: new("displayName"), AttributeValue: raw(`"changed"`)},
			{AttributePath: new("userName"), AttributeValue: raw(`"invalid username"`)},
		},
	}); err == nil {
		t.Fatal("invalid whole-candidate update succeeded")
	}

	described, err := s.DescribeUser(ctx, &api.DescribeUserRequest{IdentityStoreId: new(storeID), UserId: user.UserId})
	if err != nil || described.DisplayName != nil || *described.UserName != "alice" {
		t.Fatal(described, err)
	}

	if _, err = s.UpdateUser(ctx, &api.UpdateUserRequest{
		IdentityStoreId: new(storeID), UserId: user.UserId,
		Operations: []api.AttributeOperation{{AttributePath: new("name.familyName"), AttributeValue: raw(`"Jones"`)}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = s.UpdateUser(ctx, &api.UpdateUserRequest{
		IdentityStoreId: new(storeID),
		UserId:          user.UserId,
		Operations: []api.AttributeOperation{
			{AttributePath: new("name"), AttributeValue: raw(`{"MiddleName":"unsupported"}`)},
		},
	}); err == nil {
		t.Fatal("unsupported document field was accepted")
	}

	group, err := s.CreateGroup(
		ctx,
		&api.CreateGroupRequest{IdentityStoreId: new(storeID), DisplayName: new("Developers")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateGroup(ctx, &api.CreateGroupRequest{IdentityStoreId: new(storeID), DisplayName: new("Developers")})
	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "ConflictException" {
		t.Fatal(err)
	}

	if _, err = s.CreateGroupMembership(ctx, &api.CreateGroupMembershipRequest{
		IdentityStoreId: new(storeID), GroupId: group.GroupId,
		MemberId: &api.MemberId{UserId: user.UserId},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = s.DeleteUser(
		ctx,
		&api.DeleteUserRequest{IdentityStoreId: new(storeID), UserId: user.UserId},
	); err != nil {
		t.Fatal(err)
	}

	memberships, err := s.ListGroupMemberships(
		ctx,
		&api.ListGroupMembershipsRequest{IdentityStoreId: new(storeID), GroupId: group.GroupId},
	)
	if err != nil || len(memberships.GroupMemberships) != 0 {
		t.Fatal(memberships, err)
	}

	_, err = s.DescribeUser(ctx, &api.DescribeUserRequest{IdentityStoreId: new(storeID), UserId: user.UserId})
	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "ResourceNotFoundException" {
		t.Fatal(err)
	}

	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 1 {
		t.Fatal(resources, err)
	}

	details := resources[0].Details.(map[string]any)
	if details["RecordsOnly"] != true || details["ExecutionReady"] != false {
		t.Fatal(details)
	}
}

func TestPaginationAndAccountIsolation(t *testing.T) {
	s := identitystore.New(memory.New(nil, nil))
	ctx := identityContext("123456789012", "us-east-1")
	_, storeID := identitystore.Instance("123456789012")
	for _, name := range []string{"a", "b", "c"} {
		if _, err := s.CreateGroup(
			ctx,
			&api.CreateGroupRequest{IdentityStoreId: new(storeID), DisplayName: new(name)},
		); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.ListGroups(ctx, &api.ListGroupsRequest{IdentityStoreId: new(storeID), MaxResults: new(int64(1))})
	if err != nil || first.NextToken == nil || len(first.Groups) != 1 {
		t.Fatal(first, err)
	}

	if _, err = s.DeleteGroup(
		ctx,
		&api.DeleteGroupRequest{IdentityStoreId: new(storeID), GroupId: first.Groups[0].GroupId},
	); err != nil {
		t.Fatal(err)
	}

	second, err := s.ListGroups(ctx, &api.ListGroupsRequest{IdentityStoreId: new(storeID), NextToken: first.NextToken})
	if err != nil || len(second.Groups) != 2 {
		t.Fatal(second, err)
	}

	_, otherStore := identitystore.Instance("999999999999")
	other := identityContext("999999999999", "us-east-1")
	groups, err := s.ListGroups(other, &api.ListGroupsRequest{IdentityStoreId: new(otherStore)})
	if err != nil || len(groups.Groups) != 0 {
		t.Fatal(groups, err)
	}

	if _, err = s.ListGroups(other, &api.ListGroupsRequest{IdentityStoreId: new(storeID)}); err == nil {
		t.Fatal("cross-account store accepted")
	}

	groups, err = s.ListGroups(
		identityContext("123456789012", "eu-west-1"),
		&api.ListGroupsRequest{IdentityStoreId: new(storeID)},
	)
	if err != nil || len(groups.Groups) != 2 {
		t.Fatal("account-wide records unavailable in another Region", groups, err)
	}
}

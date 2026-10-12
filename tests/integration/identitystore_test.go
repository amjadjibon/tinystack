package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/identitystore"
	"github.com/aws/aws-sdk-go-v2/service/identitystore/document"
	"github.com/aws/aws-sdk-go-v2/service/identitystore/types"
)

func identityStoreClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestIdentityStoreSDKLifecycle(t *testing.T) {
	server := server(t)
	ctx := context.Background()
	c := identityStoreClient(server)
	instances, err := ssoAdminClient(server).ListInstances(ctx, nil)
	if err != nil || len(instances.Instances) != 1 {
		t.Fatal(instances, err)
	}
	storeID := instances.Instances[0].IdentityStoreId
	user, err := c.CreateUser(ctx, &sdk.CreateUserInput{
		IdentityStoreId: storeID, UserName: new("alice"), DisplayName: new("Alice"),
		Name:   &types.Name{GivenName: new("Alice"), FamilyName: new("Smith")},
		Emails: []types.Email{{Value: new("alice@example.com"), Type: new("work"), Primary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateUser(ctx, &sdk.CreateUserInput{IdentityStoreId: storeID, UserName: new("alice")})
	if _, ok := errors.AsType[*types.ConflictException](err); !ok {
		t.Fatal("expected modeled ConflictException", err)
	}
	found, err := c.GetUserId(
		ctx,
		&sdk.GetUserIdInput{
			IdentityStoreId: storeID,
			AlternateIdentifier: &types.AlternateIdentifierMemberUniqueAttribute{
				Value: types.UniqueAttribute{
					AttributePath:  new("userName"),
					AttributeValue: document.NewLazyDocument("alice"),
				},
			},
		},
	)
	if err != nil || *found.UserId != *user.UserId {
		t.Fatal(found, err)
	}
	if _, err = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{IdentityStoreId: storeID, UserId: user.UserId, Operations: []types.AttributeOperation{
			{AttributePath: new("displayName"), AttributeValue: document.NewLazyDocument("Alice Updated")},
			{AttributePath: new("name.familyName"), AttributeValue: document.NewLazyDocument("Jones")},
		}},
	); err != nil {
		t.Fatal(err)
	}
	described, err := c.DescribeUser(ctx, &sdk.DescribeUserInput{IdentityStoreId: storeID, UserId: user.UserId})
	if err != nil || *described.DisplayName != "Alice Updated" || *described.Name.FamilyName != "Jones" ||
		len(described.Emails) != 1 {
		t.Fatal(described, err)
	}
	//nolint:staticcheck // The brief explicitly selects the modeled legacy UserName filter.
	users, err := c.ListUsers(ctx, &sdk.ListUsersInput{IdentityStoreId: storeID, Filters: []types.Filter{
		{AttributePath: new("UserName"), AttributeValue: new("alice")},
	}})
	if err != nil || len(users.Users) != 1 {
		t.Fatal(users, err)
	}
	group, err := c.CreateGroup(
		ctx,
		&sdk.CreateGroupInput{IdentityStoreId: storeID, DisplayName: new("Developers"), Description: new("Initial")},
	)
	if err != nil {
		t.Fatal(err)
	}
	groupID, err := c.GetGroupId(
		ctx,
		&sdk.GetGroupIdInput{
			IdentityStoreId: storeID,
			AlternateIdentifier: &types.AlternateIdentifierMemberUniqueAttribute{
				Value: types.UniqueAttribute{
					AttributePath:  new("displayName"),
					AttributeValue: document.NewLazyDocument("Developers"),
				},
			},
		},
	)
	if err != nil || *groupID.GroupId != *group.GroupId {
		t.Fatal(groupID, err)
	}
	if _, err = c.UpdateGroup(
		ctx,
		&sdk.UpdateGroupInput{IdentityStoreId: storeID, GroupId: group.GroupId, Operations: []types.AttributeOperation{
			{AttributePath: new("description"), AttributeValue: document.NewLazyDocument("Updated")},
		}},
	); err != nil {
		t.Fatal(err)
	}
	gd, err := c.DescribeGroup(ctx, &sdk.DescribeGroupInput{IdentityStoreId: storeID, GroupId: group.GroupId})
	if err != nil || *gd.Description != "Updated" {
		t.Fatal(gd, err)
	}
	groups, err := c.ListGroups(ctx, &sdk.ListGroupsInput{IdentityStoreId: storeID})
	if err != nil || len(groups.Groups) != 1 {
		t.Fatal(groups, err)
	}
	member := &types.MemberIdMemberUserId{Value: *user.UserId}
	membership, err := c.CreateGroupMembership(
		ctx,
		&sdk.CreateGroupMembershipInput{IdentityStoreId: storeID, GroupId: group.GroupId, MemberId: member},
	)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := c.GetGroupMembershipId(
		ctx,
		&sdk.GetGroupMembershipIdInput{IdentityStoreId: storeID, GroupId: group.GroupId, MemberId: member},
	)
	if err != nil || *mid.MembershipId != *membership.MembershipId {
		t.Fatal(mid, err)
	}
	members, err := c.ListGroupMemberships(
		ctx,
		&sdk.ListGroupMembershipsInput{IdentityStoreId: storeID, GroupId: group.GroupId},
	)
	if err != nil || len(members.GroupMemberships) != 1 {
		t.Fatal(members, err)
	}
	byMember, err := c.ListGroupMembershipsForMember(
		ctx,
		&sdk.ListGroupMembershipsForMemberInput{IdentityStoreId: storeID, MemberId: member},
	)
	if err != nil || len(byMember.GroupMemberships) != 1 {
		t.Fatal(byMember, err)
	}
	exists, err := c.IsMemberInGroups(
		ctx,
		&sdk.IsMemberInGroupsInput{IdentityStoreId: storeID, MemberId: member, GroupIds: []string{*group.GroupId}},
	)
	if err != nil || len(exists.Results) != 1 || !exists.Results[0].MembershipExists {
		t.Fatal(exists, err)
	}
	if _, err = c.DeleteGroupMembership(
		ctx,
		&sdk.DeleteGroupMembershipInput{IdentityStoreId: storeID, MembershipId: membership.MembershipId},
	); err != nil {
		t.Fatal(err)
	}
	exists, err = c.IsMemberInGroups(
		ctx,
		&sdk.IsMemberInGroupsInput{IdentityStoreId: storeID, MemberId: member, GroupIds: []string{*group.GroupId}},
	)
	if err != nil || exists.Results[0].MembershipExists {
		t.Fatal(exists, err)
	}
	if _, err = c.CreateGroupMembership(
		ctx,
		&sdk.CreateGroupMembershipInput{IdentityStoreId: storeID, GroupId: group.GroupId, MemberId: member},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = c.DeleteGroup(
		ctx,
		&sdk.DeleteGroupInput{IdentityStoreId: storeID, GroupId: group.GroupId},
	); err != nil {
		t.Fatal(err)
	}
	byMember, err = c.ListGroupMembershipsForMember(
		ctx,
		&sdk.ListGroupMembershipsForMemberInput{IdentityStoreId: storeID, MemberId: member},
	)
	if err != nil || len(byMember.GroupMemberships) != 0 {
		t.Fatal(byMember, err)
	}
	if _, err = c.DeleteUser(ctx, &sdk.DeleteUserInput{IdentityStoreId: storeID, UserId: user.UserId}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeUser(ctx, &sdk.DescribeUserInput{IdentityStoreId: storeID, UserId: user.UserId})
	if _, ok := errors.AsType[*types.ResourceNotFoundException](err); !ok {
		t.Fatal("expected modeled ResourceNotFoundException", err)
	}
}

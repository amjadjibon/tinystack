package integration_test

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

const readOnly = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:Get*","Resource":"*"}]}`

func TestIAMGroupsAndUserPolicies(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, name := range []string{"alice", "bob"} {
		if _, err := c.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String(name)}); err != nil {
			t.Fatal(err)
		}
	}

	created, err := c.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: aws.String("devs"), Path: aws.String("/team/")})
	if err != nil || aws.ToString(created.Group.Arn) != "arn:aws:iam::000000000000:group/team/devs" ||
		len(aws.ToString(created.Group.GroupId)) != 20 {
		t.Fatalf("got %+v, %v", created, err)
	}

	_, err = c.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: aws.String("DEVS")})
	wantCode(t, err, "EntityAlreadyExists")
	_, err = iamClient(s, "team-b", "us-east-1").GetGroup(ctx, &iam.GetGroupInput{GroupName: aws.String("devs")})
	wantCode(t, err, "NoSuchEntity")

	// Membership: adding a member again changes nothing.
	for _, name := range []string{"bob", "alice", "ALICE"} {
		if _, err = c.AddUserToGroup(ctx, &iam.AddUserToGroupInput{
			GroupName: aws.String("devs"), UserName: aws.String(name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.AddUserToGroup(ctx, &iam.AddUserToGroupInput{GroupName: aws.String("devs"), UserName: aws.String("x")})
	wantCode(t, err, "NoSuchEntity")
	members := func(group string) []string {
		t.Helper()
		var names []string
		paginator := iam.NewGetGroupPaginator(
			c,
			&iam.GetGroupInput{GroupName: aws.String(group), MaxItems: aws.Int32(1)},
		)
		for paginator.HasMorePages() {
			out, pageErr := paginator.NextPage(ctx)
			if pageErr != nil {
				t.Fatal(pageErr)
			}

			for _, u := range out.Users {
				names = append(names, aws.ToString(u.UserName))
			}
		}

		return names
	}

	if got := members("devs"); !slices.Equal(got, []string{"alice", "bob"}) {
		t.Fatalf("members %v", got)
	}

	groupsOf := func(user string) []string {
		t.Helper()
		out, listErr := c.ListGroupsForUser(ctx, &iam.ListGroupsForUserInput{UserName: aws.String(user)})
		if listErr != nil {
			t.Fatal(listErr)
		}

		var names []string
		for _, g := range out.Groups {
			names = append(names, aws.ToString(g.GroupName))
		}

		return names
	}

	if got := groupsOf("alice"); !slices.Equal(got, []string{"devs"}) {
		t.Fatalf("groups %v", got)
	}

	listed, err := c.ListGroups(ctx, &iam.ListGroupsInput{PathPrefix: aws.String("/team/")})
	if err != nil || len(listed.Groups) != 1 {
		t.Fatalf("got %+v, %v", listed, err)
	}

	// Inline and managed policies, for users and groups alike.
	policy, err := c.CreatePolicy(ctx, &iam.CreatePolicyInput{
		PolicyName: aws.String("read-only"), PolicyDocument: aws.String(readOnly),
	})
	if err != nil {
		t.Fatal(err)
	}

	policyArn := policy.Policy.Arn
	if _, err = c.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName: aws.String("alice"), PolicyName: aws.String("own"), PolicyDocument: aws.String(readOnly),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutGroupPolicy(ctx, &iam.PutGroupPolicyInput{
		GroupName: aws.String("devs"), PolicyName: aws.String("shared"), PolicyDocument: aws.String(readOnly),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutGroupPolicy(ctx, &iam.PutGroupPolicyInput{
		GroupName: aws.String("devs"), PolicyName: aws.String("bad"), PolicyDocument: aws.String("{"),
	})
	wantCode(t, err, "MalformedPolicyDocument")
	if _, err = c.AttachUserPolicy(ctx, &iam.AttachUserPolicyInput{
		UserName: aws.String("alice"), PolicyArn: policyArn,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AttachGroupPolicy(ctx, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("devs"), PolicyArn: policyArn,
	}); err != nil {
		t.Fatal(err)
	}

	userPolicy, err := c.GetUserPolicy(ctx, &iam.GetUserPolicyInput{
		UserName: aws.String("alice"), PolicyName: aws.String("OWN"),
	})
	if err != nil || aws.ToString(userPolicy.PolicyName) != "own" ||
		aws.ToString(userPolicy.PolicyDocument) != url.QueryEscape(readOnly) {
		t.Fatalf("got %+v, %v", userPolicy, err)
	}

	groupPolicies, err := c.ListGroupPolicies(ctx, &iam.ListGroupPoliciesInput{GroupName: aws.String("devs")})
	if err != nil || !slices.Equal(groupPolicies.PolicyNames, []string{"shared"}) {
		t.Fatalf("got %+v, %v", groupPolicies, err)
	}

	_, err = c.GetGroupPolicy(ctx, &iam.GetGroupPolicyInput{GroupName: aws.String("devs"), PolicyName: aws.String("x")})
	wantCode(t, err, "NoSuchEntity")
	attached, err := c.ListAttachedUserPolicies(ctx, &iam.ListAttachedUserPoliciesInput{UserName: aws.String("alice")})
	if err != nil || len(attached.AttachedPolicies) != 1 ||
		aws.ToString(attached.AttachedPolicies[0].PolicyArn) != aws.ToString(policyArn) {
		t.Fatalf("got %+v, %v", attached, err)
	}

	groupAttached, err := c.ListAttachedGroupPolicies(ctx, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("devs"),
	})
	if err != nil || len(groupAttached.AttachedPolicies) != 1 {
		t.Fatalf("got %+v, %v", groupAttached, err)
	}

	got, err := c.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: policyArn})
	if err != nil || aws.ToInt32(got.Policy.AttachmentCount) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}

	_, err = c.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: policyArn})
	wantCode(t, err, "DeleteConflict")

	// Renaming a user or group keeps its memberships and policies.
	if _, err = c.UpdateUser(ctx, &iam.UpdateUserInput{
		UserName: aws.String("alice"), NewUserName: aws.String("alicia"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UpdateGroup(ctx, &iam.UpdateGroupInput{
		GroupName: aws.String("devs"), NewGroupName: aws.String("engineers"), NewPath: aws.String("/"),
	}); err != nil {
		t.Fatal(err)
	}

	if got := members("engineers"); !slices.Equal(got, []string{"alicia", "bob"}) {
		t.Fatalf("members %v", got)
	}

	if got := groupsOf("alicia"); !slices.Equal(got, []string{"engineers"}) {
		t.Fatalf("groups %v", got)
	}

	// Users and groups with policies or members cannot be deleted.
	for _, step := range []struct {
		call func() error
		code string
	}{
		{func() error {
			_, e := c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String("alicia")})
			return e
		}, "DeleteConflict"},
		{func() error {
			_, e := c.DeleteUserPolicy(ctx, &iam.DeleteUserPolicyInput{
				UserName: aws.String("alicia"), PolicyName: aws.String("own"),
			})
			return e
		}, ""},
		{func() error {
			_, e := c.DetachUserPolicy(ctx, &iam.DetachUserPolicyInput{
				UserName: aws.String("alicia"), PolicyArn: policyArn,
			})
			return e
		}, ""},
		{func() error {
			_, e := c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String("alicia")})
			return e
		}, "DeleteConflict"},
		{func() error {
			_, e := c.DeleteGroup(ctx, &iam.DeleteGroupInput{GroupName: aws.String("engineers")})
			return e
		}, "DeleteConflict"},
		{func() error {
			_, e := c.RemoveUserFromGroup(ctx, &iam.RemoveUserFromGroupInput{
				GroupName: aws.String("engineers"), UserName: aws.String("alicia"),
			})
			return e
		}, ""},
		{func() error {
			_, e := c.RemoveUserFromGroup(ctx, &iam.RemoveUserFromGroupInput{
				GroupName: aws.String("engineers"), UserName: aws.String("alicia"),
			})
			return e
		}, "NoSuchEntity"},
		{func() error {
			_, e := c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String("alicia")})
			return e
		}, ""},
		{func() error {
			_, e := c.RemoveUserFromGroup(ctx, &iam.RemoveUserFromGroupInput{
				GroupName: aws.String("engineers"), UserName: aws.String("bob"),
			})
			return e
		}, ""},
		{func() error {
			_, e := c.DeleteGroup(ctx, &iam.DeleteGroupInput{GroupName: aws.String("engineers")})
			return e
		}, "DeleteConflict"},
		{func() error {
			_, e := c.DeleteGroupPolicy(ctx, &iam.DeleteGroupPolicyInput{
				GroupName: aws.String("engineers"), PolicyName: aws.String("shared"),
			})
			return e
		}, ""},
		{func() error {
			_, e := c.DetachGroupPolicy(ctx, &iam.DetachGroupPolicyInput{
				GroupName: aws.String("engineers"), PolicyArn: policyArn,
			})
			return e
		}, ""},
		{func() error {
			_, e := c.DeleteGroup(ctx, &iam.DeleteGroupInput{GroupName: aws.String("engineers")})
			return e
		}, ""},
		{func() error {
			_, e := c.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: policyArn})
			return e
		}, ""},
	} {
		if err = step.call(); step.code == "" && err != nil {
			t.Fatal(err)
		} else if step.code != "" {
			wantCode(t, err, step.code)
		}
	}

	// A user belongs to at most 10 groups.
	for i := range 11 {
		name := fmt.Sprintf("g%d", i)
		if _, err = c.CreateGroup(ctx, &iam.CreateGroupInput{GroupName: aws.String(name)}); err != nil {
			t.Fatal(err)
		}

		_, err = c.AddUserToGroup(
			ctx,
			&iam.AddUserToGroupInput{GroupName: aws.String(name), UserName: aws.String("bob")},
		)
		if i < 10 && err != nil {
			t.Fatal(err)
		}
	}

	wantCode(t, err, "LimitExceeded")
}

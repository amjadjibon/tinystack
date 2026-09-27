package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"github.com/amjadjibon/tinystack/internal/delivery"
)

// cognitoMessage returns the newest message a user pool sent to target, such
// as "email:ana@example.com", from the delivery log.
func cognitoMessage(t *testing.T, s *httptest.Server, target string) (subject, text, secret string) {
	t.Helper()
	var out struct {
		Deliveries []delivery.Record `json:"deliveries"`
	}

	if status := internal(t, s, http.MethodGet, "/_tinystack/deliveries", &out); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}

	for i := range out.Deliveries {
		d := &out.Deliveries[i]
		if d.Via != delivery.ViaCognito || d.Target != target {
			continue
		}

		var m struct{ Subject, Message, Code string }
		if err := json.Unmarshal([]byte(d.Payload), &m); err != nil {
			t.Fatal(err)
		}

		return m.Subject, m.Message, m.Code
	}

	t.Fatalf("no message to %s in %+v", target, out.Deliveries)
	return "", "", ""
}

func attribute(attrs []ciptypes.AttributeType, name string) string {
	for _, a := range attrs {
		if aws.ToString(a.Name) == name {
			return aws.ToString(a.Value)
		}
	}

	return ""
}

func attrs(pairs ...string) []ciptypes.AttributeType {
	out := make([]ciptypes.AttributeType, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, ciptypes.AttributeType{Name: aws.String(pairs[i]), Value: aws.String(pairs[i+1])})
	}

	return out
}

func TestCognitoAdminUsers(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, c, "admin users", &cip.CreateUserPoolInput{
		AliasAttributes:        []ciptypes.AliasAttributeType{ciptypes.AliasAttributeTypeEmail},
		AutoVerifiedAttributes: []ciptypes.VerifiedAttributeType{ciptypes.VerifiedAttributeTypeEmail},
		UsernameConfiguration:  &ciptypes.UsernameConfigurationType{CaseSensitive: aws.Bool(false)},
		Schema: []ciptypes.SchemaAttributeType{
			{Name: aws.String("plan"), AttributeDataType: ciptypes.AttributeDataTypeString},
			{
				Name: aws.String("seats"), AttributeDataType: ciptypes.AttributeDataTypeNumber,
				NumberAttributeConstraints: &ciptypes.NumberAttributeConstraintsType{MaxValue: aws.String("10")},
			},
		},
		AdminCreateUserConfig: &ciptypes.AdminCreateUserConfigType{InviteMessageTemplate: &ciptypes.MessageTemplateType{
			EmailSubject: aws.String("Welcome"), EmailMessage: aws.String("Hi {username}, use {####}"),
		}},
	})

	created, err := c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("Ana"),
		UserAttributes:         attrs("email", "ana@example.com", "email_verified", "true", "custom:plan", "pro"),
		DesiredDeliveryMediums: []ciptypes.DeliveryMediumType{ciptypes.DeliveryMediumTypeEmail},
	})
	if err != nil {
		t.Fatal(err)
	}

	u := created.User
	if aws.ToString(u.Username) != "Ana" || u.UserStatus != ciptypes.UserStatusTypeForceChangePassword ||
		!u.Enabled || len(attribute(u.Attributes, "sub")) != 36 || aws.ToString(u.Attributes[0].Name) != "sub" {
		t.Fatalf("user = %+v", u)
	}

	subject, text, password := cognitoMessage(t, s, "email:ana@example.com")
	if subject != "Welcome" || text != "Hi Ana, use "+password || len(password) < 8 {
		t.Fatalf("invitation = %q %q", subject, text)
	}

	// Names ignore case, and a verified email is an alias.
	for _, name := range []string{"ana", "ANA@example.com"} {
		got, getErr := c.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: aws.String(name)})
		if getErr != nil || aws.ToString(got.Username) != "Ana" {
			t.Fatalf("%s: %+v, %v", name, got, getErr)
		}
	}

	_, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{UserPoolId: pool.Id, Username: aws.String("ANA")})
	code(t, err, "UsernameExistsException")

	_, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("bo"), MessageAction: ciptypes.MessageActionTypeSuppress,
		UserAttributes: attrs("email", "ana@example.com", "email_verified", "true"),
	})
	code(t, err, "AliasExistsException")

	for name, in := range map[string]*cip.AdminCreateUserInput{
		"unknown attribute": {UserAttributes: attrs("custom:missing", "x")},
		"bad number":        {UserAttributes: attrs("custom:seats", "11")},
		"bad email":         {UserAttributes: attrs("email", "not-an-email")},
		"bad phone":         {UserAttributes: attrs("phone_number", "555-1234")},
		"sub":               {UserAttributes: attrs("sub", "mine")},
	} {
		t.Run(name, func(t *testing.T) {
			in.UserPoolId, in.Username = pool.Id, aws.String("bad")
			_, createErr := c.AdminCreateUser(ctx, in)
			code(t, createErr, "InvalidParameterException")
		})
	}

	_, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("weak"), TemporaryPassword: aws.String("short"),
	})
	code(t, err, "InvalidPasswordException")

	for _, name := range []string{"bo", "cy"} {
		if _, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
			UserPoolId: pool.Id, Username: aws.String(name), MessageAction: ciptypes.MessageActionTypeSuppress,
			UserAttributes: attrs("email", name+"@example.org", "custom:plan", "free"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Changing an email address unverifies it and sends a code.
	if _, err = c.AdminUpdateUserAttributes(ctx, &cip.AdminUpdateUserAttributesInput{
		UserPoolId: pool.Id, Username: aws.String("ana"), UserAttributes: attrs("email", "ana@example.net"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, verification := cognitoMessage(t, s, "email:ana@example.net"); len(verification) != 6 {
		t.Fatalf("verification code = %q", verification)
	}

	got, err := c.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: aws.String("ana")})
	if err != nil || attribute(got.UserAttributes, "email_verified") != "false" {
		t.Fatalf("updated = %+v, %v", got, err)
	}

	// The unverified address is no longer an alias.
	_, err = c.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: aws.String("ana@example.com")})
	code(t, err, "UserNotFoundException")

	if _, err = c.AdminDeleteUserAttributes(ctx, &cip.AdminDeleteUserAttributesInput{
		UserPoolId: pool.Id, Username: aws.String("ana"), UserAttributeNames: []string{"custom:plan"},
	}); err != nil {
		t.Fatal(err)
	}

	// Filters, attribute selection, and paging.
	for filter, want := range map[string][]string{
		`custom:plan = "free"`: nil,
		`email ^= "bo"`:        {"bo"},
		`username = "ANA"`:     {"Ana"},
		`cognito:user_status = "FORCE_CHANGE_PASSWORD"`: {"Ana", "bo", "cy"},
	} {
		listed, listErr := c.ListUsers(ctx, &cip.ListUsersInput{UserPoolId: pool.Id, Filter: aws.String(filter)})
		if filter == `custom:plan = "free"` {
			code(t, listErr, "InvalidParameterException")
			continue
		}

		if listErr != nil {
			t.Fatal(listErr)
		}

		var names []string
		for _, lu := range listed.Users {
			names = append(names, aws.ToString(lu.Username))
		}

		if !slices.Equal(names, want) {
			t.Fatalf("%s = %v", filter, names)
		}
	}

	first, err := c.ListUsers(ctx, &cip.ListUsersInput{
		UserPoolId: pool.Id, Limit: aws.Int32(2), AttributesToGet: []string{"email"},
	})
	if err != nil || len(first.Users) != 2 || first.PaginationToken == nil || len(first.Users[0].Attributes) != 1 {
		t.Fatalf("first page = %+v, %v", first, err)
	}

	rest, err := c.ListUsers(ctx, &cip.ListUsersInput{
		UserPoolId: pool.Id, Limit: aws.Int32(2), PaginationToken: first.PaginationToken,
	})
	if err != nil || len(rest.Users) != 1 || aws.ToString(rest.Users[0].Username) != "cy" {
		t.Fatalf("second page = %+v, %v", rest, err)
	}

	// Passwords, confirmation, and disabling.
	if _, err = c.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: pool.Id, Username: aws.String("bo"), Password: aws.String("Correct-Horse-9"), Permanent: true,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: pool.Id, Username: aws.String("bo"), Password: aws.String("short"),
	})
	code(t, err, "InvalidPasswordException")

	_, err = c.AdminConfirmSignUp(ctx, &cip.AdminConfirmSignUpInput{UserPoolId: pool.Id, Username: aws.String("bo")})
	code(t, err, "NotAuthorizedException")

	if _, err = c.AdminDisableUser(ctx, &cip.AdminDisableUserInput{
		UserPoolId: pool.Id, Username: aws.String("bo"),
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListUsers(ctx, &cip.ListUsersInput{UserPoolId: pool.Id, Filter: aws.String(`status = "Disabled"`)})
	if err != nil || len(listed.Users) != 1 || listed.Users[0].UserStatus != ciptypes.UserStatusTypeConfirmed {
		t.Fatalf("disabled = %+v, %v", listed, err)
	}

	if _, err = c.AdminEnableUser(ctx, &cip.AdminEnableUserInput{
		UserPoolId: pool.Id, Username: aws.String("bo"),
	}); err != nil {
		t.Fatal(err)
	}

	// A reset needs a verified address to send the code to.
	_, err = c.AdminResetUserPassword(ctx, &cip.AdminResetUserPasswordInput{
		UserPoolId: pool.Id, Username: aws.String("bo"),
	})
	code(t, err, "InvalidParameterException")

	if _, err = c.AdminUpdateUserAttributes(ctx, &cip.AdminUpdateUserAttributesInput{
		UserPoolId: pool.Id, Username: aws.String("bo"), UserAttributes: attrs("email_verified", "true"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AdminResetUserPassword(ctx, &cip.AdminResetUserPasswordInput{
		UserPoolId: pool.Id, Username: aws.String("bo@example.org"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: aws.String("bo")})
	if err != nil || got.UserStatus != ciptypes.UserStatusTypeResetRequired {
		t.Fatalf("reset = %+v, %v", got, err)
	}

	if _, err = c.AdminDeleteUser(ctx, &cip.AdminDeleteUserInput{
		UserPoolId: pool.Id, Username: aws.String("bo"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.AdminGetUser(ctx, &cip.AdminGetUserInput{UserPoolId: pool.Id, Username: aws.String("bo@example.org")})
	code(t, err, "UserNotFoundException")

	described, err := c.DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: pool.Id})
	if err != nil || described.UserPool.EstimatedNumberOfUsers != 2 {
		t.Fatalf("pool = %+v, %v", described, err)
	}
}

func TestCognitoEmailUsernames(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, c, "email sign-in", &cip.CreateUserPoolInput{
		UsernameAttributes: []ciptypes.UsernameAttributeType{ciptypes.UsernameAttributeTypeEmail},
	})

	created, err := c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("dee@example.com"),
		MessageAction: ciptypes.MessageActionTypeSuppress,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The username is the user's sub, and the email address names it.
	u := created.User
	if aws.ToString(u.Username) != attribute(u.Attributes, "sub") ||
		attribute(u.Attributes, "email") != "dee@example.com" {
		t.Fatalf("user = %+v", u)
	}

	if _, err = c.AdminGetUser(ctx, &cip.AdminGetUserInput{
		UserPoolId: pool.Id, Username: aws.String("DEE@example.com"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("dee@example.com"),
	})
	code(t, err, "UsernameExistsException")

	_, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{UserPoolId: pool.Id, Username: aws.String("dee")})
	code(t, err, "InvalidParameterException")
}

func TestCognitoGroups(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, c, "groups", nil)
	for _, name := range []string{"admins", "readers"} {
		out, err := c.CreateGroup(ctx, &cip.CreateGroupInput{
			UserPoolId: pool.Id, GroupName: aws.String(name), Precedence: aws.Int32(1),
			RoleArn: aws.String("arn:aws:iam::000000000000:role/" + name),
		})
		if err != nil || aws.ToString(out.Group.GroupName) != name {
			t.Fatalf("group = %+v, %v", out, err)
		}
	}

	_, err := c.CreateGroup(ctx, &cip.CreateGroupInput{UserPoolId: pool.Id, GroupName: aws.String("admins")})
	code(t, err, "GroupExistsException")

	updated, err := c.UpdateGroup(ctx, &cip.UpdateGroupInput{
		UserPoolId: pool.Id, GroupName: aws.String("admins"), Description: aws.String("Operators"),
	})
	if err != nil || aws.ToString(updated.Group.Description) != "Operators" ||
		aws.ToString(updated.Group.RoleArn) == "" || aws.ToInt32(updated.Group.Precedence) != 1 {
		t.Fatalf("updated = %+v, %v", updated, err)
	}

	for _, name := range []string{"ann", "ben"} {
		if _, err = c.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
			UserPoolId: pool.Id, Username: aws.String(name), MessageAction: ciptypes.MessageActionTypeSuppress,
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = c.AdminAddUserToGroup(ctx, &cip.AdminAddUserToGroupInput{
			UserPoolId: pool.Id, Username: aws.String(name), GroupName: aws.String("readers"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.AdminAddUserToGroup(ctx, &cip.AdminAddUserToGroupInput{
		UserPoolId: pool.Id, Username: aws.String("ann"), GroupName: aws.String("admins"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.AdminAddUserToGroup(ctx, &cip.AdminAddUserToGroupInput{
		UserPoolId: pool.Id, Username: aws.String("ann"), GroupName: aws.String("missing"),
	})
	code(t, err, "ResourceNotFoundException")

	_, err = c.AdminAddUserToGroup(ctx, &cip.AdminAddUserToGroupInput{
		UserPoolId: pool.Id, Username: aws.String("nobody"), GroupName: aws.String("admins"),
	})
	code(t, err, "UserNotFoundException")

	groups, err := c.AdminListGroupsForUser(ctx, &cip.AdminListGroupsForUserInput{
		UserPoolId: pool.Id, Username: aws.String("ann"),
	})
	if err != nil || len(groups.Groups) != 2 || aws.ToString(groups.Groups[0].GroupName) != "admins" {
		t.Fatalf("groups = %+v, %v", groups, err)
	}

	members, err := c.ListUsersInGroup(ctx, &cip.ListUsersInGroupInput{
		UserPoolId: pool.Id, GroupName: aws.String("readers"), Limit: aws.Int32(1),
	})
	if err != nil || len(members.Users) != 1 || members.NextToken == nil {
		t.Fatalf("members = %+v, %v", members, err)
	}

	if _, err = c.AdminRemoveUserFromGroup(ctx, &cip.AdminRemoveUserFromGroupInput{
		UserPoolId: pool.Id, Username: aws.String("ben"), GroupName: aws.String("readers"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteGroup(ctx, &cip.DeleteGroupInput{
		UserPoolId: pool.Id, GroupName: aws.String("readers"),
	}); err != nil {
		t.Fatal(err)
	}

	groups, err = c.AdminListGroupsForUser(ctx, &cip.AdminListGroupsForUserInput{
		UserPoolId: pool.Id, Username: aws.String("ann"),
	})
	if err != nil || len(groups.Groups) != 1 {
		t.Fatalf("groups after delete = %+v, %v", groups, err)
	}

	listed, err := c.ListGroups(ctx, &cip.ListGroupsInput{UserPoolId: pool.Id})
	if err != nil || len(listed.Groups) != 1 {
		t.Fatalf("listed = %+v, %v", listed, err)
	}

	_, err = c.GetGroup(ctx, &cip.GetGroupInput{UserPoolId: pool.Id, GroupName: aws.String("readers")})
	code(t, err, "ResourceNotFoundException")
}

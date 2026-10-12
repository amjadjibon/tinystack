package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	idsdk "github.com/aws/aws-sdk-go-v2/service/identitystore"
	ssosdk "github.com/aws/aws-sdk-go-v2/service/ssoadmin"
)

func identitySSOTemplate(instance, storeID, userID string, updated bool, fail string) string {
	permission := map[string]any{
		"InstanceArn":     instance,
		"Name":            "StackPermission",
		"Description":     "Initial",
		"SessionDuration": "PT2H",
		"RelayStateType":  "https://example.com",
		"ManagedPolicies": []any{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
		"InlinePolicy": map[string]any{
			"Version":   "2012-10-17",
			"Statement": []any{},
		},
		"Tags": []any{map[string]any{"Key": "env", "Value": "test"}},
	}
	group := map[string]any{"IdentityStoreId": storeID, "DisplayName": "StackGroup", "Description": "Initial"}
	target := "123456789012"
	if updated {
		permission["Description"] = "Updated"
		delete(permission, "SessionDuration")
		delete(permission, "InlinePolicy")
		delete(permission, "ManagedPolicies")
		permission["Tags"] = []any{map[string]any{"Key": "team", "Value": "platform"}}
		group["DisplayName"] = "UpdatedGroup"
		delete(group, "Description")
		target = "999999999999"
	}
	switch fail {
	case "Description", "RelayStateType":
		delete(permission, fail)
	case "PermissionsBoundary":
		permission[fail] = map[string]any{"ManagedPolicyArn": "arn:aws:iam::aws:policy/ReadOnlyAccess"}
	case "CustomerManagedPolicyReferences":
		permission[fail] = []any{map[string]any{"Name": "Customer"}}
	}
	get := func(resource, attribute string) map[string]any {
		return map[string]any{"Fn::GetAtt": []any{resource, attribute}}
	}
	body, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"Group": map[string]any{"Type": "AWS::IdentityStore::Group", "Properties": group},
			"Membership": map[string]any{"Type": "AWS::IdentityStore::GroupMembership", "Properties": map[string]any{
				"IdentityStoreId": storeID,
				"GroupId":         get("Group", "GroupId"),
				"MemberId":        map[string]any{"UserId": userID},
			}},
			"Permission": map[string]any{"Type": "AWS::SSO::PermissionSet", "Properties": permission},
			"Assignment": map[string]any{"Type": "AWS::SSO::Assignment", "Properties": map[string]any{
				"InstanceArn":      instance,
				"PermissionSetArn": get("Permission", "PermissionSetArn"),
				"PrincipalId":      get("Group", "GroupId"),
				"PrincipalType":    "GROUP",
				"TargetId":         target,
				"TargetType":       "AWS_ACCOUNT",
			}},
		},
		"Outputs": map[string]any{
			"GroupRef":      map[string]any{"Value": map[string]any{"Ref": "Group"}},
			"GroupId":       map[string]any{"Value": get("Group", "GroupId")},
			"MembershipRef": map[string]any{"Value": map[string]any{"Ref": "Membership"}},
			"MembershipId":  map[string]any{"Value": get("Membership", "MembershipId")},
			"PermissionRef": map[string]any{"Value": map[string]any{"Ref": "Permission"}},
			"PermissionArn": map[string]any{"Value": get("Permission", "PermissionSetArn")},
			"AssignmentRef": map[string]any{"Value": map[string]any{"Ref": "Assignment"}},
		},
	})
	return string(body)
}

func TestCloudFormationIdentityStoreSSOAdminLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	ids := identityStoreClient(s)
	sso := ssoAdminClient(s)
	instances, err := sso.ListInstances(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	instance := *instances.Instances[0].InstanceArn
	storeID := *instances.Instances[0].IdentityStoreId
	user, err := ids.CreateUser(ctx, &idsdk.CreateUserInput{IdentityStoreId: new(storeID), UserName: new("stack-user")})
	if err != nil {
		t.Fatal(err)
	}
	initial := identitySSOTemplate(instance, storeID, *user.UserId, false, "")
	_, err = cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("identitysso-stack"), TemplateBody: new(initial)})
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, cf, "identitysso-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	if outputs["GroupRef"] != outputs["GroupId"]+"|"+storeID ||
		outputs["MembershipRef"] != outputs["MembershipId"]+"|"+storeID ||
		outputs["PermissionRef"] != outputs["PermissionArn"]+"|"+instance ||
		!strings.Contains(outputs["AssignmentRef"], "123456789012") {
		t.Fatal(outputs)
	}
	updated := identitySSOTemplate(instance, storeID, *user.UserId, true, "")
	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("identitysso-stack"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}
	stack = wantStatus(ctx, t, cf, "identitysso-stack", cfntypes.StackStatusUpdateComplete)
	after := outputsOf(&stack)
	if after["GroupId"] != outputs["GroupId"] || after["PermissionArn"] != outputs["PermissionArn"] ||
		after["AssignmentRef"] == outputs["AssignmentRef"] {
		t.Fatal("incorrect replacement behavior", after)
	}
	group, err := ids.DescribeGroup(
		ctx,
		&idsdk.DescribeGroupInput{IdentityStoreId: new(storeID), GroupId: new(after["GroupId"])},
	)
	if err != nil || *group.DisplayName != "UpdatedGroup" || group.Description != nil {
		t.Fatal(group, err)
	}
	permission, err := sso.DescribePermissionSet(
		ctx,
		&ssosdk.DescribePermissionSetInput{InstanceArn: new(instance), PermissionSetArn: new(after["PermissionArn"])},
	)
	if err != nil || *permission.PermissionSet.Description != "Updated" ||
		*permission.PermissionSet.SessionDuration != "PT1H" {
		t.Fatal(permission, err)
	}
	target := &ssosdk.ListManagedPoliciesInPermissionSetInput{
		InstanceArn:      new(instance),
		PermissionSetArn: new(after["PermissionArn"]),
	}
	managed, err := sso.ListManagedPoliciesInPermissionSet(ctx, target)
	if err != nil || len(managed.AttachedManagedPolicies) != 0 {
		t.Fatal(managed, err)
	}
	inline, err := sso.GetInlinePolicyForPermissionSet(
		ctx,
		&ssosdk.GetInlinePolicyForPermissionSetInput{
			InstanceArn:      new(instance),
			PermissionSetArn: new(after["PermissionArn"]),
		},
	)
	if err != nil || *inline.InlinePolicy != "" {
		t.Fatal(inline, err)
	}
	tags, err := sso.ListTagsForResource(
		ctx,
		&ssosdk.ListTagsForResourceInput{InstanceArn: new(instance), ResourceArn: new(after["PermissionArn"])},
	)
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "team" {
		t.Fatal(tags, err)
	}
	for _, failure := range []string{
		"Description", "RelayStateType", "PermissionsBoundary", "CustomerManagedPolicyReferences",
	} {
		_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
			StackName:    new("identitysso-stack"),
			TemplateBody: new(identitySSOTemplate(instance, storeID, *user.UserId, true, failure)),
		})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(ctx, t, cf, "identitysso-stack", cfntypes.StackStatusUpdateRollbackComplete)
	}
	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("identitysso-stack")}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)
	list, err := sso.ListPermissionSets(ctx, &ssosdk.ListPermissionSetsInput{InstanceArn: new(instance)})
	if err != nil || len(list.PermissionSets) != 0 {
		t.Fatal(list, err)
	}
	groups, err := ids.ListGroups(ctx, &idsdk.ListGroupsInput{IdentityStoreId: new(storeID)})
	if err != nil || len(groups.Groups) != 0 {
		t.Fatal(groups, err)
	}
}

package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	idsdk "github.com/aws/aws-sdk-go-v2/service/identitystore"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/ssoadmin"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin/types"
)

func ssoAdminClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestSSOAdminSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ssoAdminClient(s)
	instances, err := c.ListInstances(ctx, &sdk.ListInstancesInput{})
	if err != nil || len(instances.Instances) != 1 {
		t.Fatal(instances, err)
	}
	instance := instances.Instances[0].InstanceArn
	storeID := instances.Instances[0].IdentityStoreId
	describedInstance, err := c.DescribeInstance(ctx, &sdk.DescribeInstanceInput{InstanceArn: instance})
	if err != nil || describedInstance.Status != types.InstanceStatusActive ||
		*describedInstance.IdentityStoreId != *storeID {
		t.Fatal(describedInstance, err)
	}
	permission, err := c.CreatePermissionSet(ctx, &sdk.CreatePermissionSetInput{
		InstanceArn: instance, Name: new("Developers"),
		Description: new("Initial"), SessionDuration: new("PT2H"), RelayState: new("https://example.com"),
		Tags: []types.Tag{{Key: new("env"), Value: new("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	arn := permission.PermissionSet.PermissionSetArn
	_, err = c.CreatePermissionSet(ctx, &sdk.CreatePermissionSetInput{InstanceArn: instance, Name: new("Developers")})
	if _, ok := errors.AsType[*types.ConflictException](err); !ok {
		t.Fatal(err)
	}
	if _, err = c.UpdatePermissionSet(ctx, &sdk.UpdatePermissionSetInput{
		InstanceArn: instance, PermissionSetArn: arn,
		Description: new("Updated"), SessionDuration: new("PT4H"),
	}); err != nil {
		t.Fatal(err)
	}
	described, err := c.DescribePermissionSet(
		ctx,
		&sdk.DescribePermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
	)
	if err != nil || *described.PermissionSet.Description != "Updated" ||
		*described.PermissionSet.SessionDuration != "PT4H" {
		t.Fatal(described, err)
	}
	list, err := c.ListPermissionSets(ctx, &sdk.ListPermissionSetsInput{InstanceArn: instance})
	if err != nil || len(list.PermissionSets) != 1 {
		t.Fatal(list, err)
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"*"}]}`
	if _, err = c.PutInlinePolicyToPermissionSet(
		ctx,
		&sdk.PutInlinePolicyToPermissionSetInput{
			InstanceArn:      instance,
			PermissionSetArn: arn,
			InlinePolicy:     new(policy),
		},
	); err != nil {
		t.Fatal(err)
	}
	inline, err := c.GetInlinePolicyForPermissionSet(
		ctx,
		&sdk.GetInlinePolicyForPermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
	)
	if err != nil || *inline.InlinePolicy != policy {
		t.Fatal(inline, err)
	}
	if _, err = c.PutInlinePolicyToPermissionSet(
		ctx,
		&sdk.PutInlinePolicyToPermissionSetInput{
			InstanceArn:      instance,
			PermissionSetArn: arn,
			InlinePolicy:     new("invalid"),
		},
	); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err = c.DeleteInlinePolicyFromPermissionSet(
		ctx,
		&sdk.DeleteInlinePolicyFromPermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
	); err != nil {
		t.Fatal(err)
	}
	inline, err = c.GetInlinePolicyForPermissionSet(
		ctx,
		&sdk.GetInlinePolicyForPermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
	)
	if err != nil || *inline.InlinePolicy != "" {
		t.Fatal(inline, err)
	}
	managed := new("arn:aws:iam::aws:policy/ReadOnlyAccess")
	if _, err = c.AttachManagedPolicyToPermissionSet(
		ctx,
		&sdk.AttachManagedPolicyToPermissionSetInput{
			InstanceArn:      instance,
			PermissionSetArn: arn,
			ManagedPolicyArn: managed,
		},
	); err != nil {
		t.Fatal(err)
	}
	policies, err := c.ListManagedPoliciesInPermissionSet(
		ctx,
		&sdk.ListManagedPoliciesInPermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
	)
	if err != nil || len(policies.AttachedManagedPolicies) != 1 ||
		*policies.AttachedManagedPolicies[0].Name != "ReadOnlyAccess" {
		t.Fatal(policies, err)
	}
	if _, err = c.DetachManagedPolicyFromPermissionSet(
		ctx,
		&sdk.DetachManagedPolicyFromPermissionSetInput{
			InstanceArn:      instance,
			PermissionSetArn: arn,
			ManagedPolicyArn: managed,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			InstanceArn: instance,
			ResourceArn: arn,
			Tags:        []types.Tag{{Key: new("owner"), Value: new("platform")}},
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{InstanceArn: instance, ResourceArn: arn, TagKeys: []string{"env"}},
	); err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{InstanceArn: instance, ResourceArn: arn})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatal(tags, err)
	}
	tagged, err := taggingClient(s, "us-east-1").TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{*arn}, Tags: map[string]string{"rgta": "yes"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}
	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{InstanceArn: instance, ResourceArn: arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}
	user, err := identityStoreClient(
		s,
	).CreateUser(ctx, &idsdk.CreateUserInput{IdentityStoreId: storeID, UserName: new("alice")})
	if err != nil {
		t.Fatal(err)
	}
	group, err := identityStoreClient(
		s,
	).CreateGroup(ctx, &idsdk.CreateGroupInput{IdentityStoreId: storeID, DisplayName: new("Developers")})
	if err != nil {
		t.Fatal(err)
	}
	for _, principal := range []struct {
		id   *string
		kind types.PrincipalType
	}{{user.UserId, types.PrincipalTypeUser}, {group.GroupId, types.PrincipalTypeGroup}} {
		created, createErr := c.CreateAccountAssignment(ctx, &sdk.CreateAccountAssignmentInput{
			InstanceArn:      instance,
			PermissionSetArn: arn,
			TargetId:         new("123456789012"),
			TargetType:       types.TargetTypeAwsAccount,
			PrincipalType:    principal.kind,
			PrincipalId:      principal.id,
		})
		if createErr != nil || created.AccountAssignmentCreationStatus.Status != types.StatusValuesSucceeded {
			t.Fatal(created, createErr)
		}
		status, statusErr := c.DescribeAccountAssignmentCreationStatus(
			ctx,
			&sdk.DescribeAccountAssignmentCreationStatusInput{
				InstanceArn:                        instance,
				AccountAssignmentCreationRequestId: created.AccountAssignmentCreationStatus.RequestId,
			},
		)
		if statusErr != nil || status.AccountAssignmentCreationStatus.Status != types.StatusValuesSucceeded {
			t.Fatal(status, statusErr)
		}
		assignments, listErr := c.ListAccountAssignments(ctx, &sdk.ListAccountAssignmentsInput{
			InstanceArn: instance, PermissionSetArn: arn, AccountId: new("123456789012"),
		})
		if listErr != nil || len(assignments.AccountAssignments) != 1 {
			t.Fatal(assignments, listErr)
		}
		accounts, listErr := c.ListAccountsForProvisionedPermissionSet(
			ctx,
			&sdk.ListAccountsForProvisionedPermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
		)
		if listErr != nil || len(accounts.AccountIds) != 1 {
			t.Fatal(accounts, listErr)
		}
		if _, deleteErr := c.DeletePermissionSet(
			ctx,
			&sdk.DeletePermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
		); deleteErr == nil {
			t.Fatal("in-use deletion accepted")
		}
		deleted, deleteErr := c.DeleteAccountAssignment(ctx, &sdk.DeleteAccountAssignmentInput{
			InstanceArn:      instance,
			PermissionSetArn: arn,
			TargetId:         new("123456789012"),
			TargetType:       types.TargetTypeAwsAccount,
			PrincipalType:    principal.kind,
			PrincipalId:      principal.id,
		})
		if deleteErr != nil || deleted.AccountAssignmentDeletionStatus.Status != types.StatusValuesSucceeded {
			t.Fatal(deleted, deleteErr)
		}
		deletion, statusErr := c.DescribeAccountAssignmentDeletionStatus(
			ctx,
			&sdk.DescribeAccountAssignmentDeletionStatusInput{
				InstanceArn:                        instance,
				AccountAssignmentDeletionRequestId: deleted.AccountAssignmentDeletionStatus.RequestId,
			},
		)
		if statusErr != nil || deletion.AccountAssignmentDeletionStatus.Status != types.StatusValuesSucceeded {
			t.Fatal(deletion, statusErr)
		}
	}
	_, err = c.CreateAccountAssignment(ctx, &sdk.CreateAccountAssignmentInput{
		InstanceArn: instance, PermissionSetArn: arn,
		TargetId: new("123456789012"), TargetType: types.TargetTypeAwsAccount, PrincipalType: types.PrincipalTypeUser,
		PrincipalId: new("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
	})
	if _, ok := errors.AsType[*types.ResourceNotFoundException](err); !ok {
		t.Fatal("missing principal must fail", err)
	}
	if _, err = c.DeletePermissionSet(
		ctx,
		&sdk.DeletePermissionSetInput{InstanceArn: instance, PermissionSetArn: arn},
	); err != nil {
		t.Fatal(err)
	}
}

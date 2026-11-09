package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam"
)

func TestIAMServiceLinkedRoles(t *testing.T) {
	ctx := context.Background()
	c := iamClient(server(t), "test", "us-east-1")
	in := &iam.CreateServiceLinkedRoleInput{AWSServiceName: new("autoscaling.amazonaws.com"), CustomSuffix: new("test")}
	created, err := c.CreateServiceLinkedRole(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	name := created.Role.RoleName
	if *name != "AWSServiceRoleForAutoScaling_test" ||
		*created.Role.Path != "/aws-service-role/autoscaling.amazonaws.com/" ||
		!strings.Contains(decoded(t, created.Role.AssumeRolePolicyDocument), "autoscaling.amazonaws.com") {
		t.Fatal(created.Role)
	}

	_, err = c.CreateServiceLinkedRole(ctx, in)
	wantCode(t, err, "InvalidInput")
	_, err = c.GetRole(ctx, &iam.GetRoleInput{RoleName: name})
	if err != nil {
		t.Fatal(err)
	}

	roles, err := c.ListRoles(ctx, &iam.ListRolesInput{})
	if err != nil || len(roles.Roles) != 1 {
		t.Fatal(roles, err)
	}

	_, err = c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: name})
	wantCode(t, err, "UnmodifiableEntity")
	_, err = c.UpdateRole(ctx, &iam.UpdateRoleInput{RoleName: name, MaxSessionDuration: new(int32(7200))})
	wantCode(t, err, "UnmodifiableEntity")
	_, err = c.UpdateAssumeRolePolicy(
		ctx,
		&iam.UpdateAssumeRolePolicyInput{RoleName: name, PolicyDocument: new(trustPolicy)},
	)
	wantCode(t, err, "UnmodifiableEntity")
	_, err = c.UpdateRoleDescription(ctx, &iam.UpdateRoleDescriptionInput{RoleName: name, Description: new("updated")})
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteServiceLinkedRole(ctx, &iam.DeleteServiceLinkedRoleInput{RoleName: name})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(*deleted.DeletionTaskId, "task/aws-service-role/autoscaling.amazonaws.com/"+*name+"/") {
		t.Fatal(deleted)
	}

	status, err := c.GetServiceLinkedRoleDeletionStatus(
		ctx,
		&iam.GetServiceLinkedRoleDeletionStatusInput{DeletionTaskId: deleted.DeletionTaskId},
	)
	if err != nil || status.Status != "SUCCEEDED" {
		t.Fatal(status, err)
	}

	_, err = c.GetServiceLinkedRoleDeletionStatus(
		ctx,
		&iam.GetServiceLinkedRoleDeletionStatusInput{DeletionTaskId: new("unknown")},
	)
	wantCode(t, err, "NoSuchEntity")
	_, err = c.GetRole(ctx, &iam.GetRoleInput{RoleName: name})
	wantCode(t, err, "NoSuchEntity")
}

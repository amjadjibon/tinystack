package ssoadmin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/services/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func ssoContext(account string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: "us-east-1"}},
	)
}

func TestPermissionRecords(t *testing.T) {
	ctx := ssoContext("123456789012")
	s := ssoadmin.New(memory.New(nil, nil))
	arn, _ := identitystore.Instance("123456789012")
	created, err := s.CreatePermissionSet(ctx, &api.CreatePermissionSetRequest{
		InstanceArn: new(arn), Name: new("Developers"),
		Tags: []api.Tag{{Key: new("env"), Value: new("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	permission := created.PermissionSet.PermissionSetArn
	_, err = s.CreatePermissionSet(ctx, &api.CreatePermissionSetRequest{InstanceArn: new(arn), Name: new("Developers")})
	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "ConflictException" {
		t.Fatal(err)
	}

	if _, err = s.UpdatePermissionSet(ctx, &api.UpdatePermissionSetRequest{
		InstanceArn: new(arn), PermissionSetArn: permission,
		Description: new("changed"), SessionDuration: new("PT13H"),
	}); err == nil {
		t.Fatal("invalid update accepted")
	}

	described, err := s.DescribePermissionSet(
		ctx,
		&api.DescribePermissionSetRequest{InstanceArn: new(arn), PermissionSetArn: permission},
	)
	if err != nil || described.PermissionSet.Description != nil || *described.PermissionSet.SessionDuration != "PT1H" {
		t.Fatal(described, err)
	}

	if _, err = s.UpdatePermissionSet(ctx, &api.UpdatePermissionSetRequest{
		InstanceArn: new(arn), PermissionSetArn: permission,
		Description: new("updated"), SessionDuration: new("PT90M"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = s.TagResource(ctx, &api.TagResourceRequest{
		InstanceArn: new(arn), ResourceArn: permission,
		Tags: []api.Tag{{Key: new("team"), Value: new("platform")}},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := s.ListTagsForResource(
		ctx,
		&api.ListTagsForResourceRequest{InstanceArn: new(arn), ResourceArn: permission},
	)
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	if _, err = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{InstanceArn: new(arn), ResourceArn: permission, TagKeys: []string{"env"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err = s.ListTagsForResource(
		ctx,
		&api.ListTagsForResourceRequest{InstanceArn: new(arn), ResourceArn: permission},
	)
	if err != nil || len(tags.Tags) != 1 {
		t.Fatal(tags, err)
	}

	if _, err = s.DescribePermissionSet(ssoContext("999999999999"), &api.DescribePermissionSetRequest{
		InstanceArn: new(arn), PermissionSetArn: permission,
	}); err == nil {
		t.Fatal("cross-account instance accepted")
	}

	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 1 || resources[0].Details.(map[string]any)["RecordsOnly"] != true {
		t.Fatal(resources, err)
	}

	if _, err = s.DeletePermissionSet(
		ctx,
		&api.DeletePermissionSetRequest{InstanceArn: new(arn), PermissionSetArn: permission},
	); err != nil {
		t.Fatal(err)
	}

	_, err = s.DescribePermissionSet(
		ctx,
		&api.DescribePermissionSetRequest{InstanceArn: new(arn), PermissionSetArn: permission},
	)
	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "ResourceNotFoundException" {
		t.Fatal(err)
	}
}

func TestPermissionPagesAfterDeletedCursor(t *testing.T) {
	ctx := ssoContext("123456789012")
	s := ssoadmin.New(memory.New(nil, nil))
	arn, _ := identitystore.Instance("123456789012")
	for _, name := range []string{"One", "Two", "Three"} {
		if _, err := s.CreatePermissionSet(
			ctx,
			&api.CreatePermissionSetRequest{InstanceArn: new(arn), Name: new(name)},
		); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.ListPermissionSets(
		ctx,
		&api.ListPermissionSetsRequest{InstanceArn: new(arn), MaxResults: new(int64(1))},
	)
	if err != nil || first.NextToken == nil {
		t.Fatal(first, err)
	}

	if _, err = s.DeletePermissionSet(
		ctx,
		&api.DeletePermissionSetRequest{InstanceArn: new(arn), PermissionSetArn: new(first.PermissionSets[0])},
	); err != nil {
		t.Fatal(err)
	}

	rest, err := s.ListPermissionSets(
		ctx,
		&api.ListPermissionSetsRequest{InstanceArn: new(arn), NextToken: first.NextToken},
	)
	if err != nil || len(rest.PermissionSets) != 2 {
		t.Fatal(rest, err)
	}
}

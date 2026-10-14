package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	glue "github.com/aws/aws-sdk-go-v2/service/glue"
	glueTypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/lakeformation"
	"github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/aws/smithy-go"
)

func lakeClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func lakeError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestLakeFormationSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := lakeClient(s, "test", "us-east-1")
	settings, err := c.GetDataLakeSettings(ctx, &sdk.GetDataLakeSettingsInput{})
	if err != nil || len(settings.DataLakeSettings.CreateDatabaseDefaultPermissions) != 1 {
		t.Fatal(settings, err)
	}
	admin := types.DataLakePrincipal{DataLakePrincipalIdentifier: new("arn:aws:iam::000000000000:role/lake-admin")}
	_, err = c.PutDataLakeSettings(
		ctx,
		&sdk.PutDataLakeSettingsInput{
			DataLakeSettings: &types.DataLakeSettings{
				DataLakeAdmins: []types.DataLakePrincipal{admin},
				Parameters:     map[string]string{"CROSS_ACCOUNT_VERSION": "4"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	settings, err = c.GetDataLakeSettings(ctx, &sdk.GetDataLakeSettingsInput{})
	if err != nil || len(settings.DataLakeSettings.DataLakeAdmins) != 1 ||
		len(settings.DataLakeSettings.CreateDatabaseDefaultPermissions) != 0 {
		t.Fatal(settings, err)
	}
	arn := new("arn:aws:s3:::lake-data")
	_, err = c.RegisterResource(
		ctx,
		&sdk.RegisterResourceInput{ResourceArn: arn, UseServiceLinkedRole: new(true), HybridAccessEnabled: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RegisterResource(ctx, &sdk.RegisterResourceInput{ResourceArn: arn, UseServiceLinkedRole: new(true)})
	lakeError(t, err, "AlreadyExistsException")
	described, err := c.DescribeResource(ctx, &sdk.DescribeResourceInput{ResourceArn: arn})
	if err != nil || !aws.ToBool(described.ResourceInfo.HybridAccessEnabled) ||
		aws.ToString(described.ResourceInfo.RoleArn) == "" {
		t.Fatal(described, err)
	}
	registered, err := c.ListResources(ctx, &sdk.ListResourcesInput{})
	if err != nil || len(registered.ResourceInfoList) != 1 {
		t.Fatal(registered, err)
	}
	resource := &types.Resource{DataLocation: &types.DataLocationResource{ResourceArn: arn}}
	_, err = c.GrantPermissions(
		ctx,
		&sdk.GrantPermissionsInput{
			Principal:   &admin,
			Resource:    resource,
			Permissions: []types.Permission{types.PermissionDataLocationAccess},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListPermissions(ctx, &sdk.ListPermissionsInput{Principal: &admin, Resource: resource})
	if err != nil || len(listed.PrincipalResourcePermissions) != 1 {
		t.Fatal(listed, err)
	}
	_, err = c.RevokePermissions(
		ctx,
		&sdk.RevokePermissionsInput{
			Principal:   &admin,
			Resource:    resource,
			Permissions: []types.Permission{types.PermissionDataLocationAccess},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Terraform repeats revocation until AWS reports that no grant remains.
	_, err = c.RevokePermissions(
		ctx,
		&sdk.RevokePermissionsInput{
			Principal:   &admin,
			Resource:    resource,
			Permissions: []types.Permission{types.PermissionDataLocationAccess},
		},
	)
	lakeError(t, err, "InvalidInputException")
	if !strings.Contains(err.Error(), "No permissions revoked. Grantee has no") {
		t.Fatal(err)
	}
	_, err = c.DeregisterResource(ctx, &sdk.DeregisterResourceInput{ResourceArn: arn})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeResource(ctx, &sdk.DescribeResourceInput{ResourceArn: arn})
	lakeError(t, err, "EntityNotFoundException")
}

func TestLakeFormationGluePermissionsAndTags(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := lakeClient(s, "test", "us-east-1")
	g := glue.New(
		glue.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	_, err := g.CreateDatabase(
		ctx,
		&glue.CreateDatabaseInput{DatabaseInput: &glueTypes.DatabaseInput{Name: new("lake_db")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.CreateTable(
		ctx,
		&glue.CreateTableInput{
			DatabaseName: new("lake_db"),
			TableInput: &glueTypes.TableInput{
				Name: new("events"),
				StorageDescriptor: &glueTypes.StorageDescriptor{
					Columns: []glueTypes.Column{{Name: new("id"), Type: new("string")}},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	db := &types.Resource{Database: &types.DatabaseResource{Name: new("lake_db")}}
	table := &types.Resource{Table: &types.TableResource{DatabaseName: new("lake_db"), Name: new("events")}}
	p := &types.DataLakePrincipal{DataLakePrincipalIdentifier: new("arn:aws:iam::000000000000:role/reader")}
	grant := &sdk.GrantPermissionsInput{
		Principal:                  p,
		Resource:                   table,
		Permissions:                []types.Permission{types.PermissionSelect},
		PermissionsWithGrantOption: []types.Permission{types.PermissionSelect},
	}
	_, err = c.GrantPermissions(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	// An explicit catalog and omitted catalog address the same grant.
	grant.Resource.Table.CatalogId = new("000000000000")
	_, err = c.GrantPermissions(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	perms, err := c.ListPermissions(
		ctx,
		&sdk.ListPermissionsInput{Resource: table, ResourceType: types.DataLakeResourceTypeTable},
	)
	if err != nil || len(perms.PrincipalResourcePermissions) != 1 ||
		len(perms.PrincipalResourcePermissions[0].PermissionsWithGrantOption) != 1 {
		t.Fatal(perms, err)
	}
	batch, err := c.BatchGrantPermissions(
		ctx,
		&sdk.BatchGrantPermissionsInput{Entries: []types.BatchPermissionsRequestEntry{
			{Id: new("good"), Principal: p, Resource: db, Permissions: []types.Permission{types.PermissionDescribe}},
			{
				Id:          new("bad"),
				Principal:   p,
				Resource:    &types.Resource{Database: &types.DatabaseResource{Name: new("absent")}},
				Permissions: []types.Permission{types.PermissionDescribe},
			},
		}},
	)
	if err != nil || len(batch.Failures) != 1 ||
		aws.ToString(batch.Failures[0].Error.ErrorCode) != "EntityNotFoundException" {
		t.Fatal(batch, err)
	}
	revoked, err := c.BatchRevokePermissions(
		ctx,
		&sdk.BatchRevokePermissionsInput{
			Entries: []types.BatchPermissionsRequestEntry{
				{
					Id:          new("good"),
					Principal:   p,
					Resource:    db,
					Permissions: []types.Permission{types.PermissionDescribe},
				},
			},
		},
	)
	if err != nil || len(revoked.Failures) != 0 {
		t.Fatal(revoked, err)
	}
	_, err = c.CreateLFTag(ctx, &sdk.CreateLFTagInput{TagKey: new("team"), TagValues: []string{"core", "platform"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdateLFTag(
		ctx,
		&sdk.UpdateLFTagInput{
			TagKey:            new("team"),
			TagValuesToAdd:    []string{"research"},
			TagValuesToDelete: []string{"platform"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := c.GetLFTag(ctx, &sdk.GetLFTagInput{TagKey: new("team")})
	if err != nil || len(tag.TagValues) != 2 {
		t.Fatal(tag, err)
	}
	tags, err := c.ListLFTags(ctx, &sdk.ListLFTagsInput{})
	if err != nil || len(tags.LFTags) != 1 {
		t.Fatal(tags, err)
	}
	pairs := []types.LFTagPair{{TagKey: new("team"), TagValues: []string{"core"}}}
	added, err := c.AddLFTagsToResource(ctx, &sdk.AddLFTagsToResourceInput{Resource: db, LFTags: pairs})
	if err != nil || len(added.Failures) != 0 {
		t.Fatal(added, err)
	}
	assigned, err := c.GetResourceLFTags(ctx, &sdk.GetResourceLFTagsInput{Resource: db, ShowAssignedLFTags: new(true)})
	if err != nil || len(assigned.LFTagOnDatabase) != 1 {
		t.Fatal(assigned, err)
	}
	inherited, err := c.GetResourceLFTags(ctx, &sdk.GetResourceLFTagsInput{Resource: table})
	if err != nil || len(inherited.LFTagsOnTable) != 1 {
		t.Fatal(inherited, err)
	}
	direct, err := c.GetResourceLFTags(ctx, &sdk.GetResourceLFTagsInput{Resource: table, ShowAssignedLFTags: new(true)})
	if err != nil || len(direct.LFTagsOnTable) != 0 {
		t.Fatal(direct, err)
	}
	columns := &types.Resource{
		TableWithColumns: &types.TableWithColumnsResource{
			DatabaseName: new("lake_db"),
			Name:         new("events"),
			ColumnNames:  []string{"id"},
		},
	}
	added, err = c.AddLFTagsToResource(ctx, &sdk.AddLFTagsToResourceInput{Resource: columns, LFTags: pairs})
	if err != nil || len(added.Failures) != 0 {
		t.Fatal(added, err)
	}
	assigned, err = c.GetResourceLFTags(
		ctx,
		&sdk.GetResourceLFTagsInput{Resource: table, ShowAssignedLFTags: new(true)},
	)
	if err != nil || len(assigned.LFTagsOnColumns) != 1 {
		t.Fatal(assigned, err)
	}
	removed, err := c.RemoveLFTagsFromResource(ctx, &sdk.RemoveLFTagsFromResourceInput{Resource: db, LFTags: pairs})
	if err != nil || len(removed.Failures) != 0 {
		t.Fatal(removed, err)
	}
	_, err = c.DeleteLFTag(ctx, &sdk.DeleteLFTagInput{TagKey: new("team")})
	if err != nil {
		t.Fatal(err)
	}
	assigned, err = c.GetResourceLFTags(
		ctx,
		&sdk.GetResourceLFTagsInput{Resource: table, ShowAssignedLFTags: new(true)},
	)
	if err != nil || len(assigned.LFTagsOnColumns) != 0 {
		t.Fatal(assigned, err)
	}
	_, err = c.GetLFTag(ctx, &sdk.GetLFTagInput{TagKey: new("team")})
	lakeError(t, err, "EntityNotFoundException")
}

func TestLakeFormationValidationPaginationAndIsolation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := lakeClient(s, "test", "us-east-1")
	for _, k := range []string{"alpha", "beta", "gamma"} {
		_, err := c.CreateLFTag(ctx, &sdk.CreateLFTagInput{TagKey: new(k), TagValues: []string{"yes"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.ListLFTags(ctx, &sdk.ListLFTagsInput{MaxResults: new(int32(1))})
	if err != nil || len(first.LFTags) != 1 || first.NextToken == nil {
		t.Fatal(first, err)
	}
	second, err := c.ListLFTags(ctx, &sdk.ListLFTagsInput{NextToken: first.NextToken, MaxResults: new(int32(1))})
	if err != nil || len(second.LFTags) != 1 ||
		aws.ToString(second.LFTags[0].TagKey) == aws.ToString(first.LFTags[0].TagKey) {
		t.Fatal(second, err)
	}
	_, err = c.ListLFTags(ctx, &sdk.ListLFTagsInput{NextToken: new("!")})
	lakeError(t, err, "InvalidInputException")
	for _, other := range []*sdk.Client{lakeClient(s, "team-b", "us-east-1"), lakeClient(s, "test", "us-west-2")} {
		tags, e := other.ListLFTags(ctx, &sdk.ListLFTagsInput{})
		if e != nil || len(tags.LFTags) != 0 {
			t.Fatal(tags, e)
		}
	}
	_, err = c.GetDataLakeSettings(ctx, &sdk.GetDataLakeSettingsInput{CatalogId: new("999999999999")})
	lakeError(t, err, "EntityNotFoundException")
	p := &types.DataLakePrincipal{DataLakePrincipalIdentifier: new("arn:aws:iam::000000000000:role/reader")}
	_, err = c.GrantPermissions(
		ctx,
		&sdk.GrantPermissionsInput{
			Principal:   p,
			Resource:    &types.Resource{Catalog: &types.CatalogResource{}},
			Permissions: []types.Permission{"BOGUS"},
		},
	)
	lakeError(t, err, "InvalidInputException")
	_, err = c.GrantPermissions(
		ctx,
		&sdk.GrantPermissionsInput{
			Principal:   &types.DataLakePrincipal{DataLakePrincipalIdentifier: new("bad")},
			Resource:    &types.Resource{Catalog: &types.CatalogResource{}},
			Permissions: []types.Permission{types.PermissionAll},
		},
	)
	lakeError(t, err, "InvalidInputException")
	_, err = c.GrantPermissions(
		ctx,
		&sdk.GrantPermissionsInput{
			Principal:   p,
			Resource:    &types.Resource{Database: &types.DatabaseResource{Name: new("missing")}},
			Permissions: []types.Permission{types.PermissionDescribe},
		},
	)
	lakeError(t, err, "EntityNotFoundException")
	_, err = c.GetTemporaryGlueTableCredentials(
		ctx,
		&sdk.GetTemporaryGlueTableCredentialsInput{TableArn: new("arn:aws:glue:us-east-1:000000000000:table/db/t")},
	)
	lakeError(t, err, "NotImplemented")
}

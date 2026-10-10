package integration_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/backup"
	"github.com/aws/aws-sdk-go-v2/service/backup/types"
)

func TestBackupPaginationFilters(t *testing.T) {
	s := server(t)
	c := backupClient(s, "test", "us-east-1")
	ctx := context.Background()
	_, p, _ := backupFixture(t, c)
	for _, name := range []string{"alpha", "omega"} {
		if _, err := c.CreateBackupVault(ctx, &sdk.CreateBackupVaultInput{BackupVaultName: new(name)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{MaxResults: new(int32(1))})
	if err != nil || len(first.BackupVaultList) != 1 ||
		aws.ToString(first.BackupVaultList[0].BackupVaultName) != "alpha" ||
		first.NextToken == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := c.ListBackupVaults(
		ctx,
		&sdk.ListBackupVaultsInput{MaxResults: new(int32(1)), NextToken: first.NextToken},
	)
	if err != nil || aws.ToString(second.BackupVaultList[0].BackupVaultName) != "omega" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	last, err := c.ListBackupVaults(
		ctx,
		&sdk.ListBackupVaultsInput{MaxResults: new(int32(1)), NextToken: second.NextToken},
	)
	if err != nil || aws.ToString(last.BackupVaultList[0].BackupVaultName) != "primary" || last.NextToken != nil {
		t.Fatalf("last page: %+v %v", last, err)
	}
	_, err = c.ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{NextToken: new("not-valid")})
	code(t, err, "InvalidParameterValueException")
	_, err = c.ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{NextToken: first.NextToken, ByShared: true})
	code(t, err, "InvalidParameterValueException")
	_, err = backupClient(
		s,
		"team-b",
		"us-east-1",
	).ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{NextToken: first.NextToken})
	code(t, err, "InvalidParameterValueException")
	_, err = backupClient(
		s,
		"test",
		"eu-west-1",
	).ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{NextToken: first.NextToken})
	code(t, err, "InvalidParameterValueException")
	shared, err := c.ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{ByShared: true})
	if err != nil || len(shared.BackupVaultList) != 0 {
		t.Fatalf("shared filter: %+v %v", shared, err)
	}
	airgap, err := c.ListBackupVaults(
		ctx,
		&sdk.ListBackupVaultsInput{ByVaultType: types.VaultTypeLogicallyAirGappedBackupVault},
	)
	if err != nil || len(airgap.BackupVaultList) != 0 {
		t.Fatalf("type filter: %+v %v", airgap, err)
	}
	_, err = c.DeleteBackupVault(ctx, &sdk.DeleteBackupVaultInput{BackupVaultName: new("alpha")})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.ListBackupVaults(
		ctx,
		&sdk.ListBackupVaultsInput{NextToken: first.NextToken, MaxResults: new(int32(1))},
	)
	if err != nil || aws.ToString(resumed.BackupVaultList[0].BackupVaultName) != "omega" {
		t.Fatalf("deleted cursor: %+v %v", resumed, err)
	}
	for i := range 2 {
		if _, err = c.CreateBackupPlan(
			ctx,
			&sdk.CreateBackupPlanInput{BackupPlan: backupPlanInput(fmt.Sprintf("plan-%d", i))},
		); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := c.ListBackupPlans(ctx, &sdk.ListBackupPlansInput{MaxResults: new(int32(1))})
	if err != nil || len(plans.BackupPlansList) != 1 || plans.NextToken == nil {
		t.Fatalf("plans page: %+v %v", plans, err)
	}
	_, err = c.ListBackupPlans(
		ctx,
		&sdk.ListBackupPlansInput{IncludeDeleted: aws.Bool(true), NextToken: plans.NextToken},
	)
	code(t, err, "InvalidParameterValueException")
	count := len(plans.BackupPlansList)
	for plans.NextToken != nil {
		plans, err = c.ListBackupPlans(
			ctx,
			&sdk.ListBackupPlansInput{MaxResults: new(int32(1)), NextToken: plans.NextToken},
		)
		if err != nil {
			t.Fatal(err)
		}
		count += len(plans.BackupPlansList)
	}
	if count != 3 {
		t.Fatalf("plan count %d", count)
	}
	for i := range 2 {
		in := backupPlanInput("daily-plan")
		in.Rules[0].Lifecycle.DeleteAfterDays = new(int64(200 + i))
		if _, err = c.UpdateBackupPlan(
			ctx,
			&sdk.UpdateBackupPlanInput{BackupPlanId: p.BackupPlanId, BackupPlan: in},
		); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := c.ListBackupPlanVersions(
		ctx,
		&sdk.ListBackupPlanVersionsInput{BackupPlanId: p.BackupPlanId, MaxResults: new(int32(1))},
	)
	if err != nil || versions.NextToken == nil {
		t.Fatalf("version page %+v %v", versions, err)
	}
	count = 1
	for versions.NextToken != nil {
		versions, err = c.ListBackupPlanVersions(
			ctx,
			&sdk.ListBackupPlanVersionsInput{
				BackupPlanId: p.BackupPlanId,
				MaxResults:   new(int32(1)),
				NextToken:    versions.NextToken,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		count += len(versions.BackupPlanVersionsList)
	}
	if count != 3 {
		t.Fatalf("version count %d", count)
	}
	for i := range 2 {
		if _, err = c.CreateBackupSelection(
			ctx,
			&sdk.CreateBackupSelectionInput{
				BackupPlanId:    p.BackupPlanId,
				BackupSelection: backupSelectionInput(fmt.Sprintf("selection-%d", i)),
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	selections, err := c.ListBackupSelections(
		ctx,
		&sdk.ListBackupSelectionsInput{BackupPlanId: p.BackupPlanId, MaxResults: new(int32(1))},
	)
	if err != nil || selections.NextToken == nil {
		t.Fatalf("selection page %+v %v", selections, err)
	}
	_, err = c.ListBackupPlans(ctx, &sdk.ListBackupPlansInput{NextToken: selections.NextToken})
	code(t, err, "InvalidParameterValueException")
	count = 1
	for selections.NextToken != nil {
		selections, err = c.ListBackupSelections(
			ctx,
			&sdk.ListBackupSelectionsInput{
				BackupPlanId: p.BackupPlanId,
				MaxResults:   new(int32(1)),
				NextToken:    selections.NextToken,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		count += len(selections.BackupSelectionsList)
	}
	if count != 3 {
		t.Fatalf("selection count %d", count)
	}
	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: p.BackupPlanArn, Tags: map[string]string{"a": "first", "z": "last"}},
	); err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: p.BackupPlanArn, MaxResults: new(int32(1))})
	if err != nil || tags.Tags["a"] != "first" || tags.NextToken == nil {
		t.Fatalf("tag page %+v %v", tags, err)
	}
	count = 1
	for tags.NextToken != nil {
		tags, err = c.ListTags(
			ctx,
			&sdk.ListTagsInput{ResourceArn: p.BackupPlanArn, MaxResults: new(int32(1)), NextToken: tags.NextToken},
		)
		if err != nil {
			t.Fatal(err)
		}
		count += len(tags.Tags)
	}
	if count != 3 {
		t.Fatalf("tag count %d", count)
	}
}

func TestBackupPersistenceAndReset(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	s, stop := persistent(t, path, &fakeRuntime{})
	c := backupClient(s, "test", "us-east-1")
	v, p, sel := backupFixture(t, c)
	in := backupPlanInput("persisted-v2")
	in.Rules[0].Lifecycle.DeleteAfterDays = new(int64(240))
	updated, err := c.UpdateBackupPlan(ctx, &sdk.UpdateBackupPlanInput{BackupPlanId: p.BackupPlanId, BackupPlan: in})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: p.BackupPlanArn, Tags: map[string]string{"persisted": "yes"}},
	); err != nil {
		t.Fatal(err)
	}
	beforeVault, err := c.DescribeBackupVault(ctx, &sdk.DescribeBackupVaultInput{BackupVaultName: v.BackupVaultName})
	if err != nil {
		t.Fatal(err)
	}
	beforeCurrent, err := c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: p.BackupPlanId})
	if err != nil {
		t.Fatal(err)
	}
	beforeOld, err := c.GetBackupPlan(
		ctx,
		&sdk.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: p.VersionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeSel, err := c.GetBackupSelection(
		ctx,
		&sdk.GetBackupSelectionInput{BackupPlanId: p.BackupPlanId, SelectionId: sel.SelectionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeTags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: p.BackupPlanArn})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	s, stop = persistent(t, path, &fakeRuntime{})
	defer stop()
	c = backupClient(s, "test", "us-east-1")
	afterVault, err := c.DescribeBackupVault(ctx, &sdk.DescribeBackupVaultInput{BackupVaultName: v.BackupVaultName})
	if err != nil {
		t.Fatal(err)
	}
	afterCurrent, err := c.GetBackupPlan(
		ctx,
		&sdk.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: updated.VersionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	afterOld, err := c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: p.VersionId})
	if err != nil {
		t.Fatal(err)
	}
	afterSel, err := c.GetBackupSelection(
		ctx,
		&sdk.GetBackupSelectionInput{BackupPlanId: p.BackupPlanId, SelectionId: sel.SelectionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	afterTags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: p.BackupPlanArn})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeVault.CreationDate, afterVault.CreationDate) ||
		!reflect.DeepEqual(beforeVault.EncryptionKeyArn, afterVault.EncryptionKeyArn) ||
		!reflect.DeepEqual(beforeCurrent.BackupPlan, afterCurrent.BackupPlan) ||
		!reflect.DeepEqual(beforeOld.BackupPlan, afterOld.BackupPlan) ||
		!reflect.DeepEqual(beforeSel.BackupSelection, afterSel.BackupSelection) ||
		!reflect.DeepEqual(beforeTags.Tags, afterTags.Tags) {
		t.Fatal("SQLite restart lost nondefault records")
	}
	retry, err := c.CreateBackupPlan(
		ctx,
		&sdk.CreateBackupPlanInput{BackupPlan: backupPlanInput("different"), CreatorRequestId: new("plan-token")},
	)
	if err != nil || aws.ToString(retry.BackupPlanId) != aws.ToString(p.BackupPlanId) ||
		aws.ToString(retry.VersionId) != aws.ToString(p.VersionId) {
		t.Fatalf("persisted token: %+v %v", retry, err)
	}
	retryVault, err := c.CreateBackupVault(
		ctx,
		&sdk.CreateBackupVaultInput{BackupVaultName: new("changed-name"), CreatorRequestId: new("vault-token")},
	)
	if err != nil || aws.ToString(retryVault.BackupVaultArn) != aws.ToString(v.BackupVaultArn) ||
		!reflect.DeepEqual(retryVault.CreationDate, v.CreationDate) {
		t.Fatalf("persisted vault token: %+v %v", retryVault, err)
	}
	retrySelection, err := c.CreateBackupSelection(
		ctx,
		&sdk.CreateBackupSelectionInput{
			BackupPlanId:     p.BackupPlanId,
			BackupSelection:  backupSelectionInput("changed-name"),
			CreatorRequestId: new("selection-token"),
		},
	)
	if err != nil || aws.ToString(retrySelection.SelectionId) != aws.ToString(sel.SelectionId) ||
		!reflect.DeepEqual(retrySelection.CreationDate, sel.CreationDate) {
		t.Fatalf("persisted selection token: %+v %v", retrySelection, err)
	}
	for _, foreign := range []*sdk.Client{backupClient(s, "team-b", "us-east-1"), backupClient(s, "test", "eu-west-1")} {
		_, err = foreign.GetBackupPlan(
			ctx,
			&sdk.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: p.VersionId},
		)
		code(t, err, "ResourceNotFoundException")
		_, err = foreign.GetBackupSelection(
			ctx,
			&sdk.GetBackupSelectionInput{BackupPlanId: p.BackupPlanId, SelectionId: sel.SelectionId},
		)
		code(t, err, "ResourceNotFoundException")
	}
	status, body := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatalf("reset %d %s", status, body)
	}
	_, err = c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: p.BackupPlanId, VersionId: p.VersionId})
	code(t, err, "ResourceNotFoundException")
	_, err = c.GetBackupSelection(
		ctx,
		&sdk.GetBackupSelectionInput{BackupPlanId: p.BackupPlanId, SelectionId: sel.SelectionId},
	)
	code(t, err, "ResourceNotFoundException")
	_, err = c.DescribeBackupVault(ctx, &sdk.DescribeBackupVaultInput{BackupVaultName: v.BackupVaultName})
	code(t, err, "ResourceNotFoundException")
}

func TestBackupSigV4Verify(t *testing.T) {
	s := verifyingServer(t)
	c := backupClient(s, "test", "us-east-1")
	_, p, _ := backupFixture(t, c)
	ctx := context.Background()
	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: p.BackupPlanArn})
	if err != nil || tags.Tags["owner"] != "plan" {
		t.Fatalf("signed ARN REST label: %+v %v", tags, err)
	}
	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: p.BackupPlanArn, Tags: map[string]string{"signed": "yes"}},
	); err != nil {
		t.Fatal(err)
	}
	_, err = backupClient(s, "wrong", "us-east-1").ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{})
	code(t, err, "InvalidSignatureException")
}

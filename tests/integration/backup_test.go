package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/backup"
	"github.com/aws/aws-sdk-go-v2/service/backup/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func backupClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func backupPlanInput(name string) *types.BackupPlanInput {
	return &types.BackupPlanInput{BackupPlanName: new(name), Rules: []types.BackupRuleInput{
		{
			RuleName:              new("daily"),
			TargetBackupVaultName: new("primary"),
			ScheduleExpression: new(
				"cron(0 5 ? * * *)",
			),
			StartWindowMinutes:      new(int64(60)),
			CompletionWindowMinutes: new(int64(240)),
			Lifecycle: &types.Lifecycle{
				MoveToColdStorageAfterDays: new(int64(30)),
				DeleteAfterDays:            new(int64(180)),
			},
			RecoveryPointTags: map[string]string{"owner": "platform"},
		},
	}}
}

func backupSelectionInput(name string) *types.BackupSelection {
	return &types.BackupSelection{
		SelectionName: new(
			name,
		),
		IamRoleArn: new("arn:aws:iam::000000000000:role/backup"),
		Resources:  []string{"arn:aws:s3:::data-bucket"},
		ListOfTags: []types.Condition{
			{ConditionType: types.ConditionTypeStringequals, ConditionKey: new("backup"), ConditionValue: new("daily")},
		},
	}
}

func backupFixture(
	t *testing.T,
	c *sdk.Client,
) (*sdk.CreateBackupVaultOutput, *sdk.CreateBackupPlanOutput, *sdk.CreateBackupSelectionOutput) {
	t.Helper()
	ctx := context.Background()
	vault, err := c.CreateBackupVault(ctx, &sdk.CreateBackupVaultInput{
		BackupVaultName:  new("primary"),
		CreatorRequestId: new("vault-token"),
		EncryptionKeyArn: new(
			"arn:aws:kms:us-east-1:000000000000:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		),
		BackupVaultTags: map[string]string{"owner": "vault"},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.CreateBackupPlan(
		ctx,
		&sdk.CreateBackupPlanInput{
			BackupPlan:       backupPlanInput("daily-plan"),
			CreatorRequestId: new("plan-token"),
			BackupPlanTags:   map[string]string{"owner": "plan"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := c.CreateBackupSelection(
		ctx,
		&sdk.CreateBackupSelectionInput{
			BackupPlanId:     plan.BackupPlanId,
			BackupSelection:  backupSelectionInput("daily-selection"),
			CreatorRequestId: new("selection-token"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return vault, plan, sel
}

func TestBackupSDKWorkflow(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := backupClient(s, "test", "us-east-1")
	vault, plan, sel := backupFixture(t, c)
	desc, err := c.DescribeBackupVault(ctx, &sdk.DescribeBackupVaultInput{BackupVaultName: vault.BackupVaultName})
	if err != nil || desc.NumberOfRecoveryPoints != 0 || aws.ToString(desc.EncryptionKeyArn) == "" {
		t.Fatalf("vault: %+v %v", desc, err)
	}
	retry, err := c.CreateBackupVault(
		ctx,
		&sdk.CreateBackupVaultInput{BackupVaultName: new("another-name"), CreatorRequestId: new("vault-token")},
	)
	if err != nil || aws.ToString(retry.BackupVaultArn) != aws.ToString(vault.BackupVaultArn) {
		t.Fatalf("vault token: %+v %v", retry, err)
	}
	_, err = c.CreateBackupVault(
		ctx,
		&sdk.CreateBackupVaultInput{BackupVaultName: new("primary"), CreatorRequestId: new("different")},
	)
	code(t, err, "AlreadyExistsException")
	_, err = c.CreateBackupPlan(
		ctx,
		&sdk.CreateBackupPlanInput{BackupPlan: backupPlanInput("daily-plan"), CreatorRequestId: new("different")},
	)
	code(t, err, "AlreadyExistsException")
	got, err := c.GetBackupSelection(
		ctx,
		&sdk.GetBackupSelectionInput{BackupPlanId: plan.BackupPlanId, SelectionId: sel.SelectionId},
	)
	if err != nil || !reflect.DeepEqual(got.BackupSelection, backupSelectionInput("daily-selection")) {
		t.Fatalf("selection: %+v %v", got, err)
	}
	old, err := c.GetBackupPlan(
		ctx,
		&sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId, VersionId: plan.VersionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	update := backupPlanInput("renamed-plan")
	update.Rules[0].Lifecycle.DeleteAfterDays = new(int64(300))
	update.Rules = append(
		update.Rules,
		types.BackupRuleInput{RuleName: new("weekly"), TargetBackupVaultName: new("primary")},
	)
	updated, err := c.UpdateBackupPlan(
		ctx,
		&sdk.UpdateBackupPlanInput{BackupPlanId: plan.BackupPlanId, BackupPlan: update},
	)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(updated.BackupPlanId) != aws.ToString(plan.BackupPlanId) ||
		aws.ToString(updated.VersionId) == aws.ToString(plan.VersionId) {
		t.Fatal("plan identity/version")
	}
	current, err := c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId})
	if err != nil || len(current.BackupPlan.Rules) != 2 ||
		aws.ToString(current.BackupPlan.Rules[1].ScheduleExpression) != "cron(0 5 ? * * *)" {
		t.Fatalf("current: %+v %v", current, err)
	}
	historical, err := c.GetBackupPlan(
		ctx,
		&sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId, VersionId: plan.VersionId},
	)
	if err != nil || !reflect.DeepEqual(historical.BackupPlan, old.BackupPlan) ||
		aws.ToString(current.BackupPlan.Rules[0].RuleId) != aws.ToString(old.BackupPlan.Rules[0].RuleId) {
		t.Fatal("historical version mutated")
	}
	retryPlan, err := c.CreateBackupPlan(
		ctx,
		&sdk.CreateBackupPlanInput{BackupPlan: backupPlanInput("new-name"), CreatorRequestId: new("plan-token")},
	)
	if err != nil || aws.ToString(retryPlan.VersionId) != aws.ToString(plan.VersionId) {
		t.Fatalf("original token response: %+v %v", retryPlan, err)
	}
	retrySel, err := c.CreateBackupSelection(
		ctx,
		&sdk.CreateBackupSelectionInput{
			BackupPlanId:     plan.BackupPlanId,
			BackupSelection:  backupSelectionInput("new-name"),
			CreatorRequestId: new("selection-token"),
		},
	)
	if err != nil || aws.ToString(retrySel.SelectionId) != aws.ToString(sel.SelectionId) {
		t.Fatalf("selection token: %+v %v", retrySel, err)
	}
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: plan.BackupPlanArn, Tags: map[string]string{"environment": "dev"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: plan.BackupPlanArn, TagKeyList: []string{"owner"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: plan.BackupPlanArn})
	if err != nil || !reflect.DeepEqual(tags.Tags, map[string]string{"environment": "dev"}) {
		t.Fatalf("tags: %+v %v", tags, err)
	}
	tagging := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	tagged, err := tagging.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(vault.BackupVaultArn), aws.ToString(plan.BackupPlanArn)},
			Tags:            map[string]string{"shared": "yes"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) > 0 {
		t.Fatalf("shared tags: %+v %v", tagged, err)
	}
	untagged, err := tagging.UntagResources(
		ctx,
		&resourcegroupstaggingapi.UntagResourcesInput{
			ResourceARNList: []string{aws.ToString(vault.BackupVaultArn)},
			TagKeys:         []string{"shared"},
		},
	)
	if err != nil || len(untagged.FailedResourcesMap) > 0 {
		t.Fatalf("shared untag: %+v %v", untagged, err)
	}
	for _, foreign := range []*sdk.Client{backupClient(s, "team-b", "us-east-1"), backupClient(s, "test", "eu-west-1")} {
		_, err = foreign.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId})
		code(t, err, "ResourceNotFoundException")
		_, err = foreign.GetBackupSelection(
			ctx,
			&sdk.GetBackupSelectionInput{BackupPlanId: plan.BackupPlanId, SelectionId: sel.SelectionId},
		)
		code(t, err, "ResourceNotFoundException")
		_, err = foreign.CreateBackupSelection(
			ctx,
			&sdk.CreateBackupSelectionInput{
				BackupPlanId:    plan.BackupPlanId,
				BackupSelection: backupSelectionInput("foreign"),
			},
		)
		code(t, err, "InvalidParameterValueException")
		_, err = foreign.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: plan.BackupPlanArn, Tags: map[string]string{"bad": "foreign"}},
		)
		code(t, err, "ResourceNotFoundException")
		_, err = foreign.DescribeBackupVault(ctx, &sdk.DescribeBackupVaultInput{BackupVaultName: vault.BackupVaultName})
		code(t, err, "ResourceNotFoundException")
	}
	_, err = c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId, VersionId: new("unknown")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.GetBackupSelection(
		ctx,
		&sdk.GetBackupSelectionInput{BackupPlanId: plan.BackupPlanId, SelectionId: new("unknown")},
	)
	code(t, err, "ResourceNotFoundException")
	_, err = c.DeleteBackupPlan(ctx, &sdk.DeleteBackupPlanInput{BackupPlanId: plan.BackupPlanId})
	code(t, err, "InvalidRequestException")
	_, err = c.DeleteBackupSelection(
		ctx,
		&sdk.DeleteBackupSelectionInput{BackupPlanId: plan.BackupPlanId, SelectionId: sel.SelectionId},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteBackupPlan(ctx, &sdk.DeleteBackupPlanInput{BackupPlanId: plan.BackupPlanId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId})
	code(t, err, "ResourceNotFoundException")
	_, err = c.GetBackupPlan(
		ctx,
		&sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId, VersionId: updated.VersionId},
	)
	code(t, err, "ResourceNotFoundException")
	historical, err = c.GetBackupPlan(
		ctx,
		&sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId, VersionId: plan.VersionId},
	)
	if err != nil || !reflect.DeepEqual(historical.BackupPlan, old.BackupPlan) {
		t.Fatal("previous version lost after delete")
	}
	active, err := c.ListBackupPlans(ctx, &sdk.ListBackupPlansInput{})
	if err != nil || len(active.BackupPlansList) != 0 {
		t.Fatalf("active plans: %+v %v", active, err)
	}
	all, err := c.ListBackupPlans(ctx, &sdk.ListBackupPlansInput{IncludeDeleted: aws.Bool(true)})
	if err != nil || len(all.BackupPlansList) != 1 || all.BackupPlansList[0].DeletionDate == nil {
		t.Fatalf("deleted filter: %+v %v", all, err)
	}
	versions, err := c.ListBackupPlanVersions(ctx, &sdk.ListBackupPlanVersionsInput{BackupPlanId: plan.BackupPlanId})
	if err != nil || len(versions.BackupPlanVersionsList) != 2 {
		t.Fatalf("deleted history: %+v %v", versions, err)
	}
	_, err = c.DeleteBackupVault(ctx, &sdk.DeleteBackupVaultInput{BackupVaultName: vault.BackupVaultName})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteBackupVault(ctx, &sdk.DeleteBackupVaultInput{BackupVaultName: vault.BackupVaultName})
	code(t, err, "ResourceNotFoundException")
}

func backupWire(t *testing.T, s *httptest.Server, method, path, body string) (status int, result map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/backup/aws4_request, SignedHeaders=host, Signature=dummy",
	)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	result = map[string]any{}
	if len(b) > 0 {
		if err = json.Unmarshal(b, &result); err != nil {
			t.Fatalf("wire %s: %v", b, err)
		}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("wrong REST JSON content type: %v", resp.Header)
	}
	return resp.StatusCode, result
}

func TestBackupRawWireValidationAndAtomicity(t *testing.T) {
	s := server(t)
	c := backupClient(s, "test", "us-east-1")
	_, plan, _ := backupFixture(t, c)
	ctx := context.Background()
	path := "/backup/plans/" + aws.ToString(plan.BackupPlanId)
	before, err := c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId})
	if err != nil {
		t.Fatal(err)
	}
	original := `{"BackupPlan":{"BackupPlanName":"daily-plan",` +
		`"Rules":[{"RuleName":"daily","TargetBackupVaultName":"primary"}]}}`
	cases := []struct{ method, path, body, want string }{
		{"PUT", "/backup-vaults/a", `{}`, "InvalidParameterValueException"},
		{
			"PUT",
			"/backup-vaults/invalid-key",
			`{"EncryptionKeyArn":"alias/aws/backup"}`,
			"InvalidParameterValueException",
		},
		{"PUT", "/backup-vaults/invalid-key", `{"CreatorRequestId":""}`, "InvalidParameterValueException"},
		{
			"PUT",
			"/backup-vaults/invalid-key",
			`{"BackupVaultTags":{"aws:reserved":"x"}}`,
			"InvalidParameterValueException",
		},
		{"PUT", "/backup/plans", `{}`, "MissingParameterValueException"},
		{"PUT", "/backup/plans", `{"BackupPlan":{}}`, "MissingParameterValueException"},
		{
			"PUT",
			"/backup/plans",
			`{"BackupPlan":{"BackupPlanName":"valid","Rules":[null]}}`,
			"MissingParameterValueException",
		},
		{"POST", path, strings.Replace(original, `"RuleName":"daily",`, "", 1), "MissingParameterValueException"},
		{
			"POST",
			path,
			strings.Replace(original, `"TargetBackupVaultName":"primary"`, `"TargetBackupVaultName":""`, 1),
			"InvalidParameterValueException",
		},
		{
			"POST",
			path,
			strings.Replace(
				original,
				`"TargetBackupVaultName":"primary"`,
				`"TargetBackupVaultName":"primary","StartWindowMinutes":0`,
				1,
			),
			"InvalidParameterValueException",
		},
		{
			"POST",
			path,
			strings.Replace(
				original,
				`"TargetBackupVaultName":"primary"`,
				`"TargetBackupVaultName":"primary","Lifecycle":{"MoveToColdStorageAfterDays":30,"DeleteAfterDays":119}`,
				1,
			),
			"InvalidParameterValueException",
		},
		{
			"POST",
			path,
			strings.Replace(original, `"Rules":`, `"AdvancedBackupSettings":[],"Rules":`, 1),
			"NotImplemented",
		},
		{
			"POST",
			path,
			strings.Replace(
				original,
				`"TargetBackupVaultName":"primary"`,
				`"TargetBackupVaultName":"primary","EnableContinuousBackup":false`,
				1,
			),
			"NotImplemented",
		},
		{
			"POST",
			path,
			strings.Replace(
				original,
				`"TargetBackupVaultName":"primary"`,
				`"TargetBackupVaultName":"primary","CopyActions":[]`,
				1,
			),
			"NotImplemented",
		},
		{
			"POST",
			path,
			strings.Replace(
				original,
				`"TargetBackupVaultName":"primary"`,
				`"TargetBackupVaultName":"primary","Lifecycle":{"OptInToArchiveForSupportedResources":false}`,
				1,
			),
			"NotImplemented",
		},
		{"GET", path + "?MaxScheduledRunsPreview=1", ``, "NotImplemented"},
		{"GET", "/backup-vaults/primary?backupVaultAccountId=000000000000", ``, "NotImplemented"},
		{"GET", "/backup-vaults?maxResults=0", ``, "InvalidParameterValueException"},
		{"GET", "/backup-vaults?maxResults=wrong", ``, "InvalidArgument"},
		{"GET", "/backup-vaults?shared=wrong", ``, "InvalidArgument"},
		{"GET", "/backup-vaults?vaultType=bogus", ``, "InvalidParameterValueException"},
		{
			"PUT",
			path + "/selections",
			`{"BackupSelection":{"SelectionName":"bad","IamRoleArn":"arn:aws:iam::000000000000:role/backup",` +
				`"Resources":["arn:aws:s3:::*" ]}}`,
			"InvalidParameterValueException",
		},
		{
			"PUT",
			path + "/selections",
			`{"BackupSelection":{"SelectionName":"bad","IamRoleArn":"arn:aws:iam::000000000000:role/backup",` +
				`"Resources":["arn:aws:s3:::data-bucket"],"Conditions":{}}}`,
			"NotImplemented",
		},
		{
			"POST",
			"/tags/" + url.PathEscape(aws.ToString(plan.BackupPlanArn)),
			`{"Tags":{"aws:bad":"value"}}`,
			"InvalidParameterValueException",
		},
	}
	for _, test := range cases {
		status, body := backupWire(t, s, test.method, test.path, test.body)
		wantStatus := 400
		if test.want == "NotImplemented" {
			wantStatus = 501
		}
		if status != wantStatus || body["__type"] != test.want {
			t.Errorf("%s %s %s: got %d %+v want %s", test.method, test.path, test.body, status, body, test.want)
		}
	}
	after, err := c.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: plan.BackupPlanId})
	if err != nil || !reflect.DeepEqual(before.BackupPlan, after.BackupPlan) ||
		aws.ToString(before.VersionId) != aws.ToString(after.VersionId) {
		t.Fatal("invalid requests changed plan")
	}
	history, err := c.ListBackupPlanVersions(ctx, &sdk.ListBackupPlanVersionsInput{BackupPlanId: plan.BackupPlanId})
	if err != nil || len(history.BackupPlanVersionsList) != 1 {
		t.Fatalf("invalid version published: %+v %v", history, err)
	}
	status, body := backupWire(t, s, "GET", path+"?versionId="+url.QueryEscape(aws.ToString(plan.VersionId)), "")
	if status != 200 || body["VersionId"] != aws.ToString(plan.VersionId) {
		t.Fatalf("REST JSON version query: %d %+v", status, body)
	}
}

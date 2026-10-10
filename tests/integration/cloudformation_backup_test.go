package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/backup"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

const backupTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Resources:
  Vault:
    Type: AWS::Backup::BackupVault
    Properties:
      BackupVaultName: primary
      BackupVaultTags: {Owner: first}
  Plan:
    Type: AWS::Backup::BackupPlan
    Properties:
      BackupPlanTags: {Owner: first}
      BackupPlan:
        BackupPlanName: stack-plan
        BackupPlanRule:
          - RuleName: daily
            TargetBackupVault: !Ref Vault
            ScheduleExpression: cron(0 5 ? * * *)
            StartWindowMinutes: 60
            CompletionWindowMinutes: 240
            Lifecycle: {MoveToColdStorageAfterDays: 30, DeleteAfterDays: 180}
            RecoveryPointTags: {Owner: first}
  Selection:
    Type: AWS::Backup::BackupSelection
    Properties:
      BackupPlanId: !Ref Plan
      BackupSelection:
        SelectionName: stack-selection
        IamRoleArn: arn:aws:iam::000000000000:role/backup
        Resources: ["arn:aws:s3:::data-bucket"]
        ListOfTags:
          - ConditionType: STRINGEQUALS
            ConditionKey: backup
            ConditionValue: daily
Outputs:
  VaultName: {Value: !Ref Vault}
  VaultArn: {Value: !GetAtt Vault.BackupVaultArn}
  PlanID: {Value: !Ref Plan}
  PlanArn: {Value: !GetAtt Plan.BackupPlanArn}
  VersionID: {Value: !GetAtt Plan.VersionId}
  SelectionID: {Value: !Ref Selection}
  SelectionPlanID: {Value: !GetAtt Selection.BackupPlanId}
`

func TestCloudFormationBackupRecords(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	b := backupClient(s, "test", "us-east-1")
	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("backup-records"), TemplateBody: new(backupTemplate)},
	)
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "backup-records", cfntypes.StackStatusCreateComplete)
	outputs := map[string]string{}
	for _, out := range stack.Outputs {
		outputs[aws.ToString(out.OutputKey)] = aws.ToString(out.OutputValue)
	}
	if outputs["VaultName"] != "primary" || !strings.Contains(outputs["VaultArn"], ":backup-vault:primary") ||
		outputs["PlanID"] == "" ||
		!strings.Contains(outputs["PlanArn"], outputs["PlanID"]) ||
		outputs["SelectionID"] == "" ||
		outputs["SelectionPlanID"] != outputs["PlanID"] {
		t.Fatalf("refs: %+v", outputs)
	}
	planID := outputs["PlanID"]
	versionID := outputs["VersionID"]
	selectionID := outputs["SelectionID"]
	updated := strings.ReplaceAll(backupTemplate, "first", "second")
	updated = strings.Replace(updated, "DeleteAfterDays: 180", "DeleteAfterDays: 240", 1)
	updated = strings.Replace(updated, "role/backup", "role/updated-backup", 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("backup-records"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}
	stack = wantStatus(ctx, t, c, "backup-records", cfntypes.StackStatusUpdateComplete)
	for _, out := range stack.Outputs {
		outputs[aws.ToString(out.OutputKey)] = aws.ToString(out.OutputValue)
	}
	if outputs["PlanID"] != planID || outputs["VersionID"] == versionID || outputs["SelectionID"] == selectionID {
		t.Fatalf("update identities: %+v", outputs)
	}
	selections, err := b.ListBackupSelections(ctx, &sdk.ListBackupSelectionsInput{BackupPlanId: new(planID)})
	if err != nil || len(selections.BackupSelectionsList) != 1 {
		t.Fatalf("selection cleanup: %+v %v", selections, err)
	}
	got, err := b.GetBackupSelection(
		ctx,
		&sdk.GetBackupSelectionInput{BackupPlanId: new(planID), SelectionId: new(outputs["SelectionID"])},
	)
	if err != nil || aws.ToString(got.BackupSelection.IamRoleArn) != "arn:aws:iam::000000000000:role/updated-backup" {
		t.Fatalf("selection replaced %+v %v", got, err)
	}
	tags, err := b.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: new(outputs["PlanArn"])})
	if err != nil || tags.Tags["Owner"] != "second" {
		t.Fatalf("plan tags: %+v %v", tags, err)
	}
	vaultTags, err := b.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: new(outputs["VaultArn"])})
	if err != nil || vaultTags.Tags["Owner"] != "second" {
		t.Fatalf("vault tags: %+v %v", vaultTags, err)
	}
	before, err := b.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: new(planID)})
	if err != nil {
		t.Fatal(err)
	}
	// A malformed nested field must reach the service and trigger rollback.
	bad := strings.Replace(updated, "DeleteAfterDays: 240", "DeleteAfterDays: 119", 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("backup-records"), TemplateBody: new(bad)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "backup-records", cfntypes.StackStatusUpdateRollbackComplete)
	after, err := b.GetBackupPlan(ctx, &sdk.GetBackupPlanInput{BackupPlanId: new(planID)})
	if err != nil ||
		*after.BackupPlan.Rules[0].Lifecycle.DeleteAfterDays != *before.BackupPlan.Rules[0].Lifecycle.DeleteAfterDays {
		t.Fatalf("rollback: %+v %v", after, err)
	}
	badCopy := "TargetBackupVault: !Ref Vault\n            CopyActions: []"
	badPolicy := "BackupVaultTags: {Owner: second}\n      AccessPolicy: {}"
	for _, bad := range []string{
		strings.Replace(updated, "TargetBackupVault: !Ref Vault", badCopy, 1),
		strings.Replace(updated, "BackupVaultTags: {Owner: second}", badPolicy, 1),
	} {
		_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("backup-records"), TemplateBody: new(bad)})
		if err == nil {
			wantStatus(ctx, t, c, "backup-records", cfntypes.StackStatusUpdateRollbackComplete)
		} else {
			code(t, err, "ValidationError")
		}
	}
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("backup-records")})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	plans, err := b.ListBackupPlans(ctx, &sdk.ListBackupPlansInput{})
	if err != nil || len(plans.BackupPlansList) != 0 {
		t.Fatalf("plans left: %+v %v", plans, err)
	}
	vaults, err := b.ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{})
	if err != nil || len(vaults.BackupVaultList) != 0 {
		t.Fatalf("vaults left: %+v %v", vaults, err)
	}
}

func TestCloudFormationBackupCreateRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	b := backupClient(s, "test", "us-east-1")
	bad := strings.Replace(backupTemplate, "DeleteAfterDays: 180", "DeleteAfterDays: 119", 1)
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("backup-rollback"), TemplateBody: new(bad)})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "backup-rollback", cfntypes.StackStatusRollbackComplete)
	vaults, err := b.ListBackupVaults(ctx, &sdk.ListBackupVaultsInput{})
	if err != nil || len(vaults.BackupVaultList) != 0 {
		t.Fatalf("rollback leaked vault: %+v %v", vaults, err)
	}
	plans, err := b.ListBackupPlans(ctx, &sdk.ListBackupPlansInput{IncludeDeleted: new(true)})
	if err != nil || len(plans.BackupPlansList) != 0 {
		t.Fatalf("invalid plan published: %+v %v", plans, err)
	}
}

func TestCloudFormationBackupReplaceTagsAtLimit(t *testing.T) {
	for _, member := range []string{"BackupVaultTags", "BackupPlanTags"} {
		t.Run(member, func(t *testing.T) {
			ctx := context.Background()
			s := server(t)
			c := cfnClient(s)
			b := backupClient(s, "test", "us-east-1")
			tags := map[string]string{}
			for i := range 50 {
				tags[fmt.Sprintf("key%02d", i)] = "value"
			}
			encoded, err := json.Marshal(tags)
			if err != nil {
				t.Fatal(err)
			}
			template := strings.Replace(backupTemplate, member+": {Owner: first}", member+": "+string(encoded), 1)
			_, err = c.CreateStack(
				ctx,
				&cfn.CreateStackInput{StackName: new("backup-limit"), TemplateBody: new(template)},
			)
			if err != nil {
				t.Fatal(err)
			}
			stack := wantStatus(ctx, t, c, "backup-limit", cfntypes.StackStatusCreateComplete)
			outputs := outputsOf(&stack)
			updated := strings.Replace(template, `"key49":"value"`, `"replacement":"value"`, 1)
			_, err = c.UpdateStack(
				ctx,
				&cfn.UpdateStackInput{StackName: new("backup-limit"), TemplateBody: new(updated)},
			)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus(ctx, t, c, "backup-limit", cfntypes.StackStatusUpdateComplete)
			arn := outputs["VaultArn"]
			if member == "BackupPlanTags" {
				arn = outputs["PlanArn"]
			}
			got, err := b.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: new(arn)})
			if err != nil || len(got.Tags) != 50 || got.Tags["replacement"] != "value" {
				t.Fatalf("replacement tags: %+v %v", got, err)
			}
			if _, found := got.Tags["key49"]; found {
				t.Fatal("removed key remains after update")
			}
		})
	}
}

func TestCloudFormationBackupTagsAtLimitRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	b := backupClient(s, "test", "us-east-1")
	tags := map[string]string{}
	for i := range 50 {
		tags[fmt.Sprintf("key%02d", i)] = fmt.Sprintf("original%02d", i)
	}
	encoded, err := json.Marshal(tags)
	if err != nil {
		t.Fatal(err)
	}
	template := backupTemplate
	for _, member := range []string{"BackupVaultTags", "BackupPlanTags"} {
		template = strings.Replace(template, member+": {Owner: first}", member+": "+string(encoded), 1)
	}
	_, err = c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("backup-tag-rollback"), TemplateBody: &template})
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "backup-tag-rollback", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	updatedTags := maps.Clone(tags)
	delete(updatedTags, "key49")
	updatedTags["replacement"] = "new-value"
	for i := range 49 {
		updatedTags[fmt.Sprintf("key%02d", i)] = fmt.Sprintf("updated%02d", i)
	}
	updatedEncoded, err := json.Marshal(updatedTags)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.ReplaceAll(template, string(encoded), string(updatedEncoded))
	// Selection depends on Plan, which depends on Vault. Fail selection creation
	// only after both tag updates have completed successfully.
	updated = strings.Replace(updated, "role/backup", "user/invalid-role", 1)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("backup-tag-rollback"), TemplateBody: &updated})
	if err != nil {
		t.Fatal(err)
	}
	stack = wantStatus(ctx, t, c, "backup-tag-rollback", cfntypes.StackStatusUpdateRollbackComplete)
	rolledBackOutputs := outputsOf(&stack)
	events, err := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new("backup-tag-rollback")})
	if err != nil {
		t.Fatal(err)
	}
	// Events are newest first: find the failure, then the successful updates that
	// preceded it. Rollback updates alone must not satisfy this assertion.
	failed, completed := false, map[string]bool{}
	for _, event := range events.StackEvents {
		if aws.ToString(event.LogicalResourceId) == "Selection" &&
			event.ResourceStatus == cfntypes.ResourceStatusUpdateFailed &&
			strings.Contains(aws.ToString(event.ResourceStatusReason), "InvalidParameterValueException") {
			failed = true
		}
		if failed && event.ResourceStatus == cfntypes.ResourceStatusUpdateComplete {
			completed[aws.ToString(event.LogicalResourceId)] = true
		}
	}
	if !failed || !completed["Vault"] || !completed["Plan"] {
		t.Fatalf("expected both successful tag updates before selection failure: %+v", events.StackEvents)
	}
	for _, output := range []string{"VaultArn", "PlanArn"} {
		got, listErr := b.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: new(outputs[output])})
		if listErr != nil || !reflect.DeepEqual(got.Tags, tags) {
			t.Fatalf("%s rollback tags: %+v, want %v, err %v", output, got, tags, listErr)
		}
	}
	selection, err := b.GetBackupSelection(ctx, &sdk.GetBackupSelectionInput{
		BackupPlanId: new(outputs["PlanID"]), SelectionId: new(rolledBackOutputs["SelectionID"]),
	})
	if err != nil || aws.ToString(selection.BackupSelection.IamRoleArn) != "arn:aws:iam::000000000000:role/backup" {
		t.Fatalf("original selection after rollback: %+v %v", selection, err)
	}
}

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestCodePipelineS3SourceChanges(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "poll-source")
	p.ExecutionMode = types.ExecutionModeQueued
	first := &p.Stages[0].Actions[0]
	first.Configuration["PollForSourceChanges"] = "true"
	second := *first
	second.Name = new("SecondFetch")
	second.OutputArtifacts = []types.OutputArtifact{{Name: new("second-src")}}
	p.Stages[0].Actions = append(p.Stages[0].Actions, second)

	buckets := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"),
		UsePathStyle: true,
	})
	bucket := new("poll-source-src")
	key := new("source.zip")
	if _, err := buckets.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  bucket,
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusApproved)
	data := pipelineZip(t, map[string]string{"test": "revised source"})
	if _, err := buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: new("unrelated.zip"), Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}

	written, err := buckets.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: key, CopySource: new(*bucket + "/unrelated.zip"),
	})
	if err != nil {
		t.Fatal(err)
	}

	rows := pipelineExecutions(t, c, *p.Name)
	if len(rows) != 2 {
		t.Fatalf("want one creation and one source-change execution, got %v", rows)
	}

	var triggered *string
	for _, row := range rows {
		if row.Trigger.TriggerType == types.TriggerTypePollForSourceChanges {
			triggered = row.PipelineExecutionId
		}
	}

	if triggered == nil {
		t.Fatalf("no S3 source change execution in %v", rows)
	}

	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusApproved)
	pipelineWait(t, c, *p.Name, *triggered, "Succeeded")
	out, err := c.GetPipelineExecution(ctx, &sdk.GetPipelineExecutionInput{
		PipelineName: p.Name, PipelineExecutionId: triggered,
	})
	if err != nil || len(out.PipelineExecution.ArtifactRevisions) != 2 {
		t.Fatal(out, err)
	}

	for _, revision := range out.PipelineExecution.ArtifactRevisions {
		if aws.ToString(revision.RevisionId) != aws.ToString(written.VersionId) {
			t.Fatalf("source revision = %v, want %q", revision, aws.ToString(written.VersionId))
		}
	}

	if _, err = buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}

	if rows = pipelineExecutions(t, c, *p.Name); len(rows) != 3 {
		t.Fatalf("matching PutObject should start one execution: %v", rows)
	}

	for _, action := range p.Stages[0].Actions {
		action.Configuration["PollForSourceChanges"] = "false"
	}

	if _, err = c.UpdatePipeline(ctx, &sdk.UpdatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	if _, err = buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}

	if rows = pipelineExecutions(t, c, *p.Name); len(rows) != 3 {
		t.Fatalf("disabled polling started another execution: %v", rows)
	}
}

func TestCodePipelineStageRollback(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "roll-back")
	p.Stages[0].Actions[0].Namespace = new("SourceVariables")
	p.Stages[0].Actions[0].Configuration["PollForSourceChanges"] = "false"
	p.Variables = []types.PipelineVariableDeclaration{{Name: new("message"), DefaultValue: new("known-good")}}
	deploy := codePipelineAction("Deploy", types.ActionCategoryDeploy, "CloudFormation", "src", "")
	deploy.RunOrder = new(int32(2))
	deploy.Configuration = map[string]string{
		"ActionMode":   "CREATE_UPDATE",
		"StackName":    "#{SourceVariables.BucketName}-#{variables.message}",
		"TemplatePath": "src::template.json",
	}
	p.Stages[1].Actions = append(p.Stages[1].Actions, deploy)
	p.Stages[1].OnFailure = &types.FailureConditions{Result: types.ResultRollback}
	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	prior := pipelineExecutions(t, c, *p.Name)[0].PipelineExecutionId
	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusApproved)
	pipelineWait(t, c, *p.Name, *prior, "Succeeded")

	buckets := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"),
		UsePathStyle: true,
	})
	if _, err := buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("roll-back-src"), Key: new("source.zip"), Body: bytes.NewReader([]byte("not a zip")),
	}); err != nil {
		t.Fatal(err)
	}

	manual, err := c.RollbackStage(ctx, &sdk.RollbackStageInput{
		PipelineName: p.Name, StageName: new("Approve"), TargetPipelineExecutionId: prior,
	})
	if err != nil {
		t.Fatal(err)
	}

	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusApproved)
	pipelineWait(t, c, *p.Name, *manual.PipelineExecutionId, "Succeeded")
	checkRollback(t, c, p.Name, manual.PipelineExecutionId, prior, types.TriggerTypeManualRollback)

	failed, err := c.StartPipelineExecution(ctx, &sdk.StartPipelineExecutionInput{
		Name: p.Name, Variables: []types.PipelineVariable{{Name: new("message"), Value: new("regression")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusRejected)
	pipelineWait(t, c, *p.Name, *failed.PipelineExecutionId, "Failed")

	var rolledBack *string
	for _, row := range pipelineExecutions(t, c, *p.Name) {
		if row.Trigger.TriggerType == types.TriggerTypeAutomatedRollback {
			if row.RollbackMetadata == nil ||
				aws.ToString(row.RollbackMetadata.RollbackTargetPipelineExecutionId) != *prior {
				t.Fatal(row)
			}

			rolledBack = row.PipelineExecutionId
		}
	}

	if rolledBack == nil {
		t.Fatal("missing automatic rollback")
	}

	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusApproved)
	pipelineWait(t, c, *p.Name, *rolledBack, "Succeeded")
	checkRollback(t, c, p.Name, rolledBack, prior, types.TriggerTypeAutomatedRollback)

	if _, err = c.UpdatePipeline(ctx, &sdk.UpdatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RollbackStage(ctx, &sdk.RollbackStageInput{
		PipelineName: p.Name, StageName: new("Approve"), TargetPipelineExecutionId: prior,
	}); !errors.As(err, new(*types.PipelineExecutionOutdatedException)) {
		t.Fatalf("rollback to obsolete version = %v", err)
	}
}

func checkRollback(t *testing.T, c *sdk.Client, name, id, target *string, trigger types.TriggerType) {
	t.Helper()
	ctx := context.Background()
	out, err := c.GetPipelineExecution(ctx, &sdk.GetPipelineExecutionInput{PipelineName: name, PipelineExecutionId: id})
	if err != nil {
		t.Fatal(err)
	}

	r := out.PipelineExecution
	if r.ExecutionType != types.ExecutionTypeRollback || r.Trigger.TriggerType != trigger ||
		r.RollbackMetadata == nil || aws.ToString(r.RollbackMetadata.RollbackTargetPipelineExecutionId) != *target ||
		len(
			r.ArtifactRevisions,
		) != 1 || len(r.Variables) != 1 || aws.ToString(r.Variables[0].ResolvedValue) != "known-good" {
		t.Fatalf("unexpected rollback execution %v", r)
	}

	actions, err := c.ListActionExecutions(ctx, &sdk.ListActionExecutionsInput{
		PipelineName: name, Filter: &types.ActionExecutionFilter{PipelineExecutionId: id},
	})
	if err != nil || len(actions.ActionExecutionDetails) != 2 {
		t.Fatal(actions, err)
	}

	for _, action := range actions.ActionExecutionDetails {
		if aws.ToString(action.StageName) != "Approve" {
			t.Fatalf("unexpected rollback action stage: %v", action)
		}

		if aws.ToString(action.ActionName) == "Deploy" &&
			action.Input.ResolvedConfiguration["StackName"] != "roll-back-src-known-good" {
			t.Fatalf("rollback did not restore its variables: %v", action)
		}
	}
}

func TestCodePipelineAutomaticStageRetry(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "retry-stage")
	p.Stages[1].OnFailure = &types.FailureConditions{
		Result:             types.ResultRetry,
		RetryConfiguration: &types.RetryConfiguration{RetryMode: types.StageRetryModeFailedActions},
	}
	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	initial := pipelineExecutions(t, c, *p.Name)[0].PipelineExecutionId
	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusRejected)
	approvePipeline(t, c, *p.Name, pipelineApproval(t, c, *p.Name), types.ApprovalStatusRejected)
	pipelineWait(t, c, *p.Name, *initial, "Failed")
	actions, err := c.ListActionExecutions(ctx, &sdk.ListActionExecutionsInput{
		PipelineName: p.Name, Filter: &types.ActionExecutionFilter{PipelineExecutionId: initial},
	})
	if err != nil || len(actions.ActionExecutionDetails) != 3 || len(pipelineExecutions(t, c, *p.Name)) != 1 {
		t.Fatal(actions, err)
	}
}

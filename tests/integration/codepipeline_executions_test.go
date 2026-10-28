package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
	sdk "github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	lambdasdk "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func pipelineWait(t *testing.T, c *sdk.Client, name, id, status string) {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GetPipelineExecution(
			context.Background(),
			&sdk.GetPipelineExecutionInput{PipelineName: new(name), PipelineExecutionId: new(id)},
		)
		if err != nil {
			t.Fatal(err)
		}

		if string(out.PipelineExecution.Status) == status {
			return
		}

		if out.PipelineExecution.Status == types.PipelineExecutionStatusFailed && status != "Failed" {
			actions, _ := c.ListActionExecutions(
				context.Background(),
				&sdk.ListActionExecutionsInput{
					PipelineName: new(name),
					Filter:       &types.ActionExecutionFilter{PipelineExecutionId: new(id)},
				},
			)
			data, _ := json.Marshal(actions)
			t.Fatalf("execution failed: %s: %s", aws.ToString(out.PipelineExecution.StatusSummary), data)
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("execution %s did not reach %s", id, status)
}

func pipelineApproval(t *testing.T, c *sdk.Client, name string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GetPipelineState(context.Background(), &sdk.GetPipelineStateInput{Name: new(name)})
		if err != nil {
			t.Fatal(err)
		}

		for _, stage := range out.StageStates {
			for _, a := range stage.ActionStates {
				if a.LatestExecution != nil && a.LatestExecution.Token != nil {
					return *a.LatestExecution.Token
				}
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("approval never became pending")
	return ""
}

func pipelineExecutions(t *testing.T, c *sdk.Client, name string) []types.PipelineExecutionSummary {
	t.Helper()
	out, err := c.ListPipelineExecutions(
		context.Background(),
		&sdk.ListPipelineExecutionsInput{PipelineName: new(name)},
	)
	if err != nil {
		t.Fatal(err)
	}

	return out.PipelineExecutionSummaries
}

func pipelineZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return data.Bytes()
}

func pipelineSource(t *testing.T, s *httptest.Server, name string) *types.PipelineDeclaration {
	t.Helper()
	ctx := context.Background()
	buckets := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	for _, bucket := range []string{name + "-src", name + "-artifacts"} {
		if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(bucket)}); err != nil {
			t.Fatal(err)
		}
	}

	body := pipelineZip(t, map[string]string{"template.json": `{"Resources":{"Queue":{"Type":"AWS::SQS::Queue"}}}`})
	if _, err := buckets.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new(name + "-src"), Key: new("source.zip"), Body: bytes.NewReader(body)},
	); err != nil {
		t.Fatal(err)
	}

	p := codePipelineDeclaration(name)
	p.ArtifactStore.Location = new(name + "-artifacts")
	p.Stages[0].Actions[0].Configuration = map[string]string{"S3Bucket": name + "-src", "S3ObjectKey": "source.zip"}
	p.Stages[1] = types.StageDeclaration{
		Name: new("Approve"),
		Actions: []types.ActionDeclaration{
			codePipelineAction("Review", types.ActionCategoryApproval, "Manual", "", ""),
		},
	}
	p.Stages[1].Actions[0].Configuration = nil
	return p
}

func approvePipeline(t *testing.T, c *sdk.Client, name, token string, status types.ApprovalStatus) {
	t.Helper()
	_, err := c.PutApprovalResult(
		context.Background(),
		&sdk.PutApprovalResultInput{
			PipelineName: new(name),
			StageName:    new("Approve"),
			ActionName:   new("Review"),
			Token:        new(token),
			Result:       &types.ApprovalResult{Status: status, Summary: new("reviewed")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCodePipelineExecutionApprovalDeploy(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "release")
	cc := codecommit.New(codecommit.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	if _, err := cc.CreateRepository(
		ctx,
		&codecommit.CreateRepositoryInput{RepositoryName: new("release")},
	); err != nil {
		t.Fatal(err)
	}

	file, err := cc.PutFile(
		ctx,
		&codecommit.PutFileInput{
			RepositoryName: new("release"),
			BranchName:     new("main"),
			FilePath:       new("template.json"),
			FileContent: []byte(
				`{"Resources":{"Queue":{"Type":"AWS::SQS::Queue"}},"Outputs":{"Queue":{"Value":{"Ref":"Queue"}}}}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	p.Stages[0].Actions[0].ActionTypeId.Provider = new("CodeCommit")
	p.Stages[0].Actions[0].Configuration = map[string]string{
		"RepositoryName":       "release",
		"BranchName":           "main",
		"PollForSourceChanges": "false",
	}
	p.Stages[0].Actions[0].Namespace = new("Source")
	deploy := codePipelineAction("Stack", types.ActionCategoryDeploy, "CloudFormation", "src", "stackOutput")
	deploy.Configuration = map[string]string{
		"ActionMode":     "CREATE_UPDATE",
		"StackName":      "pipeline-stack",
		"TemplatePath":   "src::template.json",
		"OutputFileName": "outputs.json",
	}
	p.Stages[1].Actions[0].Configuration = map[string]string{
		"CustomData": "#{Source.CommitId}/#{codepipeline.PipelineExecutionId}",
	}

	p.Stages = append(p.Stages, types.StageDeclaration{Name: new("Deploy"), Actions: []types.ActionDeclaration{deploy}})
	if _, err = c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	rows := pipelineExecutions(t, c, "release")
	if len(rows) != 1 || rows[0].Trigger.TriggerType != types.TriggerTypeCreatePipeline {
		t.Fatal(rows)
	}

	id := *rows[0].PipelineExecutionId
	token := pipelineApproval(t, c, "release")
	approvePipeline(t, c, "release", token, types.ApprovalStatusApproved)
	pipelineWait(t, c, "release", id, "Succeeded")
	stacks := cloudformation.New(
		cloudformation.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	if _, err = stacks.DescribeStacks(
		ctx,
		&cloudformation.DescribeStacksInput{StackName: new("pipeline-stack")},
	); err != nil {
		t.Fatal(err)
	}

	actions, err := c.ListActionExecutions(
		ctx,
		&sdk.ListActionExecutionsInput{
			PipelineName: new("release"),
			Filter:       &types.ActionExecutionFilter{PipelineExecutionId: new(id)},
			MaxResults:   new(int32(1)),
		},
	)
	if err != nil || len(actions.ActionExecutionDetails) != 1 || actions.NextToken == nil {
		t.Fatal(actions, err)
	}

	var all []types.ActionExecutionDetail
	pages := sdk.NewListActionExecutionsPaginator(
		c,
		&sdk.ListActionExecutionsInput{PipelineName: new("release"), MaxResults: new(int32(1))},
	)
	for pages.HasMorePages() {
		out, e := pages.NextPage(ctx)
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, out.ActionExecutionDetails...)
	}

	if len(all) != 3 || all[0].Output.OutputVariables["CommitId"] != *file.CommitId {
		t.Fatal(all)
	}
	if all[1].Input.ResolvedConfiguration["CustomData"] != *file.CommitId+"/"+id ||
		len(all[2].Output.OutputArtifacts) != 1 {
		t.Fatal("action variables or output artifact missing", all)
	}

	if _, dateErr := time.Parse(time.RFC3339, all[0].Output.OutputVariables["AuthorDate"]); dateErr != nil {
		t.Fatal(dateErr)
	}

	location := all[2].Output.OutputArtifacts[0].S3location
	buckets := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	artifact, artifactErr := buckets.GetObject(ctx, &s3.GetObjectInput{Bucket: location.Bucket, Key: location.Key})
	if artifactErr != nil {
		t.Fatal(artifactErr)
	}

	data, readErr := io.ReadAll(artifact.Body)
	artifact.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}

	archive, archiveErr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if archiveErr != nil || len(archive.File) != 1 || archive.File[0].Name != "outputs.json" {
		t.Fatal(archive, archiveErr)
	}

	started, err := c.StartPipelineExecution(
		ctx,
		&sdk.StartPipelineExecutionInput{Name: new("release"), ClientRequestToken: new("idempotent")},
	)
	if err != nil {
		t.Fatal(err)
	}

	duplicate, err := c.StartPipelineExecution(
		ctx,
		&sdk.StartPipelineExecutionInput{Name: new("release"), ClientRequestToken: new("idempotent")},
	)
	if err != nil || *duplicate.PipelineExecutionId != *started.PipelineExecutionId {
		t.Fatal(duplicate, err)
	}

	approvePipeline(t, c, "release", pipelineApproval(t, c, "release"), types.ApprovalStatusRejected)
	pipelineWait(t, c, "release", *started.PipelineExecutionId, "Failed")
	if _, err = c.RetryStageExecution(
		ctx,
		&sdk.RetryStageExecutionInput{
			PipelineName:        new("release"),
			PipelineExecutionId: started.PipelineExecutionId,
			StageName:           new("Approve"),
			RetryMode:           types.StageRetryModeFailedActions,
		},
	); err != nil {
		t.Fatal(err)
	}

	approvePipeline(t, c, "release", pipelineApproval(t, c, "release"), types.ApprovalStatusApproved)
	pipelineWait(t, c, "release", *started.PipelineExecutionId, "Succeeded")
	// Update does not execute; the next push does.
	p.Stages[0].Actions[0].Configuration["PollForSourceChanges"] = "true"
	if _, err = c.UpdatePipeline(ctx, &sdk.UpdatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	if len(pipelineExecutions(t, c, "release")) != 2 {
		t.Fatal("update started execution")
	}

	if _, err = cc.PutFile(
		ctx,
		&codecommit.PutFileInput{
			RepositoryName: new("release"),
			BranchName:     new("main"),
			ParentCommitId: file.CommitId,
			FilePath:       new("readme"),
			FileContent:    []byte("push"),
		},
	); err != nil {
		t.Fatal(err)
	}

	if len(pipelineExecutions(t, c, "release")) != 3 {
		t.Fatal("push did not start execution")
	}
}

func TestCodePipelineExecutionTransitionsModesStop(t *testing.T) {
	for _, mode := range []types.ExecutionMode{
		types.ExecutionModeSuperseded, types.ExecutionModeQueued, types.ExecutionModeParallel,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			s := server(t)
			c := codePipelineClient(s, "us-east-1")
			ctx := context.Background()
			p := pipelineSource(t, s, "modes")
			p.ExecutionMode = mode
			p.PipelineType = types.PipelineTypeV2
			if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
				t.Fatal(err)
			}

			first := pipelineExecutions(t, c, "modes")[0].PipelineExecutionId
			// Hold before any action enters approval.
			if _, err := c.DisableStageTransition(
				ctx,
				&sdk.DisableStageTransitionInput{
					PipelineName:   p.Name,
					StageName:      new("Approve"),
					TransitionType: types.StageTransitionTypeInbound,
					Reason:         new("hold"),
				},
			); err != nil {
				t.Fatal(err)
			}

			second, err := c.StartPipelineExecution(ctx, &sdk.StartPipelineExecutionInput{Name: p.Name})
			if err != nil {
				t.Fatal(err)
			}

			if mode == types.ExecutionModeSuperseded {
				pipelineWait(t, c, "modes", *first, "Superseded")
			} else {
				time.Sleep(5 * time.Second)
			}

			state, err := c.GetPipelineState(ctx, &sdk.GetPipelineStateInput{Name: p.Name})
			if err != nil || state.StageStates[1].InboundTransitionState.Enabled {
				t.Fatal(state, err)
			}

			for _, a := range state.StageStates[1].ActionStates {
				if a.LatestExecution != nil {
					t.Fatal("disabled stage entered")
				}
			}

			if _, err = c.EnableStageTransition(
				ctx,
				&sdk.EnableStageTransitionInput{
					PipelineName:   p.Name,
					StageName:      new("Approve"),
					TransitionType: types.StageTransitionTypeInbound,
				},
			); err != nil {
				t.Fatal(err)
			}

			pipelineApproval(t, c, "modes")
			if _, err = c.StopPipelineExecution(
				ctx,
				&sdk.StopPipelineExecutionInput{PipelineName: p.Name, PipelineExecutionId: first, Abandon: true},
			); err == nil &&
				mode == types.ExecutionModeSuperseded {
				t.Fatal("stopped terminal execution")
			}

			if _, err = c.StopPipelineExecution(
				ctx,
				&sdk.StopPipelineExecutionInput{
					PipelineName:        p.Name,
					PipelineExecutionId: second.PipelineExecutionId,
					Abandon:             false,
				},
			); err != nil {
				t.Fatal(err)
			}

			pipelineWait(t, c, "modes", *second.PipelineExecutionId, "Stopped")
		})
	}
}

func TestCodePipelineExecutionPersistentApproval(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	//nolint:gocritic // Follow the replacement stop function after restart.
	defer func() { stop() }()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "persistpipe")
	if _, err := c.CreatePipeline(context.Background(), &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	id := pipelineExecutions(t, c, "persistpipe")[0].PipelineExecutionId
	token := pipelineApproval(t, c, "persistpipe")
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = codePipelineClient(s, "us-east-1")
	if got := pipelineApproval(t, c, "persistpipe"); got != token {
		t.Fatal("approval token lost")
	}

	approvePipeline(t, c, "persistpipe", token, types.ApprovalStatusApproved)
	pipelineWait(t, c, "persistpipe", *id, "Succeeded")
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		s.URL+"/_tinystack/reset",
		http.NoBody,
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}

	listed, err := c.ListPipelines(context.Background(), &sdk.ListPipelinesInput{})
	if err != nil || len(listed.Pipelines) != 0 {
		t.Fatal(listed, err)
	}
}

type pipelineRuntime struct{ jobs chan string }

func (r *pipelineRuntime) Invoke(_ context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event struct {
		Job struct {
			ID string `json:"id"`
		} `json:"CodePipeline.job"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.jobs <- event.Job.ID
	return lambda.Result{Payload: []byte(`{}`)}, nil
}
func (r *pipelineRuntime) Release(string) {}

func TestCodePipelineExecutionLambdaResults(t *testing.T) {
	runtime := &pipelineRuntime{jobs: make(chan string, 4)}
	s := serverWith(t, runtime)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	functions := lambdasdk.New(
		lambdasdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	if _, err := functions.CreateFunction(
		ctx,
		&lambdasdk.CreateFunctionInput{
			FunctionName: new("pipeline-job"),
			Role:         new(codePipelineRole),
			Runtime:      lambdatypes.RuntimePython312,
			Handler:      new("app.run"),
			Code:         &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def run(e,c): return {}")},
		},
	); err != nil {
		t.Fatal(err)
	}

	p := pipelineSource(t, s, "jobpipe")
	action := codePipelineAction("Invoke", types.ActionCategoryInvoke, "Lambda", "src", "")
	action.Configuration = map[string]string{
		"FunctionName":   "pipeline-job",
		"UserParameters": "#{codepipeline.PipelineExecutionId}",
	}
	p.Stages[1] = types.StageDeclaration{Name: new("Invoke"), Actions: []types.ActionDeclaration{action}}
	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	id := pipelineExecutions(t, c, "jobpipe")[0].PipelineExecutionId
	var job string
	select {
	case job = <-runtime.jobs:
	case <-time.After(30 * time.Second):
		t.Fatal("Lambda not invoked")
	}

	if _, err := c.PutJobSuccessResult(
		ctx,
		&sdk.PutJobSuccessResultInput{JobId: new(job), OutputVariables: map[string]string{"result": "yes"}},
	); err != nil {
		t.Fatal(err)
	}

	pipelineWait(t, c, "jobpipe", *id, "Succeeded")
	started, err := c.StartPipelineExecution(ctx, &sdk.StartPipelineExecutionInput{Name: p.Name})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case job = <-runtime.jobs:
	case <-time.After(30 * time.Second):
		t.Fatal("Lambda not invoked")
	}

	if _, err = c.PutJobFailureResult(
		ctx,
		&sdk.PutJobFailureResultInput{
			JobId:          new(job),
			FailureDetails: &types.FailureDetails{Type: types.FailureTypeJobFailed, Message: new("rejected")},
		},
	); err != nil {
		t.Fatal(err)
	}

	pipelineWait(t, c, "jobpipe", *started.PipelineExecutionId, "Failed")
}

func TestCodePipelineExecutionBuildRequiresDocker(t *testing.T) {
	s := server(t)
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "buildpipe")
	a := codePipelineAction("Build", types.ActionCategoryBuild, "CodeBuild", "src", "")
	a.Configuration = map[string]string{"ProjectName": "project"}
	p.Stages[1] = types.StageDeclaration{Name: new("Build"), Actions: []types.ActionDeclaration{a}}
	if _, err := c.CreatePipeline(context.Background(), &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	id := pipelineExecutions(t, c, "buildpipe")[0].PipelineExecutionId
	pipelineWait(t, c, "buildpipe", *id, "Failed")
	out, err := c.ListActionExecutions(context.Background(), &sdk.ListActionExecutionsInput{PipelineName: p.Name})
	if err != nil || len(out.ActionExecutionDetails) != 2 ||
		!strings.Contains(
			aws.ToString(out.ActionExecutionDetails[1].Output.ExecutionResult.ErrorDetails.Message),
			"codebuild.builds: docker",
		) {
		t.Fatal(out, err)
	}
}

func (r *pipelineRuntime) Close() {}

func TestCodePipelineExecutionVariablesAndPages(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "variables")
	p.PipelineType = types.PipelineTypeV2
	p.ExecutionMode = types.ExecutionModeParallel
	p.Variables = []types.PipelineVariableDeclaration{{Name: new("environment"), DefaultValue: new("dev")}}
	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	started, err := c.StartPipelineExecution(
		ctx,
		&sdk.StartPipelineExecutionInput{
			Name:      p.Name,
			Variables: []types.PipelineVariable{{Name: new("environment"), Value: new("prod")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.GetPipelineExecution(
		ctx,
		&sdk.GetPipelineExecutionInput{PipelineName: p.Name, PipelineExecutionId: started.PipelineExecutionId},
	)
	if err != nil || len(out.PipelineExecution.Variables) != 1 ||
		aws.ToString(out.PipelineExecution.Variables[0].ResolvedValue) != "prod" {
		t.Fatal(out, err)
	}

	var ids []string
	pages := sdk.NewListPipelineExecutionsPaginator(
		c,
		&sdk.ListPipelineExecutionsInput{PipelineName: p.Name, MaxResults: new(int32(1))},
	)
	for pages.HasMorePages() {
		page, e := pages.NextPage(ctx)
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range page.PipelineExecutionSummaries {
			ids = append(ids, *r.PipelineExecutionId)
		}
	}

	if len(ids) != 2 || ids[0] >= ids[1] {
		t.Fatal(ids)
	}

	if _, err = c.StartPipelineExecution(
		ctx,
		&sdk.StartPipelineExecutionInput{
			Name:      p.Name,
			Variables: []types.PipelineVariable{{Name: new("unknown"), Value: new("x")}},
		},
	); err == nil {
		t.Fatal("unknown variable accepted")
	}

	if _, err = c.StartPipelineExecution(
		ctx,
		&sdk.StartPipelineExecutionInput{
			Name: p.Name,
			SourceRevisions: []types.SourceRevisionOverride{
				{
					ActionName:    new("missing"),
					RevisionType:  types.SourceRevisionTypeS3ObjectVersionId,
					RevisionValue: new("id"),
				},
			},
		},
	); err == nil {
		t.Fatal("unknown source accepted")
	}

	if _, err = c.ListPipelineExecutions(
		ctx,
		&sdk.ListPipelineExecutionsInput{PipelineName: p.Name, NextToken: new("!")},
	); err == nil {
		t.Fatal("bad token accepted")
	}

	if _, err = c.PutApprovalResult(
		ctx,
		&sdk.PutApprovalResultInput{
			PipelineName: p.Name,
			StageName:    new("Approve"),
			ActionName:   new("Review"),
			Token:        new("bad"),
			Result:       &types.ApprovalResult{Status: types.ApprovalStatusApproved, Summary: new("bad")},
		},
	); err == nil {
		t.Fatal("bad approval accepted")
	}

	for _, id := range ids {
		if _, err = c.StopPipelineExecution(
			ctx,
			&sdk.StopPipelineExecutionInput{PipelineName: p.Name, PipelineExecutionId: new(id), Abandon: true},
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodePipelineExecutionECS(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "ecspipe")
	containers := ecsClient(s, "us-east-1")
	if _, err := containers.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: new("pipeline")}); err != nil {
		t.Fatal(err)
	}

	task := webTask(ctx, t, containers, "pipeline", "example/old:1")
	if _, err := containers.CreateService(
		ctx,
		&ecs.CreateServiceInput{
			Cluster:              new("pipeline"),
			ServiceName:          new("web"),
			TaskDefinition:       task.TaskDefinitionArn,
			DesiredCount:         new(int32(0)),
			LaunchType:           ecstypes.LaunchTypeFargate,
			NetworkConfiguration: awsvpc(),
		},
	); err != nil {
		t.Fatal(err)
	}

	buckets := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	data := pipelineZip(t, map[string]string{"imagedefinitions.json": `[{"name":"web","imageUri":"example/new:2"}]`})
	if _, err := buckets.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new("ecspipe-src"), Key: new("source.zip"), Body: bytes.NewReader(data)},
	); err != nil {
		t.Fatal(err)
	}

	a := codePipelineAction("Deploy", types.ActionCategoryDeploy, "ECS", "src", "")
	a.Configuration = map[string]string{"ClusterName": "pipeline", "ServiceName": "web"}
	p.Stages[1] = types.StageDeclaration{Name: new("Deploy"), Actions: []types.ActionDeclaration{a}}
	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	id := pipelineExecutions(t, c, "ecspipe")[0].PipelineExecutionId
	pipelineWait(t, c, "ecspipe", *id, "Succeeded")
	services, err := containers.DescribeServices(
		ctx,
		&ecs.DescribeServicesInput{Cluster: new("pipeline"), Services: []string{"web"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	definition, err := containers.DescribeTaskDefinition(
		ctx,
		&ecs.DescribeTaskDefinitionInput{TaskDefinition: services.Services[0].TaskDefinition},
	)
	if err != nil || definition.TaskDefinition.Revision != 2 ||
		*definition.TaskDefinition.ContainerDefinitions[0].Image != "example/new:2" {
		t.Fatal(definition, err)
	}
}

func TestCodePipelineExecutionEvents(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "eventpipe")
	queues := sqsClient(s, "test")
	url, arn := queueARN(t, queues, "pipeline-events")
	bus := eventsClient(s, "test")
	putRule(t, bus, "pipeline-state", `{"source":["aws.codepipeline"]}`)
	if _, err := bus.PutTargets(
		ctx,
		&eventbridge.PutTargetsInput{
			Rule:    new("pipeline-state"),
			Targets: []ebtypes.Target{{Id: new("queue"), Arn: new(arn)}},
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	id := pipelineExecutions(t, c, "eventpipe")[0].PipelineExecutionId
	approvePipeline(t, c, "eventpipe", pipelineApproval(t, c, "eventpipe"), types.ApprovalStatusApproved)
	pipelineWait(t, c, "eventpipe", *id, "Succeeded")
	seen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := queues.ReceiveMessage(
			ctx,
			&sqs.ReceiveMessageInput{QueueUrl: new(url), MaxNumberOfMessages: 10},
		)
		if err != nil {
			t.Fatal(err)
		}

		for _, message := range messages.Messages {
			var event struct {
				Kind   string `json:"detail-type"`
				Detail struct {
					State string `json:"state"`
				} `json:"detail"`
			}
			if err = json.Unmarshal([]byte(*message.Body), &event); err != nil {
				t.Fatal(err)
			}

			seen[event.Kind+"/"+event.Detail.State] = true
		}

		if seen["CodePipeline Pipeline Execution State Change/SUCCEEDED"] {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	for _, kind := range []string{"Pipeline", "Stage", "Action"} {
		for _, state := range []string{"STARTED", "SUCCEEDED"} {
			if !seen["CodePipeline "+kind+" Execution State Change/"+state] {
				t.Fatalf("missing %s %s in %v", kind, state, seen)
			}
		}
	}
}

func TestCodePipelineExecutionSourceVersionOverride(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codePipelineClient(s, "us-east-1")
	p := pipelineSource(t, s, "versionpipe")
	p.ExecutionMode = types.ExecutionModeParallel
	p.PipelineType = types.PipelineTypeV2
	buckets := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	if _, err := buckets.PutBucketVersioning(
		ctx,
		&s3.PutBucketVersioningInput{
			Bucket:                  new("versionpipe-src"),
			VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
		},
	); err != nil {
		t.Fatal(err)
	}

	old, err := buckets.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("versionpipe-src"),
			Key:    new("source.zip"),
			Body:   bytes.NewReader(pipelineZip(t, map[string]string{"old": "old"})),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = buckets.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("versionpipe-src"),
			Key:    new("source.zip"),
			Body:   bytes.NewReader(pipelineZip(t, map[string]string{"new": "new"})),
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreatePipeline(ctx, &sdk.CreatePipelineInput{Pipeline: p}); err != nil {
		t.Fatal(err)
	}

	started, err := c.StartPipelineExecution(
		ctx,
		&sdk.StartPipelineExecutionInput{
			Name: p.Name,
			SourceRevisions: []types.SourceRevisionOverride{
				{
					ActionName:    new("Fetch"),
					RevisionType:  types.SourceRevisionTypeS3ObjectVersionId,
					RevisionValue: old.VersionId,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var artifact *types.S3Location
	for time.Now().Before(deadline) {
		actions, e := c.ListActionExecutions(
			ctx,
			&sdk.ListActionExecutionsInput{
				PipelineName: p.Name,
				Filter:       &types.ActionExecutionFilter{PipelineExecutionId: started.PipelineExecutionId},
			},
		)
		if e != nil {
			t.Fatal(e)
		}

		if len(actions.ActionExecutionDetails) > 0 && actions.ActionExecutionDetails[0].Output != nil {
			action := actions.ActionExecutionDetails[0]
			if action.Output.OutputVariables["VersionId"] != *old.VersionId {
				t.Fatal(action)
			}

			artifact = action.Output.OutputArtifacts[0].S3location
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if artifact == nil {
		t.Fatal("source did not finish")
	}

	object, err := buckets.GetObject(ctx, &s3.GetObjectInput{Bucket: artifact.Bucket, Key: artifact.Key})
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) != 1 || archive.File[0].Name != "old" {
		t.Fatal(archive, err)
	}
}

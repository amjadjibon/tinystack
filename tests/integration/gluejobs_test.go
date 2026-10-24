package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gt "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func gluejobPython(t *testing.T, boto bool) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is absent")
	}

	if boto {
		if err := exec.CommandContext(context.Background(), "python3", "-c", "import boto3").Run(); err != nil {
			t.Skip("boto3 is absent")
		}
	}
}

func gluejobSetup(t *testing.T, s *httptest.Server, job, script string) *glue.Client {
	t.Helper()
	ctx := context.Background()
	roles := iamClient(s, "test", "us-east-1")
	_, err := roles.CreateRole(ctx, &iam.CreateRoleInput{RoleName: new(job), AssumeRolePolicyDocument: new(`{
 "Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"glue.amazonaws.com"},
 "Action":"sts:AssumeRole"}]}`)})
	if err != nil {
		t.Fatal(err)
	}

	objects := s3Client(s, "test", "us-east-1")
	_, err = objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(job)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = objects.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new(job), Key: new("job.py"), Body: strings.NewReader(script)},
	)
	if err != nil {
		t.Fatal(err)
	}

	c := glueClient(s)
	_, err = c.CreateJob(ctx, &glue.CreateJobInput{
		Name: new(job),
		Role: new(job),
		Command: &gt.JobCommand{
			Name:           new("pythonshell"),
			ScriptLocation: new("s3://" + job + "/job.py"),
			PythonVersion:  new("3.9"),
		},
		DefaultArguments:        map[string]string{"--value": "default", "--locked": "default"},
		NonOverridableArguments: map[string]string{"--locked": "fixed"},
		Tags:                    map[string]string{"team": "core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func gluejobWait(t *testing.T, c *glue.Client, job, id string, states ...gt.JobRunState) *gt.JobRun {
	t.Helper()
	deadline := time.Now().Add(80 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GetJobRun(context.Background(), &glue.GetJobRunInput{JobName: new(job), RunId: new(id)})
		if err != nil {
			t.Fatal(err)
		}

		for _, state := range states {
			if out.JobRun.JobRunState == state {
				return out.JobRun
			}
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("job %s/%s did not reach %v", job, id, states)
	return nil
}

func gluejobStart(t *testing.T, c *glue.Client, job string, args map[string]string) string {
	t.Helper()
	out, err := c.StartJobRun(context.Background(), &glue.StartJobRunInput{JobName: new(job), Arguments: args})
	if err != nil {
		t.Fatal(err)
	}

	if len(*out.JobRunId) != 67 || !strings.HasPrefix(*out.JobRunId, "jr_") {
		t.Fatalf("invalid run ID: %v", out)
	}

	return *out.JobRunId
}

func TestGlueJobPythonSDKArgumentsLogs(t *testing.T) {
	gluejobPython(t, true)
	s := serverWith(t, local.New(local.Options{}))
	c := gluejobSetup(t, s, "glue-python", `import boto3,sys,json
args=dict(zip(sys.argv[1::2],sys.argv[2::2]))
assert args['--value']=='run' and args['--locked']=='fixed'
boto3.client('s3').put_object(Bucket='glue-python',Key='result',Body=json.dumps(args).encode())
print('glue stdout')
print('glue stderr',file=sys.stderr)
`)
	id := gluejobStart(t, c, "glue-python", map[string]string{"--value": "run", "--locked": "override"})
	r := gluejobWait(t, c, "glue-python", id, gt.JobRunStateSucceeded, gt.JobRunStateFailed)
	if r.JobRunState != gt.JobRunStateSucceeded {
		t.Fatalf("run failed: %+v", r)
	}

	if r.CompletedOn == nil || r.ExecutionTime < 0 || r.Arguments["--locked"] != "fixed" {
		t.Fatalf("bad run: %+v", r)
	}

	object, err := s3Client(s, "test", "us-east-1").GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: new("glue-python"), Key: new("result"),
	})
	if err != nil {
		t.Fatal(err)
	}

	object.Body.Close()
	for _, kind := range []string{"output", "error"} {
		logs, err := logsClient(s, "test").GetLogEvents(context.Background(), &cloudwatchlogs.GetLogEventsInput{
			LogGroupName: new("/aws-glue/python-jobs/" + kind), LogStreamName: new(id),
		})
		if err != nil || len(logs.Events) == 0 {
			t.Fatalf("logs %s: %+v %v", kind, logs, err)
		}
	}
}

func TestGlueJobStopRetryFailureAndValidation(t *testing.T) {
	gluejobPython(t, false)
	ctx := context.Background()
	s := serverWith(t, local.New(local.Options{}))
	c := gluejobSetup(t, s, "glue-stop", "import time\ntime.sleep(120)\n")
	id := gluejobStart(t, c, "glue-stop", nil)
	gluejobWait(t, c, "glue-stop", id, gt.JobRunStateRunning)
	_, err := c.StartJobRun(ctx, &glue.StartJobRunInput{JobName: new("glue-stop")})
	code(t, err, "ConcurrentRunsExceededException")
	_, err = c.BatchStopJobRun(ctx, &glue.BatchStopJobRunInput{JobName: new("glue-stop"), JobRunIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}

	gluejobWait(t, c, "glue-stop", id, gt.JobRunStateStopped)
	_, err = s3Client(s, "test", "us-east-1").PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("glue-stop"), Key: new("job.py"),
		Body: strings.NewReader("import sys\nprint('specific failure',file=sys.stderr)\nsys.exit(3)\n"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateJob(ctx, &glue.UpdateJobInput{JobName: new("glue-stop"), JobUpdate: &gt.JobUpdate{
		Role:    new("glue-stop"),
		Command: &gt.JobCommand{Name: new("pythonshell"), ScriptLocation: new("s3://glue-stop/job.py")}, MaxRetries: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}

	id = gluejobStart(t, c, "glue-stop", nil)
	failed := gluejobWait(t, c, "glue-stop", id, gt.JobRunStateFailed)
	if failed.ErrorMessage == nil || !strings.Contains(*failed.ErrorMessage, "specific failure") {
		t.Fatalf("failure: %+v", failed)
	}

	deadline := time.Now().Add(5 * time.Second)
	retried := false
	for time.Now().Before(deadline) {
		rows, listErr := c.GetJobRuns(ctx, &glue.GetJobRunsInput{JobName: new("glue-stop")})
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, r := range rows.JobRuns {
			if r.Attempt == 1 && r.JobRunState == gt.JobRunStateFailed {
				retried = true
			}
		}

		if retried {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	if !retried {
		t.Fatal("automatic retry did not run")
	}

	j, err := c.GetJob(ctx, &glue.GetJobInput{JobName: new("glue-stop")})
	if err != nil || len(j.Job.DefaultArguments) != 0 {
		t.Fatalf("UpdateJob must replace the definition: %+v %v", j, err)
	}

	_, err = c.CreateJob(ctx, &glue.CreateJobInput{Name: new("bad"), Role: new("absent"), Command: j.Job.Command})
	code(t, err, "InvalidInputException")
	_, err = c.CreateJob(ctx, &glue.CreateJobInput{
		Name: new("spark"), Role: new("glue-stop"),
		Command: &gt.JobCommand{Name: new("glueetl"), ScriptLocation: new("s3://glue-stop/job.py")},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartJobRun(ctx, &glue.StartJobRunInput{JobName: new("spark")})
	code(t, err, "InvalidInputException")
	_, err = c.StartJobRun(ctx, &glue.StartJobRunInput{JobName: new("missing")})
	code(t, err, "EntityNotFoundException")
	_, err = c.StartJobRun(ctx, &glue.StartJobRunInput{JobName: new("glue-stop"), Timeout: new(int32(0))})
	code(t, err, "InvalidInputException")
	_, err = c.GetJobRuns(ctx, &glue.GetJobRunsInput{JobName: new("glue-stop"), NextToken: new("!invalid!")})
	code(t, err, "InvalidInputException")
}

func TestGlueJobMissingScriptAndDisabled(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := gluejobSetup(t, s, "glue-disabled", "print('must not run')")
	id := gluejobStart(t, c, "glue-disabled", nil)
	r := gluejobWait(t, c, "glue-disabled", id, gt.JobRunStateFailed)
	if !strings.Contains(*r.ErrorMessage, "Local execution is disabled") {
		t.Fatalf("disabled execution: %+v", r)
	}

	gluejobPython(t, false)
	localServer := serverWith(t, local.New(local.Options{}))
	c = gluejobSetup(t, localServer, "glue-missing", "")
	_, err := s3Client(localServer, "test", "us-east-1").DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: new("glue-missing"), Key: new("job.py"),
	})
	if err != nil {
		t.Fatal(err)
	}

	id = gluejobStart(t, c, "glue-missing", nil)
	r = gluejobWait(t, c, "glue-missing", id, gt.JobRunStateFailed)
	if !strings.Contains(*r.ErrorMessage, "S3") {
		t.Fatalf("missing script: %+v", r)
	}
}

func TestGlueJobTimeout(t *testing.T) {
	t.Parallel()
	gluejobPython(t, false)
	s := serverWith(t, local.New(local.Options{}))
	c := gluejobSetup(t, s, "glue-timeout", "import time\ntime.sleep(120)\n")
	out, err := c.StartJobRun(
		context.Background(),
		&glue.StartJobRunInput{JobName: new("glue-timeout"), Timeout: new(int32(1))},
	)
	if err != nil {
		t.Fatal(err)
	}

	r := gluejobWait(t, c, "glue-timeout", *out.JobRunId, gt.JobRunStateTimeout)
	if r.ExecutionTime < 60 {
		t.Fatalf("timeout fired too early: %+v", r)
	}
}

func TestGlueWorkflowChainPropertiesGraphTagsAndPages(t *testing.T) {
	gluejobPython(t, false)
	ctx := context.Background()
	s := serverWith(t, local.New(local.Options{}))
	c := gluejobSetup(t, s, "glue-first", "print('first')\n")
	gluejobSetup(t, s, "glue-second", "print('second')\n")
	_, err := c.CreateWorkflow(ctx, &glue.CreateWorkflowInput{
		Name: new("flow"), MaxConcurrentRuns: new(int32(1)),
		DefaultRunProperties: map[string]string{"default": "kept"}, Tags: map[string]string{"team": "core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("start"), Type: gt.TriggerTypeOnDemand, WorkflowName: new("flow"),
		Actions: []gt.Action{{JobName: new("glue-first")}}, Tags: map[string]string{"team": "core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("then"), Type: gt.TriggerTypeConditional, WorkflowName: new("flow"),
		StartOnCreation: true, Actions: []gt.Action{{JobName: new("glue-second")}}, Predicate: &gt.Predicate{
			Logical: gt.LogicalAnd,
			Conditions: []gt.Condition{
				{JobName: new("glue-first"), State: gt.JobRunStateSucceeded, LogicalOperator: gt.LogicalOperatorEquals},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := c.StartWorkflowRun(
		ctx,
		&glue.StartWorkflowRunInput{Name: new("flow"), RunProperties: map[string]string{"run": "value"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartWorkflowRun(ctx, &glue.StartWorkflowRunInput{Name: new("flow")})
	code(t, err, "ConcurrentRunsExceededException")
	_, err = c.PutWorkflowRunProperties(ctx, &glue.PutWorkflowRunPropertiesInput{
		Name: new("flow"), RunId: run.RunId,
		RunProperties: map[string]string{"extra": "changed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(8 * time.Second)
	var completed *gt.WorkflowRun
	for time.Now().Before(deadline) {
		out, getErr := c.GetWorkflowRun(
			ctx,
			&glue.GetWorkflowRunInput{Name: new("flow"), RunId: run.RunId, IncludeGraph: new(true)},
		)
		if getErr != nil {
			t.Fatal(getErr)
		}

		if out.Run.Status == gt.WorkflowRunStatusCompleted {
			completed = out.Run
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	if completed == nil || completed.Statistics.SucceededActions != 2 || len(completed.Graph.Nodes) != 4 ||
		len(completed.Graph.Edges) != 3 {
		t.Fatalf("workflow chain: %+v", completed)
	}

	props, err := c.GetWorkflowRunProperties(
		ctx,
		&glue.GetWorkflowRunPropertiesInput{Name: new("flow"), RunId: run.RunId},
	)
	if err != nil || props.RunProperties["default"] != "kept" || props.RunProperties["extra"] != "changed" {
		t.Fatalf("properties: %+v %v", props, err)
	}

	graph, err := c.GetWorkflow(ctx, &glue.GetWorkflowInput{Name: new("flow"), IncludeGraph: new(true)})
	if err != nil || len(graph.Workflow.Graph.Nodes) != 4 {
		t.Fatalf("graph: %+v %v", graph, err)
	}

	jobs, err := c.ListJobs(
		ctx,
		&glue.ListJobsInput{Tags: map[string]string{"team": "core"}, MaxResults: new(int32(1))},
	)
	if err != nil || len(jobs.JobNames) != 1 || jobs.NextToken == nil {
		t.Fatalf("pagination: %+v %v", jobs, err)
	}

	next, err := c.ListJobs(
		ctx,
		&glue.ListJobsInput{
			Tags:       map[string]string{"team": "core"},
			MaxResults: new(int32(1)),
			NextToken:  jobs.NextToken,
		},
	)
	if err != nil || len(next.JobNames) != 1 || next.JobNames[0] == jobs.JobNames[0] {
		t.Fatalf("next page: %+v %v", next, err)
	}

	for _, resource := range []string{"job/glue-first", "trigger/start", "workflow/flow"} {
		arn := "arn:aws:glue:us-east-1:000000000000:" + resource
		_, err = c.TagResource(
			ctx,
			&glue.TagResourceInput{ResourceArn: new(arn), TagsToAdd: map[string]string{"new": "tag"}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, err := c.GetTags(ctx, &glue.GetTagsInput{ResourceArn: new(arn)})
		if err != nil || tags.Tags["new"] != "tag" {
			t.Fatalf("tags: %+v %v", tags, err)
		}

		_, err = c.UntagResource(ctx, &glue.UntagResourceInput{ResourceArn: new(arn), TagsToRemove: []string{"new"}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func gluejobPersistent(t *testing.T, dir string) (s *httptest.Server, stop func()) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	s, stopServer := serverStore(t, cfg, local.New(local.Options{}), store)
	return s, func() {
		stopServer()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestGlueJobRestartFailsInterruptedRun(t *testing.T) {
	gluejobPython(t, false)
	dir := t.TempDir()
	s, stop := gluejobPersistent(t, dir)
	c := gluejobSetup(t, s, "glue-restart", "import time\ntime.sleep(120)\n")
	id := gluejobStart(t, c, "glue-restart", nil)
	gluejobWait(t, c, "glue-restart", id, gt.JobRunStateRunning)
	stop()
	s, stop = gluejobPersistent(t, dir)
	defer stop()
	r := gluejobWait(t, glueClient(s), "glue-restart", id, gt.JobRunStateFailed)
	if r.ErrorMessage == nil || !strings.Contains(*r.ErrorMessage, "interrupted") {
		t.Fatalf("restart recovery: %+v", r)
	}
}

func TestGlueJobScheduledTriggerAndStandaloneChain(t *testing.T) {
	gluejobPython(t, false)
	ctx := context.Background()
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), local.New(local.Options{}), store)
	defer stop()
	c := gluejobSetup(t, s, "scheduled-source", "print('scheduled')\n")
	gluejobSetup(t, s, "scheduled-target", "print('conditional')\n")
	_, err := c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("chain"), Type: gt.TriggerTypeConditional,
		StartOnCreation: true, Actions: []gt.Action{{JobName: new("scheduled-target")}}, Predicate: &gt.Predicate{
			Logical:    gt.LogicalAny,
			Conditions: []gt.Condition{{JobName: new("scheduled-source"), State: gt.JobRunStateSucceeded}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name:            new("schedule"),
		Type:            gt.TriggerTypeScheduled,
		StartOnCreation: true,
		Schedule:        new("cron(0/5 * * * ? *)"),
		Actions:         []gt.Action{{JobName: new("scheduled-source")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Make the durable timer overdue, exercising the worker without a five-minute wall-clock wait.
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		key := "000000000000/us-east-1/glue/trigger/schedule"
		record, _, readErr := storage.Read[map[string]json.RawMessage](tx, key)
		if readErr != nil {
			return readErr
		}

		(*record)["Next"] = json.RawMessage("1")
		return storage.Write(tx, key, record)
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "conditional Glue chain", func() bool {
		runs, getErr := c.GetJobRuns(ctx, &glue.GetJobRunsInput{JobName: new("scheduled-target")})
		return getErr == nil && len(runs.JobRuns) == 1 && runs.JobRuns[0].JobRunState == gt.JobRunStateSucceeded
	})
	_, err = c.StopTrigger(ctx, &glue.StopTriggerInput{Name: new("schedule")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("too-fast"), Type: gt.TriggerTypeScheduled,
		Schedule: new("cron(* * * * ? *)"), Actions: []gt.Action{{JobName: new("scheduled-source")}},
	})
	code(t, err, "InvalidInputException")
	_, err = c.UpdateTrigger(ctx, &glue.UpdateTriggerInput{Name: new("schedule"), TriggerUpdate: &gt.TriggerUpdate{
		Actions: []gt.Action{
			{JobName: new("scheduled-source")},
		},
		Schedule:    new("cron(0/10 * * * ? *)"),
		Description: new("updated"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	triggers, err := c.GetTriggers(ctx, &glue.GetTriggersInput{DependentJobName: new("scheduled-source")})
	if err != nil || len(triggers.Triggers) != 1 {
		t.Fatalf("dependent triggers: %+v %v", triggers, err)
	}

	batch, err := c.BatchGetTriggers(ctx, &glue.BatchGetTriggersInput{TriggerNames: []string{"chain", "missing"}})
	if err != nil || len(batch.Triggers) != 1 || len(batch.TriggersNotFound) != 1 {
		t.Fatalf("batch triggers: %+v %v", batch, err)
	}

	_, err = c.DeleteTrigger(ctx, &glue.DeleteTriggerInput{Name: new("chain")})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGlueWorkflowStopAndBatch(t *testing.T) {
	gluejobPython(t, false)
	ctx := context.Background()
	s := serverWith(t, local.New(local.Options{}))
	c := gluejobSetup(t, s, "workflow-stop", "import time\ntime.sleep(120)\n")
	_, err := c.CreateWorkflow(ctx, &glue.CreateWorkflowInput{Name: new("stop-flow")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("stop-start"), Type: gt.TriggerTypeOnDemand,
		WorkflowName: new("stop-flow"), Actions: []gt.Action{{JobName: new("workflow-stop")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := c.StartWorkflowRun(ctx, &glue.StartWorkflowRunInput{Name: new("stop-flow")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StopWorkflowRun(ctx, &glue.StopWorkflowRunInput{Name: new("stop-flow"), RunId: run.RunId})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stopped Glue workflow", func() bool {
		out, getErr := c.GetWorkflowRun(ctx, &glue.GetWorkflowRunInput{Name: new("stop-flow"), RunId: run.RunId})
		return getErr == nil && out.Run.Status == gt.WorkflowRunStatusStopped && out.Run.Statistics.StoppedActions == 1
	})
	jobs, err := c.BatchGetJobs(ctx, &glue.BatchGetJobsInput{JobNames: []string{"workflow-stop", "missing"}})
	if err != nil || len(jobs.Jobs) != 1 || len(jobs.JobsNotFound) != 1 {
		t.Fatalf("batch jobs: %+v %v", jobs, err)
	}

	flows, err := c.BatchGetWorkflows(
		ctx,
		&glue.BatchGetWorkflowsInput{Names: []string{"stop-flow", "missing"}, IncludeGraph: new(true)},
	)
	if err != nil || len(flows.Workflows) != 1 || len(flows.MissingWorkflows) != 1 {
		t.Fatalf("batch workflows: %+v %v", flows, err)
	}

	runs, err := c.GetWorkflowRuns(ctx, &glue.GetWorkflowRunsInput{Name: new("stop-flow")})
	if err != nil || len(runs.Runs) != 1 {
		t.Fatalf("workflow runs: %+v %v", runs, err)
	}

	arn := "arn:aws:glue:us-east-1:000000000000:workflow/stop-flow"
	tags, err := taggingClient(s, "us-east-1").TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{arn}, Tags: map[string]string{"inventory": "yes"},
	})
	if err != nil || len(tags.FailedResourcesMap) != 0 {
		t.Fatalf("resource tagging: %+v %v", tags, err)
	}

	_, err = c.DeleteWorkflow(ctx, &glue.DeleteWorkflowInput{Name: new("stop-flow")})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGlueJobsCloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := gluejobSetup(t, s, "cfn-role", "print('no execution')\n")
	stacks := cfnClient(s)
	const template = `{"Resources":{
 "Flow":{"Type":"AWS::Glue::Workflow","Properties":{"Name":"cfn-flow","Description":"%s","Tags":{"team":"%s"}}},
 "Job":{"Type":"AWS::Glue::Job","Properties":{"Name":"cfn-job","Role":"cfn-role",
 "Command":{"Name":"pythonshell","ScriptLocation":"s3://cfn-role/job.py"},"Description":"%s","Tags":{"team":"%s"}}},
 "Trigger":{"Type":"AWS::Glue::Trigger","Properties":{"Name":"cfn-start","Type":"ON_DEMAND",
 "WorkflowName":{"Ref":"Flow"},"Actions":[{"JobName":{"Ref":"Job"}}],"Tags":{"team":"%s"}}}}}`
	_, err := stacks.CreateStack(ctx, &cloudformation.CreateStackInput{
		StackName:    new("glue-stack"),
		TemplateBody: new(fmt.Sprintf(template, "core", "core", "core", "core", "core")),
	})
	if err != nil {
		t.Fatal(err)
	}

	if stack := settled(ctx, t, stacks, "glue-stack"); string(stack.StackStatus) != "CREATE_COMPLETE" {
		t.Fatalf("create: %+v", stack)
	}

	_, err = stacks.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName:    new("glue-stack"),
		TemplateBody: new(fmt.Sprintf(template, "platform", "platform", "platform", "platform", "platform")),
	})
	if err != nil {
		t.Fatal(err)
	}

	if stack := settled(ctx, t, stacks, "glue-stack"); string(stack.StackStatus) != "UPDATE_COMPLETE" {
		t.Fatalf("update: %+v", stack)
	}

	job, err := c.GetJob(ctx, &glue.GetJobInput{JobName: new("cfn-job")})
	if err != nil || *job.Job.Description != "platform" {
		t.Fatalf("job update: %+v %v", job, err)
	}

	for _, kind := range []string{"job/cfn-job", "trigger/cfn-start", "workflow/cfn-flow"} {
		tags, getErr := c.GetTags(
			ctx,
			&glue.GetTagsInput{ResourceArn: new("arn:aws:glue:us-east-1:000000000000:" + kind)},
		)
		if getErr != nil || tags.Tags["team"] != "platform" {
			t.Fatalf("CFN tags: %+v %v", tags, getErr)
		}
	}

	_, err = stacks.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: new("glue-stack")})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGlueJobDefinitionCombinations(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := gluejobSetup(t, s, "glue-validation", "print('definition')\n")
	for _, test := range []struct {
		capacity                                *float64
		workers                                 *int32
		name, command, version, worker, runtime string
		invalid                                 bool
	}{
		{name: "shell-capacity", command: "pythonshell", capacity: new(2.0), invalid: true},
		{name: "shell-workers", command: "pythonshell", worker: "G.1X", workers: new(int32(2)), invalid: true},
		{name: "modern-capacity", command: "glueetl", version: "3.0", capacity: new(10.0), invalid: true},
		{
			name:     "mixed-capacity",
			command:  "glueetl",
			version:  "1.0",
			capacity: new(10.0),
			worker:   "G.1X",
			workers:  new(int32(2)),
			invalid:  true,
		},
		{name: "fractional-spark", command: "glueetl", version: "1.0", capacity: new(2.5), invalid: true},
		{
			name:    "standard-modern",
			command: "glueetl",
			version: "3.0",
			worker:  "Standard",
			workers: new(int32(2)),
			invalid: true,
		},
		{
			name:    "stream-small-old",
			command: "gluestreaming",
			version: "2.0",
			worker:  "G.025X",
			workers: new(int32(2)),
			invalid: true,
		},
		{name: "ray-worker", command: "glueray", version: "4.0", worker: "G.1X", workers: new(int32(2)), invalid: true},
		{name: "ray-old", command: "glueray", version: "3.0", worker: "Z.2X", workers: new(int32(2)), invalid: true},
		{name: "shell-ok", command: "pythonshell", capacity: new(0.0625)},
		{name: "spark-ok", command: "glueetl", version: "3.0", worker: "G.1X", workers: new(int32(2))},
		{name: "ray-ok", command: "glueray", version: "4.0", worker: "Z.2X", workers: new(int32(2)), runtime: "Ray2.4"},
		{name: "ray-new", command: "glueray", version: "5.0", worker: "Z.2X", workers: new(int32(2)), runtime: "Ray2.4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := &glue.CreateJobInput{
				Name: new(test.name),
				Role: new("arn:aws:iam::000000000000:role/glue-validation"),
				Command: &gt.JobCommand{
					Name:           new(test.command),
					ScriptLocation: new("s3://glue-validation/job.py"),
				},
				MaxCapacity:     test.capacity,
				NumberOfWorkers: test.workers,
			}
			if test.version != "" {
				input.GlueVersion = new(test.version)
			}

			if test.worker != "" {
				input.WorkerType = gt.WorkerType(test.worker)
			}

			if test.runtime != "" {
				input.Command.Runtime = new(test.runtime)
			}

			_, err := c.CreateJob(ctx, input)
			if test.invalid {
				code(t, err, "InvalidInputException")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, name := range []string{"folder/job", "percent%job"} {
		_, err := c.CreateJob(ctx, &glue.CreateJobInput{
			Name: new(name), Role: new("glue-validation"),
			Command: &gt.JobCommand{Name: new("pythonshell"), ScriptLocation: new("s3://glue-validation/job.py")},
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.GetJob(ctx, &glue.GetJobInput{JobName: new(name)})
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	var token *string
	for {
		page, err := c.ListJobs(ctx, &glue.ListJobsInput{NextToken: token, MaxResults: new(int32(1))})
		if err != nil {
			t.Fatal(err)
		}

		for _, name := range page.JobNames {
			if seen[name] {
				t.Fatalf("duplicate paginated name %q", name)
			}

			seen[name] = true
		}

		token = page.NextToken
		if token == nil {
			break
		}
	}

	if len(seen) != 7 {
		t.Fatalf("expected seven stored job definitions, got %v", seen)
	}
}

func TestGlueWorkflowScheduledAndTriggerStarts(t *testing.T) {
	gluejobPython(t, false)
	ctx := context.Background()
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), local.New(local.Options{}), store)
	defer stop()
	c := gluejobSetup(t, s, "workflow-scheduled-first", "print('workflow source')\n")
	gluejobSetup(t, s, "workflow-scheduled-second", "print('workflow target')\n")
	_, err := c.CreateWorkflow(ctx, &glue.CreateWorkflowInput{
		Name:                 new("scheduled-flow"),
		DefaultRunProperties: map[string]string{"scheduled": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("workflow-schedule"), Type: gt.TriggerTypeScheduled,
		WorkflowName: new("scheduled-flow"), StartOnCreation: true, Schedule: new("cron(0/5 * * * ? *)"),
		Actions: []gt.Action{{JobName: new("workflow-scheduled-first")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("workflow-schedule-then"),
		Type: gt.TriggerTypeConditional,
		WorkflowName: new(
			"scheduled-flow",
		),
		StartOnCreation: true,
		Actions:         []gt.Action{{JobName: new("workflow-scheduled-second")}},
		Predicate: &gt.Predicate{
			Conditions: []gt.Condition{{JobName: new("workflow-scheduled-first"), State: gt.JobRunStateSucceeded}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an overdue persisted schedule while keeping AWS's five-minute precision.
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		key := "000000000000/us-east-1/glue/trigger/workflow-schedule"
		record, _, readErr := storage.Read[map[string]json.RawMessage](tx, key)
		if readErr != nil {
			return readErr
		}

		(*record)["Next"] = json.RawMessage("1")
		return storage.Write(tx, key, record)
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "scheduled workflow chain", func() bool {
		runs, getErr := c.GetWorkflowRuns(ctx, &glue.GetWorkflowRunsInput{Name: new("scheduled-flow")})
		return getErr == nil && len(runs.Runs) == 1 && runs.Runs[0].Status == gt.WorkflowRunStatusCompleted &&
			runs.Runs[0].Statistics.SucceededActions == 2
	})
	jobs, err := c.GetJobRuns(ctx, &glue.GetJobRunsInput{JobName: new("workflow-scheduled-first")})
	if err != nil || len(jobs.JobRuns) != 1 || jobs.JobRuns[0].Arguments["--WORKFLOW_NAME"] != "scheduled-flow" {
		t.Fatalf("workflow job context: %+v %v", jobs, err)
	}

	_, err = c.CreateWorkflow(ctx, &glue.CreateWorkflowInput{Name: new("trigger-flow")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name: new("workflow-on-demand"), Type: gt.TriggerTypeOnDemand,
		WorkflowName: new("trigger-flow"), Actions: []gt.Action{{JobName: new("workflow-scheduled-first")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartTrigger(ctx, &glue.StartTriggerInput{Name: new("workflow-on-demand")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "trigger-started workflow", func() bool {
		runs, getErr := c.GetWorkflowRuns(ctx, &glue.GetWorkflowRunsInput{Name: new("trigger-flow")})
		return getErr == nil && len(runs.Runs) == 1 && runs.Runs[0].Status == gt.WorkflowRunStatusCompleted
	})
}

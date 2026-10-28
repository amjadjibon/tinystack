package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	ecsdocker "github.com/amjadjibon/tinystack/internal/services/ecs/docker"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func submitBatch(t *testing.T, c *sdk.Client, name string, overrides *types.ContainerOverrides,
	deps []types.JobDependency, array, retries, timeout int32,
) string {
	t.Helper()
	in := &sdk.SubmitJobInput{
		JobName: new(name), JobQueue: new("queue"), JobDefinition: new("worker"),
		ContainerOverrides: overrides, DependsOn: deps, Parameters: map[string]string{"input": "submitted"},
	}

	if array > 0 {
		in.ArrayProperties = &types.ArrayProperties{Size: new(array)}
	}

	if retries > 0 {
		in.RetryStrategy = &types.RetryStrategy{Attempts: new(retries)}
	}

	if timeout > 0 {
		in.Timeout = &types.JobTimeout{AttemptDurationSeconds: new(timeout)}
	}

	out, err := c.SubmitJob(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	return *out.JobId
}

func describeBatch(t *testing.T, c *sdk.Client, id string) types.JobDetail {
	t.Helper()
	out, err := c.DescribeJobs(context.Background(), &sdk.DescribeJobsInput{Jobs: []string{id}})
	if err != nil || len(out.Jobs) != 1 {
		t.Fatalf("describe %s: %+v %v", id, out, err)
	}

	return out.Jobs[0]
}

func waitBatch(t *testing.T, c *sdk.Client, id string, status types.JobStatus) types.JobDetail {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		job := describeBatch(t, c, id)
		if job.Status == status {
			return job
		}

		if job.Status == types.JobStatusFailed || job.Status == types.JobStatusSucceeded {
			t.Fatalf("job %s: %+v attempts=%+v", id, job, job.Attempts)
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("job %s did not reach %s", id, status)
	return types.JobDetail{}
}

func batchLogs(t *testing.T, s *httptest.Server, j *types.JobDetail) string {
	t.Helper()
	out, err := logsClient(s, "test").GetLogEvents(context.Background(), &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: new("/aws/batch/job"), LogStreamName: j.Container.LogStreamName, StartFromHead: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	messages := []string{}
	for _, e := range out.Events {
		messages = append(messages, *e.Message)
	}

	return strings.Join(messages, "\n")
}

func TestBatchJobsRecordsAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	_, _, _ = batchFixture(t, s)
	c := batchClient(s, "test", "us-east-1")
	first := submitBatch(t, c, "first", nil, nil, 0, 0, 0)
	second := submitBatch(t, c, "dependent", nil, []types.JobDependency{{JobId: new(first)}}, 0, 0, 0)
	array := submitBatch(t, c, "array", nil, []types.JobDependency{{Type: types.ArrayJobDependencySequential}}, 3, 0, 0)
	if job := describeBatch(
		t,
		c,
		first,
	); job.Status != types.JobStatusRunnable ||
		job.Container.Command[1] != "submitted" {
		t.Fatal(job)
	}

	if describeBatch(t, c, second).Status != types.JobStatusPending {
		t.Fatal("dependency not pending")
	}

	list, err := c.ListJobs(ctx, &sdk.ListJobsInput{ArrayJobId: new(array)})
	if err != nil || len(list.JobSummaryList) != 3 {
		t.Fatal(list, err)
	}

	for i := range 3 {
		want := types.JobStatusPending
		if i == 0 {
			want = types.JobStatusRunnable
		}

		if job := describeBatch(t, c, fmt.Sprintf("%s:%d", array, i)); job.Status != want {
			t.Fatal(job)
		}
	}

	// Cursor is scoped to the query, not its page size.
	_ = submitBatch(t, c, "another", nil, nil, 0, 0, 0)
	for name, values := range map[string][]string{
		"JOB_NAME":         {"FIRST"},
		"JOB_DEFINITION":   {"wor*"},
		"AFTER_CREATED_AT": {"0"},
	} {
		result, e := c.ListJobs(
			ctx,
			&sdk.ListJobsInput{
				JobQueue: new("queue"),
				Filters:  []types.KeyValuesPair{{Name: new(name), Values: values}},
			},
		)
		if e != nil {
			t.Fatal(e)
		}

		want := 4
		if name == "JOB_NAME" {
			want = 1
		}

		if len(result.JobSummaryList) != want {
			t.Fatalf("filter %s: %+v", name, result)
		}
	}

	nton := submitBatch(
		t,
		c,
		"nton",
		nil,
		[]types.JobDependency{{JobId: new(array), Type: types.ArrayJobDependencyNToN}},
		3,
		0,
		0,
	)
	if describeBatch(t, c, nton+":0").Status != types.JobStatusPending {
		t.Fatal("N_TO_N did not wait")
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: describeBatch(t, c, first).JobArn, Tags: map[string]string{"job": "tagged"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if describeBatch(t, c, first).Tags["job"] != "tagged" {
		t.Fatal("job tag was not reflected")
	}

	page, err := c.ListJobs(
		ctx,
		&sdk.ListJobsInput{JobQueue: new("queue"), JobStatus: types.JobStatusRunnable, MaxResults: new(int32(1))},
	)
	if err != nil || page.NextToken == nil || len(page.JobSummaryList) != 1 {
		t.Fatal(page, err)
	}

	next, err := c.ListJobs(
		ctx,
		&sdk.ListJobsInput{JobQueue: new("queue"), JobStatus: types.JobStatusRunnable, NextToken: page.NextToken},
	)
	if err != nil || len(next.JobSummaryList) != 1 || *next.JobSummaryList[0].JobId == *page.JobSummaryList[0].JobId {
		t.Fatal(next, err)
	}

	_, err = c.ListJobs(
		ctx,
		&sdk.ListJobsInput{JobQueue: new("queue"), JobStatus: types.JobStatusPending, NextToken: page.NextToken},
	)
	code(t, err, "ClientException")
	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{JobId: new(second), Reason: new("cancel later")})
	if err != nil {
		t.Fatal(err)
	}

	if describeBatch(t, c, second).Status != types.JobStatusPending {
		t.Fatal("pending cancellation did not wait")
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = batchClient(s, "test", "us-east-1")
	if describeBatch(t, c, first).Status != types.JobStatusRunnable {
		t.Fatal("restart lost runnable job")
	}

	_, err = c.TerminateJob(ctx, &sdk.TerminateJobInput{JobId: new(first), Reason: new("terminated")})
	if err != nil {
		t.Fatal(err)
	}

	if job := describeBatch(t, c, second); job.Status != types.JobStatusFailed || *job.StatusReason != "cancel later" {
		t.Fatal(job)
	}

	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{JobId: new(array), Reason: new("array cancel")})
	if err != nil {
		t.Fatal(err)
	}

	if job := describeBatch(
		t,
		c,
		array,
	); job.Status != types.JobStatusFailed ||
		job.ArrayProperties.StatusSummary["FAILED"] != 3 {
		t.Fatal(job)
	}

	missing, err := c.DescribeJobs(ctx, &sdk.DescribeJobsInput{Jobs: []string{"absent"}})
	if err != nil || len(missing.Jobs) != 0 {
		t.Fatal(missing, err)
	}

	for _, body := range []string{
		`{}`, `{"jobName":"x","jobQueue":"queue","jobDefinition":"worker","retryStrategy":{"attempts":11}}`,
		`{"jobName":"x","jobQueue":"queue","jobDefinition":"worker","timeout":{"attemptDurationSeconds":1}}`,
		`{"jobName":"x","jobQueue":"queue","jobDefinition":"worker","arrayProperties":{"size":1}}`,
		`{"jobName":"x","jobQueue":"queue","jobDefinition":"worker","dependsOn":[{"jobId":"absent"}]}`,
	} {
		status, out := batchWire(t, s, "POST", "/v1/submitjob", body)
		if status != 400 || out["__type"] != "ClientException" {
			t.Fatal(status, out)
		}
	}
}

func batchDockerServer(t *testing.T) (*httptest.Server, func(), config.Config, *sqlite.Store) {
	t.Helper()
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for Batch containers")
	}

	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { store.Close() })
	cfg := testConfig()
	cfg.Batch.Jobs = config.ContainersDocker
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	batchFixture(t, s)
	c := batchClient(s, "test", "us-east-1")
	def := batchDefinition()
	def.ContainerProperties.Image = new("busybox:1.37.0")
	def.ContainerProperties.JobRoleArn = nil
	def.ContainerProperties.ExecutionRoleArn = nil
	_, err = c.RegisterJobDefinition(context.Background(), def)
	if err != nil {
		t.Fatal(err)
	}

	return s, stop, cfg, store
}

func TestBatchDockerJobsLogsRetriesArraysDependencies(t *testing.T) {
	s, stop, _, _ := batchDockerServer(t)
	defer stop()
	c := batchClient(s, "test", "us-east-1")
	first := submitBatch(t, c, "success", nil, nil, 0, 0, 0)
	j := waitBatch(t, c, first, types.JobStatusSucceeded)
	if got := batchLogs(t, s, new(j)); !strings.Contains(got, "submitted") {
		t.Fatal(got)
	}

	failed := submitBatch(
		t,
		c,
		"retry",
		&types.ContainerOverrides{Command: []string{"sh", "-c", "echo attempt=$AWS_BATCH_JOB_ATTEMPT; exit 7"}},
		nil,
		0,
		2,
		0,
	)
	j = waitBatch(t, c, failed, types.JobStatusFailed)
	if len(j.Attempts) != 2 || *j.Attempts[0].Container.ExitCode != 7 || *j.Attempts[1].Container.ExitCode != 7 {
		t.Fatal(j)
	}

	if !strings.Contains(batchLogs(t, s, new(j)), "attempt=2") {
		t.Fatal("missing retry environment")
	}

	exitRule, err := c.SubmitJob(context.Background(), &sdk.SubmitJobInput{
		JobName:            new("exit-rule"),
		JobQueue:           new("queue"),
		JobDefinition:      new("worker"),
		ContainerOverrides: &types.ContainerOverrides{Command: []string{"sh", "-c", "exit 4"}},
		RetryStrategy: &types.RetryStrategy{
			Attempts:       new(int32(3)),
			EvaluateOnExit: []types.EvaluateOnExit{{Action: types.RetryActionExit, OnExitCode: new("4")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if job := waitBatch(t, c, *exitRule.JobId, types.JobStatusFailed); len(job.Attempts) != 1 {
		t.Fatal(job)
	}

	array := submitBatch(
		t,
		c,
		"array",
		&types.ContainerOverrides{
			Command: []string{"sh", "-c", "echo $AWS_BATCH_JOB_ARRAY_INDEX/$AWS_BATCH_JOB_ARRAY_SIZE"},
		},
		nil,
		3,
		0,
		0,
	)
	waitBatch(t, c, array, types.JobStatusSucceeded)
	for i := range 3 {
		j = describeBatch(t, c, fmt.Sprintf("%s:%d", array, i))
		if got := batchLogs(t, s, new(j)); got != fmt.Sprintf("%d/3", i) {
			t.Fatal(got)
		}
	}

	dependent := submitBatch(t, c, "dependent", nil, []types.JobDependency{{JobId: new(array)}}, 0, 0, 0)
	j = waitBatch(t, c, dependent, types.JobStatusSucceeded)
	if *j.StartedAt < *describeBatch(t, c, array+":2").StoppedAt {
		t.Fatal("dependency ran early")
	}
}

func TestBatchDockerTimeoutTerminateCapacityAndRestart(t *testing.T) {
	s, stop, cfg, store := batchDockerServer(t)
	t.Cleanup(func() { stop() })
	c := batchClient(s, "test", "us-east-1")
	ctx := context.Background()
	_, err := c.UpdateComputeEnvironment(ctx, &sdk.UpdateComputeEnvironmentInput{
		ComputeEnvironment: new("managed"),
		ComputeResources:   &types.ComputeResourceUpdate{MaxvCpus: new(int32(4))},
	})
	if err != nil {
		t.Fatal(err)
	}

	sleep := &types.ContainerOverrides{Command: []string{"sh", "-c", "echo started; sleep 600"}}
	running := submitBatch(t, c, "running", sleep, nil, 0, 0, 0)
	waitBatch(t, c, running, types.JobStatusRunning)
	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{JobId: new(running), Reason: new("too late")})
	if err != nil || describeBatch(t, c, running).Status != types.JobStatusRunning {
		t.Fatal(err)
	}

	second := submitBatch(t, c, "timeout", sleep, nil, 0, 3, 60)
	waitBatch(t, c, second, types.JobStatusRunning)
	third := submitBatch(t, c, "capacity", nil, nil, 0, 0, 0)
	time.Sleep(1500 * time.Millisecond)
	if describeBatch(t, c, third).Status != types.JobStatusRunnable {
		t.Fatal("maxvCpus exceeded")
	}

	_, err = c.TerminateJob(ctx, &sdk.TerminateJobInput{JobId: new(running), Reason: new("test termination")})
	if err != nil {
		t.Fatal(err)
	}

	waitBatch(t, c, running, types.JobStatusFailed)
	waitBatch(t, c, third, types.JobStatusSucceeded)
	j := waitBatch(t, c, second, types.JobStatusFailed)
	if *j.StatusReason != "Job attempt duration exceeded timeout" || len(j.Attempts) != 1 {
		t.Fatal(j)
	}

	interrupted := submitBatch(t, c, "restart", sleep, nil, 0, 2, 0)
	waitBatch(t, c, interrupted, types.JobStatusRunning)
	var owner string
	err = store.View(ctx, func(tx storage.ReadTx) error {
		value, ok, e := storage.Read[string](tx, "aws/batch/runtime-owner")
		if ok {
			owner = *value
		}

		return e
	})
	if err != nil || owner == "" {
		t.Fatal(owner, err)
	}

	orphanRuntime := ecsdocker.New()
	t.Cleanup(orphanRuntime.Close)
	orphans := []string{}
	for _, storeOwner := range []string{owner, owner + "-other"} {
		started, e := orphanRuntime.Start(ctx, &ecs.ContainerSpec{
			Image: "busybox:1.37.0", Command: []string{"sleep", "600"},
			Labels: map[string]string{"tinystack.batch.job": "orphan-test", "tinystack.batch.store": storeOwner},
		})
		if e != nil {
			t.Fatal(e)
		}

		orphans = append(orphans, started.ID)
	}

	stop()
	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	if _, e := dockercli.Run(ctx, nil, "inspect", orphans[0]); e == nil {
		t.Fatal("owned orphan survived restart")
	}

	if _, e := dockercli.Run(ctx, nil, "inspect", orphans[1]); e != nil {
		t.Fatal("startup removed another store's container", e)
	}

	c = batchClient(s, "test", "us-east-1")
	j = waitBatch(t, c, interrupted, types.JobStatusRunning)
	if len(j.Attempts) != 1 || !strings.Contains(*j.Attempts[0].StatusReason, "restarted") {
		t.Fatal(j)
	}

	_, err = c.TerminateJob(ctx, &sdk.TerminateJobInput{JobId: new(interrupted), Reason: new("done")})
	if err != nil {
		t.Fatal(err)
	}

	waitBatch(t, c, interrupted, types.JobStatusFailed)
	resetJob := submitBatch(t, c, "reset", sleep, nil, 0, 0, 0)
	waitBatch(t, c, resetJob, types.JobStatusRunning)
	status, body := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatal(status, string(body))
	}

	out, e := c.DescribeJobs(ctx, &sdk.DescribeJobsInput{Jobs: []string{resetJob}})
	if e != nil || len(out.Jobs) != 0 {
		t.Fatal(out, e)
	}

	stop()
	owned, err := dockercli.ListLabeled(ctx, "tinystack.batch.job")
	if err != nil {
		t.Fatal(err)
	}

	for _, arn := range owned {
		if strings.Contains(arn, interrupted) {
			t.Fatal("container survived shutdown")
		}
	}
}

func TestBatchJobsStateChangeEvents(t *testing.T) {
	s := server(t)
	batchFixture(t, s)
	c := batchClient(s, "test", "us-east-1")
	events := eventsClient(s, "test")
	queues := sqsClient(s, "test")
	queueURL, queue := queueARN(t, queues, "batch-events")
	putRule(t, events, "batch-events", `{"source":["aws.batch"],"detail-type":["Batch Job State Change"]}`)
	_, err := events.PutTargets(context.Background(), &eventbridge.PutTargetsInput{
		Rule: new("batch-events"), Targets: []ebtypes.Target{{Id: new("queue"), Arn: new(queue)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := submitBatch(t, c, "event", nil, nil, 0, 0, 0)
	waitEvent := func(status string) {
		t.Helper()
		waitFor(t, "Batch "+status+" event", func() bool {
			messages, e := queues.ReceiveMessage(
				context.Background(),
				&sqs.ReceiveMessageInput{QueueUrl: new(queueURL), MaxNumberOfMessages: 10},
			)
			if e != nil {
				t.Fatal(e)
			}

			for _, message := range messages.Messages {
				var event struct {
					Detail struct {
						JobID  string `json:"jobId"`
						Status string `json:"status"`
					} `json:"detail"`
				}

				if e := json.Unmarshal([]byte(*message.Body), &event); e != nil {
					t.Fatal(e)
				}

				if event.Detail.JobID == id && event.Detail.Status == status {
					return true
				}
			}

			return false
		})
	}

	waitEvent("RUNNABLE")
	_, err = c.CancelJob(context.Background(), &sdk.CancelJobInput{JobId: new(id), Reason: new("event test")})
	if err != nil {
		t.Fatal(err)
	}

	waitEvent("FAILED")
}

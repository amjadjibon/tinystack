package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/iotjobsdataplane"
	jobstypes "github.com/aws/aws-sdk-go-v2/service/iotjobsdataplane/types"
)

func jobsClient(s *httptest.Server) *iotjobsdataplane.Client {
	return iotjobsdataplane.New(iotjobsdataplane.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func TestIoTJobs(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	jobs := jobsClient(s)
	for _, name := range []string{"pump-1", "pump-2", "pump-3"} {
		if _, err := c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String(name)}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.CreateThingGroup(ctx, &iot.CreateThingGroupInput{ThingGroupName: aws.String("pumps")}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"pump-2", "pump-3"} {
		if _, err := c.AddThingToThingGroup(ctx, &iot.AddThingToThingGroupInput{
			ThingName: aws.String(name), ThingGroupName: aws.String("pumps"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A device hears about new jobs on notify-next.
	device := connected(t, mqttURL(s), "pump-1")
	device.subscribe("$aws/things/pump-1/jobs/#")

	thingARN := "arn:aws:iot:us-east-1:000000000000:thing/pump-1"
	groupARN := "arn:aws:iot:us-east-1:000000000000:thinggroup/pumps"
	created, err := c.CreateJob(ctx, &iot.CreateJobInput{
		JobId: aws.String("firmware-1"), Targets: []string{thingARN, groupARN},
		Document: aws.String(`{"operation":"update","version":"2.0"}`), Description: aws.String("firmware"),
	})
	if err != nil || aws.ToString(created.JobArn) != "arn:aws:iot:us-east-1:000000000000:job/firmware-1" {
		t.Fatalf("created = %+v, %v", created, err)
	}

	var next struct {
		Execution struct {
			JobID       string `json:"jobId"`
			Status      string `json:"status"`
			JobDocument string `json:"jobDocument"`
		} `json:"execution"`
	}
	if err = json.Unmarshal(device.message("$aws/things/pump-1/jobs/notify-next").Payload, &next); err != nil ||
		next.Execution.JobID != "firmware-1" || next.Execution.Status != "QUEUED" ||
		next.Execution.JobDocument == "" {
		t.Fatalf("notify-next = %+v, %v", next, err)
	}

	_, err = c.CreateJob(ctx, &iot.CreateJobInput{
		JobId: aws.String("firmware-1"), Targets: []string{thingARN}, Document: aws.String(`{}`),
	})
	wantCode(t, err, "ResourceAlreadyExistsException")
	_, err = c.CreateJob(ctx, &iot.CreateJobInput{
		JobId: aws.String("bad"), Targets: []string{thingARN}, Document: aws.String(`{}`),
		TargetSelection: iottypes.TargetSelectionContinuous,
	})
	wantCode(t, err, "InvalidRequestException")
	_, err = c.CreateJob(ctx, &iot.CreateJobInput{
		JobId: aws.String("bad"), Targets: []string{"arn:aws:iot:us-east-1:000000000000:thing/missing"},
		Document: aws.String(`{}`),
	})
	wantCode(t, err, "ResourceNotFoundException")

	described, err := c.DescribeJob(ctx, &iot.DescribeJobInput{JobId: aws.String("firmware-1")})
	if err != nil || described.Job.Status != iottypes.JobStatusInProgress ||
		aws.ToInt32(described.Job.JobProcessDetails.NumberOfQueuedThings) != 3 {
		t.Fatalf("described = %+v, %v", described, err)
	}

	doc, err := c.GetJobDocument(ctx, &iot.GetJobDocumentInput{JobId: aws.String("firmware-1")})
	if err != nil || aws.ToString(doc.Document) != `{"operation":"update","version":"2.0"}` {
		t.Fatalf("document = %+v, %v", doc, err)
	}

	// The jobs data plane lists, starts, and updates a thing's executions.
	pending, err := jobs.GetPendingJobExecutions(ctx, &iotjobsdataplane.GetPendingJobExecutionsInput{
		ThingName: aws.String("pump-2"),
	})
	if err != nil || len(pending.QueuedJobs) != 1 || len(pending.InProgressJobs) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}

	started, err := jobs.StartNextPendingJobExecution(ctx, &iotjobsdataplane.StartNextPendingJobExecutionInput{
		ThingName: aws.String("pump-2"), StatusDetails: map[string]string{"step": "download"},
	})
	if err != nil || started.Execution.Status != jobstypes.JobExecutionStatusInProgress ||
		aws.ToString(started.Execution.JobDocument) == "" || started.Execution.VersionNumber != 2 {
		t.Fatalf("started = %+v, %v", started, err)
	}

	_, err = jobs.UpdateJobExecution(ctx, &iotjobsdataplane.UpdateJobExecutionInput{
		ThingName: aws.String("pump-2"), JobId: aws.String("firmware-1"),
		Status: jobstypes.JobExecutionStatusSucceeded, ExpectedVersion: aws.Int64(1),
	})
	wantCode(t, err, "VersionConflictException")
	updated, err := jobs.UpdateJobExecution(ctx, &iotjobsdataplane.UpdateJobExecutionInput{
		ThingName: aws.String("pump-2"), JobId: aws.String("firmware-1"),
		Status: jobstypes.JobExecutionStatusSucceeded, ExpectedVersion: aws.Int64(2),
		IncludeJobExecutionState: aws.Bool(true),
	})
	if err != nil || updated.ExecutionState.Status != jobstypes.JobExecutionStatusSucceeded {
		t.Fatalf("updated = %+v, %v", updated, err)
	}

	_, err = jobs.UpdateJobExecution(ctx, &iotjobsdataplane.UpdateJobExecutionInput{
		ThingName: aws.String("pump-2"), JobId: aws.String("firmware-1"), Status: jobstypes.JobExecutionStatusFailed,
	})
	wantCode(t, err, "InvalidStateTransitionException")

	// Over MQTT, the device starts its next job and reports it done.
	if code := device.publish("$aws/things/pump-1/jobs/start-next", `{"clientToken":"c1"}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	var accepted map[string]any
	if err = json.Unmarshal(
		device.message("$aws/things/pump-1/jobs/start-next/accepted").Payload,
		&accepted,
	); err != nil ||
		accepted["clientToken"] != "c1" ||
		at(accepted, "execution.status") != "IN_PROGRESS" {
		t.Fatalf("start-next accepted = %v, %v", accepted, err)
	}

	if code := device.publish("$aws/things/pump-1/jobs/firmware-1/update",
		`{"status":"SUCCEEDED","expectedVersion":7,"clientToken":"c2"}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	var rejected map[string]any
	if err = json.Unmarshal(
		device.message("$aws/things/pump-1/jobs/firmware-1/update/rejected").Payload,
		&rejected,
	); err != nil || rejected["code"] != "VersionMismatch" || at(rejected, "executionState.versionNumber") != 2.0 {
		t.Fatalf("rejected = %v, %v", rejected, err)
	}

	if code := device.publish("$aws/things/pump-1/jobs/firmware-1/update",
		`{"status":"SUCCEEDED","includeJobDocument":true}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if err = json.Unmarshal(device.message("$aws/things/pump-1/jobs/firmware-1/update/accepted").Payload,
		&accepted); err != nil || accepted["jobDocument"] == nil {
		t.Fatalf("update accepted = %v, %v", accepted, err)
	}

	if code := device.publish("$aws/things/pump-1/jobs/$next/get", `{}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if err = json.Unmarshal(device.message("$aws/things/pump-1/jobs/$next/get/rejected").Payload,
		&rejected); err != nil || rejected["code"] != "ResourceNotFound" {
		t.Fatalf("no next job = %v, %v", rejected, err)
	}

	// Canceling the job cancels the queued execution; the job is canceled.
	list, err := c.ListJobExecutionsForJob(ctx, &iot.ListJobExecutionsForJobInput{JobId: aws.String("firmware-1")})
	if err != nil || len(list.ExecutionSummaries) != 3 {
		t.Fatalf("executions = %+v, %v", list, err)
	}

	if _, err = c.CancelJob(ctx, &iot.CancelJobInput{
		JobId: aws.String("firmware-1"), Comment: aws.String("enough"),
	}); err != nil {
		t.Fatal(err)
	}

	execution, err := c.DescribeJobExecution(ctx, &iot.DescribeJobExecutionInput{
		JobId: aws.String("firmware-1"), ThingName: aws.String("pump-3"),
	})
	if err != nil || execution.Execution.Status != iottypes.JobExecutionStatusCanceled {
		t.Fatalf("pump-3 execution = %+v, %v", execution, err)
	}

	described, err = c.DescribeJob(ctx, &iot.DescribeJobInput{JobId: aws.String("firmware-1")})
	if err != nil || described.Job.Status != iottypes.JobStatusCanceled ||
		aws.ToString(described.Job.Comment) != "enough" ||
		aws.ToInt32(described.Job.JobProcessDetails.NumberOfSucceededThings) != 2 {
		t.Fatalf("canceled = %+v, %v", described, err)
	}

	// A job whose executions all finish completes.
	if _, err = c.CreateJob(ctx, &iot.CreateJobInput{
		JobId: aws.String("reboot"), Targets: []string{thingARN}, Document: aws.String(`{"op":"reboot"}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = jobs.UpdateJobExecution(ctx, &iotjobsdataplane.UpdateJobExecutionInput{
		ThingName: aws.String("pump-1"), JobId: aws.String("reboot"), Status: jobstypes.JobExecutionStatusSucceeded,
	}); err != nil {
		t.Fatal(err)
	}

	jobList, err := c.ListJobs(ctx, &iot.ListJobsInput{Status: iottypes.JobStatusCompleted})
	if err != nil || len(jobList.Jobs) != 1 || aws.ToString(jobList.Jobs[0].JobId) != "reboot" {
		t.Fatalf("completed jobs = %+v, %v", jobList, err)
	}

	forThing, err := c.ListJobExecutionsForThing(ctx, &iot.ListJobExecutionsForThingInput{
		ThingName: aws.String("pump-1"),
	})
	if err != nil || len(forThing.ExecutionSummaries) != 2 {
		t.Fatalf("pump-1 executions = %+v, %v", forThing, err)
	}

	if _, err = c.DeleteJobExecution(ctx, &iot.DeleteJobExecutionInput{
		JobId: aws.String("reboot"), ThingName: aws.String("pump-1"), ExecutionNumber: aws.Int64(1),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteJob(ctx, &iot.DeleteJobInput{JobId: aws.String("firmware-1")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeJob(ctx, &iot.DescribeJobInput{JobId: aws.String("firmware-1")})
	wantCode(t, err, "ResourceNotFoundException")
}

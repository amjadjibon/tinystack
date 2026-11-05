package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/emrserverless"
	"github.com/aws/aws-sdk-go-v2/service/emrserverless/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func emrClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantEMRError(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestEMRServerlessSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := emrClient(s, "test", "us-east-1")
	create := &sdk.CreateApplicationInput{
		Name:         new("analytics"),
		Type:         new("Spark"),
		ReleaseLabel: new("emr-7.0.0"),
		ClientToken:  new("create-analytics"),
		Tags:         map[string]string{"team": "core"},
		InitialCapacity: map[string]types.InitialCapacityConfig{
			"Driver": {
				WorkerCount:         new(int64(1)),
				WorkerConfiguration: &types.WorkerResourceConfig{Cpu: new("2 vCPU"), Memory: new("4 GB")},
			},
		},
		MaximumCapacity: &types.MaximumAllowedResources{
			Cpu:    new("8 vCPU"),
			Memory: new("16 GB"),
			Disk:   new("20 GB"),
		},
		AutoStartConfiguration: &types.AutoStartConfig{Enabled: new(false)},
		NetworkConfiguration: &types.NetworkConfiguration{
			SubnetIds:        []string{"subnet-one"},
			SecurityGroupIds: []string{"sg-one"},
		},
	}

	app, err := c.CreateApplication(ctx, create)
	if err != nil {
		t.Fatal(err)
	}

	repeat, err := c.CreateApplication(ctx, create)
	if err != nil || aws.ToString(repeat.ApplicationId) != aws.ToString(app.ApplicationId) {
		t.Fatal(repeat, err)
	}

	applications, listErr := c.ListApplications(ctx, &sdk.ListApplicationsInput{})
	if listErr != nil || len(applications.Applications) != 1 {
		t.Fatalf("duplicate application: %+v %v", applications, listErr)
	}

	create.Name = new("changed")
	_, err = c.CreateApplication(ctx, create)
	wantEMRError(t, err, "ConflictException")
	got, err := c.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationId: app.ApplicationId})
	if err != nil || got.Application.State != types.ApplicationStateCreated ||
		got.Application.Architecture != types.ArchitectureX8664 ||
		got.Application.MaximumCapacity == nil ||
		len(got.Application.InitialCapacity) != 1 ||
		got.Application.CreatedAt == nil {
		t.Fatal(got, err)
	}

	updated, err := c.UpdateApplication(
		ctx,
		&sdk.UpdateApplicationInput{
			ApplicationId: app.ApplicationId,
			ReleaseLabel:  new("emr-7.1.0"),
			Architecture:  types.ArchitectureArm64,
		},
	)
	if err != nil || updated.Application.Architecture != types.ArchitectureArm64 {
		t.Fatal(updated, err)
	}

	jobInput := &sdk.StartJobRunInput{
		ApplicationId:    app.ApplicationId,
		ClientToken:      new("job-one"),
		ExecutionRoleArn: new("arn:aws:iam::000000000000:role/analytics"),
		JobDriver: &types.JobDriverMemberSparkSubmit{
			Value: types.SparkSubmit{
				EntryPoint:            new("s3://code/job.py"),
				EntryPointArguments:   []string{"one"},
				SparkSubmitParameters: new("--conf spark.executor.cores=2"),
			},
		},
		ConfigurationOverrides: &types.ConfigurationOverrides{
			ApplicationConfiguration: []types.Configuration{
				{Classification: new("spark-defaults"), Properties: map[string]string{"spark.executor.memory": "2g"}},
			},
		},
		Tags: map[string]string{"job": "one"},
		Name: new("first"),
	}

	_, err = c.StartJobRun(ctx, jobInput)
	wantEMRError(t, err, "ConflictException")
	_, err = c.StartApplication(ctx, &sdk.StartApplicationInput{ApplicationId: app.ApplicationId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateApplication(
		ctx,
		&sdk.UpdateApplicationInput{ApplicationId: app.ApplicationId, ReleaseLabel: new("emr-7.2.0")},
	)
	wantEMRError(t, err, "ConflictException")
	job, err := c.StartJobRun(ctx, jobInput)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := c.StartJobRun(ctx, jobInput)
	if err != nil || aws.ToString(replay.JobRunId) != aws.ToString(job.JobRunId) {
		t.Fatal(replay, err)
	}

	jobInput.Name = new("changed")
	_, err = c.StartJobRun(ctx, jobInput)
	wantEMRError(t, err, "ConflictException")
	_, err = c.StopApplication(ctx, &sdk.StopApplicationInput{ApplicationId: app.ApplicationId})
	wantEMRError(t, err, "ConflictException")
	_, err = c.DeleteApplication(ctx, &sdk.DeleteApplicationInput{ApplicationId: app.ApplicationId})
	wantEMRError(t, err, "ConflictException")
	for _, state := range []types.JobRunState{
		types.JobRunStateSubmitted, types.JobRunStatePending, types.JobRunStateScheduled,
		types.JobRunStateRunning, types.JobRunStateSuccess, types.JobRunStateSuccess,
	} {
		j, e := c.GetJobRun(ctx, &sdk.GetJobRunInput{ApplicationId: app.ApplicationId, JobRunId: job.JobRunId})
		if e != nil || j.JobRun.State != state ||
			!strings.Contains(aws.ToString(j.JobRun.StateDetails), "no code executes") ||
			aws.ToString(j.JobRun.ReleaseLabel) != "emr-7.1.0" ||
			j.JobRun.JobDriver == nil ||
			j.JobRun.ConfigurationOverrides == nil {
			t.Fatal(j, e)
		}
	}

	_, err = c.CancelJobRun(ctx, &sdk.CancelJobRunInput{ApplicationId: app.ApplicationId, JobRunId: job.JobRunId})
	wantEMRError(t, err, "ConflictException")
	jobInput.ClientToken = new("job-two")
	second, err := c.StartJobRun(ctx, jobInput)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		_, err = c.CancelJobRun(
			ctx,
			&sdk.CancelJobRunInput{ApplicationId: app.ApplicationId, JobRunId: second.JobRunId},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	cancelled, err := c.GetJobRun(ctx, &sdk.GetJobRunInput{ApplicationId: app.ApplicationId, JobRunId: second.JobRunId})
	if err != nil || cancelled.JobRun.State != types.JobRunStateCancelled {
		t.Fatal(cancelled, err)
	}

	list, err := c.ListJobRuns(ctx, &sdk.ListJobRunsInput{ApplicationId: app.ApplicationId, MaxResults: new(int32(1))})
	if err != nil || len(list.JobRuns) != 1 || list.NextToken == nil {
		t.Fatal(list, err)
	}

	next, err := c.ListJobRuns(
		ctx,
		&sdk.ListJobRunsInput{ApplicationId: app.ApplicationId, MaxResults: new(int32(1)), NextToken: list.NextToken},
	)
	if err != nil || len(next.JobRuns) != 1 || aws.ToString(next.JobRuns[0].Id) == aws.ToString(list.JobRuns[0].Id) ||
		next.NextToken != nil {
		t.Fatal(next, err)
	}

	filtered, err := c.ListJobRuns(
		ctx,
		&sdk.ListJobRunsInput{
			ApplicationId: app.ApplicationId,
			States:        []types.JobRunState{types.JobRunStateCancelled},
		},
	)
	if err != nil || len(filtered.JobRuns) != 1 {
		t.Fatal(filtered, err)
	}

	for _, arn := range []*string{app.Arn, job.Arn} {
		_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: arn, Tags: map[string]string{"env": "dev"}})
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		if e != nil || tags.Tags["env"] != "dev" {
			t.Fatal(tags, e)
		}

		_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"env"}})
		if err != nil {
			t.Fatal(err)
		}
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(app.Arn), aws.ToString(job.Arn)},
			Tags:            map[string]string{"rgta": "works"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	// The public inventory makes execution limits visible for both resource kinds.
	resp, err := http.Get(s.URL + "/_tinystack/resources")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	var inventory any
	if err = json.NewDecoder(resp.Body).Decode(&inventory); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(inventory)
	if err != nil || !strings.Contains(string(raw), `"RecordsOnly":true`) ||
		!strings.Contains(string(raw), `"ExecutionReady":false`) ||
		!strings.Contains(string(raw), aws.ToString(job.JobRunId)) {
		t.Fatal(string(raw), err)
	}

	for _, other := range []*sdk.Client{emrClient(s, "team-b", "us-east-1"), emrClient(s, "test", "us-west-2")} {
		empty, e := other.ListApplications(ctx, &sdk.ListApplicationsInput{})
		if e != nil || len(empty.Applications) != 0 {
			t.Fatal(empty, e)
		}

		_, e = other.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationId: app.ApplicationId})
		wantEMRError(t, e, "ResourceNotFoundException")
		_, e = other.GetJobRun(ctx, &sdk.GetJobRunInput{ApplicationId: app.ApplicationId, JobRunId: job.JobRunId})
		wantEMRError(t, e, "ResourceNotFoundException")
		_, e = other.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: app.Arn})
		wantEMRError(t, e, "ResourceNotFoundException")
	}

	_, err = c.StopApplication(ctx, &sdk.StopApplicationInput{ApplicationId: app.ApplicationId})
	if err != nil {
		t.Fatal(err)
	}

	stopped, err := c.ListApplications(
		ctx,
		&sdk.ListApplicationsInput{States: []types.ApplicationState{types.ApplicationStateStopped}},
	)
	if err != nil || len(stopped.Applications) != 1 {
		t.Fatal(stopped, err)
	}

	_, err = c.DeleteApplication(ctx, &sdk.DeleteApplicationInput{ApplicationId: app.ApplicationId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationId: app.ApplicationId})
	wantEMRError(t, err, "ResourceNotFoundException")
	_, err = c.GetJobRun(ctx, &sdk.GetJobRunInput{ApplicationId: app.ApplicationId, JobRunId: job.JobRunId})
	wantEMRError(t, err, "ResourceNotFoundException")
}

func TestEMRServerlessValidationAndHive(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := emrClient(s, "test", "us-east-1")
	for _, input := range []*sdk.CreateApplicationInput{
		{Type: new("Spark"), ReleaseLabel: new("latest")},
		{Type: new("Unsupported"), ReleaseLabel: new("emr-7.0.0")},
		{Type: new("Spark"), ReleaseLabel: new("emr-7.0.0"), Architecture: types.Architecture("bad")},
		{Type: new("Spark"), ReleaseLabel: new("emr-7.0.0"), Tags: map[string]string{"aws:reserved": "x"}},
		{
			Type: new("Spark"), ReleaseLabel: new("emr-7.0.0"),
			MaximumCapacity: &types.MaximumAllowedResources{Cpu: new("bad"), Memory: new("1 GB")},
		},
	} {
		_, err := c.CreateApplication(ctx, input)
		wantEMRError(t, err, "ValidationException")
	}

	_, err := c.CreateApplication(
		ctx,
		&sdk.CreateApplicationInput{
			Type:               new("Spark"),
			ReleaseLabel:       new("emr-7.0.0"),
			ImageConfiguration: &types.ImageConfigurationInput{ImageUri: new("example/image")},
		},
	)
	wantEMRError(t, err, "NotImplemented")
	hive, err := c.CreateApplication(
		ctx,
		&sdk.CreateApplicationInput{Type: new("Hive"), ReleaseLabel: new("emr-7.0.0")},
	)
	if err != nil {
		t.Fatal(err)
	}

	jobIn := &sdk.StartJobRunInput{
		ApplicationId:    hive.ApplicationId,
		ExecutionRoleArn: new("bad"),
		JobDriver:        &types.JobDriverMemberHive{Value: types.Hive{Query: new("s3://queries/query.sql")}},
	}

	_, err = c.StartJobRun(ctx, jobIn)
	wantEMRError(t, err, "ValidationException")
	jobIn.ExecutionRoleArn = new("arn:aws:iam::000000000000:role/hive")
	job, err := c.StartJobRun(ctx, jobIn)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetJobRun(ctx, &sdk.GetJobRunInput{ApplicationId: hive.ApplicationId, JobRunId: job.JobRunId})
	if err != nil || got.JobRun.State != types.JobRunStateSubmitted {
		t.Fatal(got, err)
	}

	app, err := c.GetApplication(ctx, &sdk.GetApplicationInput{ApplicationId: hive.ApplicationId})
	if err != nil || app.Application.State != types.ApplicationStateStarted ||
		!aws.ToBool(app.Application.AutoStartConfiguration.Enabled) ||
		aws.ToInt32(app.Application.AutoStopConfiguration.IdleTimeoutMinutes) != 15 {
		t.Fatal(app, err)
	}

	_, err = c.GetJobRun(
		ctx,
		&sdk.GetJobRunInput{ApplicationId: hive.ApplicationId, JobRunId: job.JobRunId, Attempt: new(int32(1))},
	)
	wantEMRError(t, err, "NotImplemented")
	_, err = c.ListApplications(ctx, &sdk.ListApplicationsInput{NextToken: new("!!!")})
	wantEMRError(t, err, "ValidationException")
	_, err = c.ListJobRuns(ctx, &sdk.ListJobRunsInput{ApplicationId: hive.ApplicationId, MaxResults: new(int32(51))})
	wantEMRError(t, err, "ValidationException")
	_, err = c.ListApplications(
		ctx,
		&sdk.ListApplicationsInput{States: []types.ApplicationState{types.ApplicationState("bad")}},
	)
	wantEMRError(t, err, "ValidationException")
	tags := map[string]string{}
	for i := range 51 {
		tags[fmt.Sprintf("key%d", i)] = "value"
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: hive.Arn, Tags: tags})
	wantEMRError(t, err, "ValidationException")
	// Applications paginate independently of job runs and tokens.
	_, err = c.CreateApplication(ctx, &sdk.CreateApplicationInput{Type: new("Spark"), ReleaseLabel: new("emr-7.0.0")})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListApplications(ctx, &sdk.ListApplicationsInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Applications) != 1 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.ListApplications(
		ctx,
		&sdk.ListApplicationsInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(next.Applications) != 1 || next.NextToken != nil {
		t.Fatal(next, err)
	}
}

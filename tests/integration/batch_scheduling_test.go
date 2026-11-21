package integration_test

import (
	"context"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

func TestBatchFairshareSDK(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := batchClient(s, "test", "us-east-1")
	ce, fifo, d := batchFixture(t, s)
	in := &sdk.CreateSchedulingPolicyInput{
		Name: new("team"),
		FairsharePolicy: &types.FairsharePolicy{
			ComputeReservation: new(int32(25)),
			ShareDecaySeconds:  new(int32(1200)),
			ShareDistribution: []types.ShareAttributes{
				{ShareIdentifier: new("Team*"), WeightFactor: new(float32(0.25))},
			},
		},
		Tags: map[string]string{"owner": "batch"},
	}
	policy, err := c.CreateSchedulingPolicy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	in.Name = new("replacement")
	replacement, err := c.CreateSchedulingPolicy(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	in.Name = new("overlap")
	in.FairsharePolicy.ShareDistribution = append(in.FairsharePolicy.ShareDistribution,
		types.ShareAttributes{ShareIdentifier: new("TeamA")})
	_, err = c.CreateSchedulingPolicy(ctx, in)
	code(t, err, "ClientException")

	list, err := c.ListSchedulingPolicies(ctx, &sdk.ListSchedulingPoliciesInput{MaxResults: new(int32(1))})
	if err != nil || len(list.SchedulingPolicies) != 1 || list.NextToken == nil ||
		*list.SchedulingPolicies[0].Arn != *replacement.Arn {
		t.Fatalf("first page: %+v %v", list, err)
	}

	list, err = c.ListSchedulingPolicies(ctx, &sdk.ListSchedulingPoliciesInput{NextToken: list.NextToken})
	if err != nil || len(list.SchedulingPolicies) != 1 || list.NextToken != nil ||
		*list.SchedulingPolicies[0].Arn != *policy.Arn {
		t.Fatalf("second page: %+v %v", list, err)
	}

	_, err = c.UpdateJobQueue(ctx, &sdk.UpdateJobQueueInput{
		JobQueue: fifo.JobQueueArn, SchedulingPolicyArn: policy.Arn,
	})
	code(t, err, "ClientException")

	q, err := c.CreateJobQueue(ctx, &sdk.CreateJobQueueInput{
		JobQueueName: new("weighted"), Priority: new(int32(1)), SchedulingPolicyArn: policy.Arn,
		ComputeEnvironmentOrder: []types.ComputeEnvironmentOrder{
			{Order: new(int32(1)), ComputeEnvironment: ce.ComputeEnvironmentArn},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteSchedulingPolicy(ctx, &sdk.DeleteSchedulingPolicyInput{Arn: policy.Arn})
	code(t, err, "ClientException")

	submit := &sdk.SubmitJobInput{JobName: new("weighted"), JobQueue: q.JobQueueArn, JobDefinition: d.JobDefinitionArn}
	_, err = c.SubmitJob(ctx, submit)
	code(t, err, "ClientException")

	submit.ShareIdentifier, submit.SchedulingPriorityOverride = new("TeamA"), new(int32(25))
	j, err := c.SubmitJob(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.DescribeJobs(ctx, &sdk.DescribeJobsInput{Jobs: []string{*j.JobId}})
	if err != nil || len(got.Jobs) != 1 || *got.Jobs[0].ShareIdentifier != "TeamA" ||
		*got.Jobs[0].SchedulingPriority != 25 {
		t.Fatalf("job: %+v %v", got, err)
	}

	jobs, err := c.ListJobs(ctx, &sdk.ListJobsInput{
		JobQueue: q.JobQueueArn, JobStatus: types.JobStatusRunnable,
		Filters: []types.KeyValuesPair{{Name: new("SHARE_IDENTIFIER"), Values: []string{"TeamA"}}},
	})
	if err != nil || len(jobs.JobSummaryList) != 1 || *jobs.JobSummaryList[0].JobId != *j.JobId {
		t.Fatalf("share filter: %+v %v", jobs, err)
	}

	_, err = c.UpdateSchedulingPolicy(ctx, &sdk.UpdateSchedulingPolicyInput{
		Arn: policy.Arn, FairsharePolicy: &types.FairsharePolicy{ComputeReservation: new(int32(0))},
	})
	if err != nil {
		t.Fatal(err)
	}

	desc, err := c.DescribeSchedulingPolicies(ctx, &sdk.DescribeSchedulingPoliciesInput{Arns: []string{*policy.Arn}})
	if err != nil || len(desc.SchedulingPolicies) != 1 {
		t.Fatalf("describe: %+v %v", desc, err)
	}

	details := desc.SchedulingPolicies[0]
	if *details.FairsharePolicy.ComputeReservation != 0 || *details.FairsharePolicy.ShareDecaySeconds != 1200 ||
		len(details.FairsharePolicy.ShareDistribution) != 1 || details.Tags["owner"] != "batch" {
		t.Fatalf("updated policy: %+v", details)
	}

	_, err = c.UpdateJobQueue(
		ctx,
		&sdk.UpdateJobQueueInput{JobQueue: q.JobQueueArn, SchedulingPolicyArn: replacement.Arn},
	)
	if err != nil {
		t.Fatal(err)
	}

	queues, err := c.DescribeJobQueues(ctx, &sdk.DescribeJobQueuesInput{JobQueues: []string{*q.JobQueueArn}})
	if err != nil || len(queues.JobQueues) != 1 || *queues.JobQueues[0].SchedulingPolicyArn != *replacement.Arn {
		t.Fatalf("updated queue: %+v %v", queues, err)
	}

	_, err = c.DeleteSchedulingPolicy(ctx, &sdk.DeleteSchedulingPolicyInput{Arn: policy.Arn})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBatchSecretVolumesRecordsSDK(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := batchClient(s, "test", "us-east-1")
	_, q, _ := batchFixture(t, s)

	in := batchDefinition()
	in.ContainerProperties.Secrets = []types.Secret{{Name: new("TOKEN"), ValueFrom: new("/will/resolve/at/launch")}}
	in.ContainerProperties.Volumes = []types.Volume{{Name: new("host"), Host: &types.Host{SourcePath: new("/data")}}}
	in.ContainerProperties.MountPoints = []types.MountPoint{
		{SourceVolume: new("host"), ContainerPath: new("/input"), ReadOnly: new(true)},
	}
	d, err := c.RegisterJobDefinition(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	j, err := c.SubmitJob(ctx, &sdk.SubmitJobInput{
		JobName: new("references"), JobQueue: q.JobQueueArn, JobDefinition: d.JobDefinitionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.DescribeJobs(ctx, &sdk.DescribeJobsInput{Jobs: []string{*j.JobId}})
	if err != nil || len(got.Jobs) != 1 || got.Jobs[0].Status != types.JobStatusRunnable {
		t.Fatalf("records job: %+v %v", got, err)
	}

	container := got.Jobs[0].Container
	if len(container.Secrets) != 1 || len(container.Volumes) != 1 || len(container.MountPoints) != 1 ||
		*container.Secrets[0].ValueFrom != "/will/resolve/at/launch" || *container.Volumes[0].Host.SourcePath != "/data" ||
		!*container.MountPoints[0].ReadOnly {
		t.Fatalf("stored references: %+v", container)
	}

	in.ContainerProperties.MountPoints[0].SourceVolume = new("unknown")
	_, err = c.RegisterJobDefinition(ctx, in)
	code(t, err, "ClientException")
}

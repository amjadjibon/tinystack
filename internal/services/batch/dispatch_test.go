package batch

import (
	"reflect"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func TestFairshareDispatch(t *testing.T) {
	ctx := batchContext()
	s := jobsFixture(t)
	p, err := s.CreateSchedulingPolicy(ctx, &api.CreateSchedulingPolicyRequest{
		Name: new("shares"),
		FairsharePolicy: &api.FairsharePolicy{
			ShareDistribution: []api.ShareAttributes{{ShareIdentifier: new("fast*"), WeightFactor: new(0.5)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateJobQueue(ctx, &api.CreateJobQueueRequest{
		JobQueueName: new("fair"), Priority: new(int64(5)), SchedulingPolicyArn: p.Arn,
		ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{
			{Order: new(int64(1)), ComputeEnvironment: new("compute")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	submit := func(name, share string, priority int64) string {
		t.Helper()
		job, e := s.SubmitJob(ctx, &api.SubmitJobRequest{
			JobName: new(name), JobQueue: new("fair"), JobDefinition: new("worker"),
			ShareIdentifier: new(share), SchedulingPriorityOverride: new(priority),
		})
		if e != nil {
			t.Fatal(e)
		}

		return *job.JobId
	}

	slow := submit("slow-high", "slow", 9999)
	fastLow := submit("fast-low", "fastTeam", 0)
	fastHigh := submit("fast-high", "fastTeam", 50)
	scope, _ := request.RegionalScope(ctx)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		env, e := read(tx, scope, kindCE, new("compute"))
		if e != nil {
			return e
		}

		env.CE.ComputeResources.MaxvCpus = new(int64(2))
		return save(tx, scope, env)
	})
	if err != nil {
		t.Fatal(err)
	}

	s.runtime = &unitRuntime{}
	s.bg.Stop()
	s.visitJobs(ctx)
	for id, want := range map[string]string{fastHigh: statusStarting, fastLow: statusRunnable, slow: statusRunnable} {
		if got := common.Value(unitJob(t, s, id).Status); got != want {
			t.Fatalf("job %s status = %s, want %s", id, got, want)
		}
	}

	_, err = s.TerminateJob(ctx, &api.TerminateJobRequest{JobId: new(fastHigh), Reason: new("finished")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.UpdateSchedulingPolicy(ctx, &api.UpdateSchedulingPolicyRequest{
		Arn: p.Arn,
		FairsharePolicy: &api.FairsharePolicy{
			ShareDistribution: []api.ShareAttributes{{ShareIdentifier: new("slow"), WeightFactor: new(0.5)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	s.visitJobs(ctx)
	if got := common.Value(unitJob(t, s, slow).Status); got != statusStarting {
		t.Fatalf("updated weighted order picked %s, want STARTING", got)
	}
}

func TestListJobsCreationOrderThroughFilteredPages(t *testing.T) {
	s := jobsFixture(t)
	ctx := batchContext()
	scope, _ := request.RegionalScope(ctx)
	ids := []string{submitUnit(t, s, nil, 0), submitUnit(t, s, nil, 0), submitUnit(t, s, nil, 0)}
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for i, id := range ids {
			row, e := read(tx, scope, kindJob, new(id))
			if e != nil {
				return e
			}

			row.Job.CreatedAt = new(int64(i + 1))
			if e := saveJob(tx, row); e != nil {
				return e
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	in := &api.ListJobsRequest{
		JobQueue: new("queue"), MaxResults: new(int64(1)),
		Filters: []api.KeyValuesPair{{Name: new("JOB_NAME"), Values: []string{"job"}}},
	}
	var got []string
	for range ids {
		p, e := s.ListJobs(ctx, in)
		if e != nil || len(p.JobSummaryList) != 1 {
			t.Fatalf("page: %+v %v", p, e)
		}

		got = append(got, common.Value(p.JobSummaryList[0].JobId))
		in.NextToken = p.NextToken
	}

	if in.NextToken != nil || !reflect.DeepEqual(got, []string{ids[2], ids[1], ids[0]}) {
		t.Fatalf("jobs not in descending creation order: %v", got)
	}
}

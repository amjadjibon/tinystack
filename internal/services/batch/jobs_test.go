package batch

import (
	"context"
	"reflect"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func jobsFixture(t *testing.T) *Service {
	t.Helper()
	s, in := managedFixture(t)
	ce, err := s.CreateComputeEnvironment(batchContext(), in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateJobQueue(batchContext(), &api.CreateJobQueueRequest{
		JobQueueName: new("queue"),
		Priority:     new(int64(1)),
		ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{
			{Order: new(int64(1)), ComputeEnvironment: ce.ComputeEnvironmentArn},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.RegisterJobDefinition(batchContext(), definitionInput())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	return s
}

func submitUnit(t *testing.T, s *Service, deps []api.JobDependency, size int64) string {
	t.Helper()
	in := &api.SubmitJobRequest{
		JobName:       new("job"),
		JobQueue:      new("queue"),
		JobDefinition: new("worker"),
		DependsOn:     deps,
	}

	if size > 0 {
		in.ArrayProperties = &api.ArrayProperties{Size: new(size)}
	}

	out, err := s.SubmitJob(batchContext(), in)
	if err != nil {
		t.Fatal(err)
	}

	return *out.JobId
}

func unitJob(t *testing.T, s *Service, id string) api.JobDetail {
	t.Helper()
	out, err := s.DescribeJobs(batchContext(), &api.DescribeJobsRequest{Jobs: []string{id}})
	if err != nil || len(out.Jobs) != 1 {
		t.Fatalf("describe: %+v %v", out, err)
	}

	return out.Jobs[0]
}

func TestJobParametersAndRetryRules(t *testing.T) {
	got, err := substitute([]string{"echo", "Ref::x", "literal"}, map[string]string{"x": "hello"})
	if err != nil || !reflect.DeepEqual(got, []string{"echo", "hello", "literal"}) {
		t.Fatal(got, err)
	}

	_, err = substitute([]string{"Ref::missing"}, nil)
	clientError(t, err)
	strategy := &api.RetryStrategy{Attempts: new(int64(3)), EvaluateOnExit: []api.EvaluateOnExit{
		{Action: new("EXIT"), OnExitCode: new("4*"), OnReason: new("fatal*")},
		{Action: new("RETRY"), OnStatusReason: new("Transient*")},
	}}
	for _, tc := range []struct {
		reason, status string
		attempt        int
		code           int64
		want           bool
	}{
		{"fatal issue", "", 1, 42, false},
		{"other", "", 1, 42, true},
		{"", "Transient error", 2, 1, true},
		{"", "Transient", 3, 1, false},
	} {
		if got := retry(strategy, tc.attempt, new(tc.code), tc.reason, tc.status); got != tc.want {
			t.Fatalf("%+v got %v", tc, got)
		}
	}
}

func TestRecordsJobsArraysDependenciesAndCancel(t *testing.T) {
	s := jobsFixture(t)
	first := submitUnit(t, s, nil, 0)
	dependent := submitUnit(t, s, []api.JobDependency{{JobId: new(first)}}, 0)
	sequential := submitUnit(t, s, []api.JobDependency{{Type: new("SEQUENTIAL")}}, 3)
	other := submitUnit(t, s, []api.JobDependency{{Type: new("N_TO_N"), JobId: new(sequential)}}, 3)
	for id, want := range map[string]string{
		first: statusRunnable, dependent: statusPending, sequential: statusPending,
		sequential + ":0": statusRunnable, sequential + ":1": statusPending, other + ":0": statusPending,
	} {
		if got := common.Value(unitJob(t, s, id).Status); got != want {
			t.Fatalf("%s: %s want %s", id, got, want)
		}
	}

	_, err := s.CancelJob(batchContext(), &api.CancelJobRequest{JobId: new(dependent), Reason: new("cancel")})
	if err != nil {
		t.Fatal(err)
	}

	if common.Value(unitJob(t, s, dependent).Status) != statusPending {
		t.Fatal("PENDING cancellation must wait")
	}

	_, err = s.TerminateJob(batchContext(), &api.TerminateJobRequest{JobId: new(first), Reason: new("stop")})
	if err != nil {
		t.Fatal(err)
	}

	if j := unitJob(
		t,
		s,
		dependent,
	); common.Value(j.Status) != statusFailed ||
		common.Value(j.StatusReason) != "cancel" {
		t.Fatal(j)
	}

	_, err = s.CancelJob(batchContext(), &api.CancelJobRequest{JobId: new(sequential), Reason: new("stop array")})
	if err != nil {
		t.Fatal(err)
	}

	if common.Value(unitJob(t, s, sequential).Status) != statusFailed ||
		common.Value(unitJob(t, s, other).Status) != statusFailed {
		t.Fatal("array failure did not propagate")
	}
}

func TestJobModelValidation(t *testing.T) {
	s := jobsFixture(t)
	for _, in := range []*api.SubmitJobRequest{
		{},
		{
			JobName: new("job"), JobQueue: new("queue"), JobDefinition: new("worker"),
			RetryStrategy: &api.RetryStrategy{EvaluateOnExit: []api.EvaluateOnExit{{Action: new("INVALID")}}},
		},
	} {
		_, err := api.Descriptor(s).Operations["SubmitJob"].Invoke(batchContext(), in)
		clientError(t, err)
	}

	for _, in := range []*api.SubmitJobRequest{
		{
			JobName: new("job"), JobQueue: new("queue"), JobDefinition: new("worker"),
			RetryStrategy: &api.RetryStrategy{Attempts: new(int64(11))},
		},
		{
			JobName: new("job"), JobQueue: new("queue"), JobDefinition: new("worker"),
			Timeout: &api.JobTimeout{AttemptDurationSeconds: new(int64(59))},
		},
	} {
		_, err := api.Descriptor(s).Operations["SubmitJob"].Invoke(batchContext(), in)
		clientError(t, err)
	}
}

func TestSchedulerPriorityCapacityAndDisabledEnvironment(t *testing.T) {
	s := jobsFixture(t)
	// Queue priority controls claims even when a lower-priority job was submitted first.
	_, err := s.CreateJobQueue(batchContext(), &api.CreateJobQueueRequest{
		JobQueueName: new("high"),
		Priority:     new(int64(99)),
		ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{
			{Order: new(int64(1)), ComputeEnvironment: new("compute")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	low := submitUnit(t, s, nil, 0)
	high, err := s.SubmitJob(
		batchContext(),
		&api.SubmitJobRequest{JobName: new("high"), JobQueue: new("high"), JobDefinition: new("worker")},
	)
	if err != nil {
		t.Fatal(err)
	}

	scope, _ := request.RegionalScope(batchContext())
	err = s.store.Update(batchContext(), func(tx storage.WriteTx) error {
		ce, e := read(tx, scope, kindCE, new("compute"))
		if e != nil {
			return e
		}

		ce.CE.ComputeResources.MaxvCpus = new(int64(2))
		return save(tx, scope, ce)
	})
	if err != nil {
		t.Fatal(err)
	}

	// A stopped background group keeps the transaction's scheduling claims inspectable.
	s.bg.Stop()
	s.runtime = &unitRuntime{}
	s.visitJobs(context.Background())
	if common.Value(unitJob(t, s, *high.JobId).Status) != statusStarting ||
		common.Value(unitJob(t, s, low).Status) != statusRunnable {
		t.Fatal("priority or capacity violated")
	}
}

type unitRuntime struct{}

func (*unitRuntime) Start(context.Context, *ecs.ContainerSpec) (ecs.Started, error) {
	return ecs.Started{ID: "container"}, nil
}

func (*unitRuntime) State(context.Context, string) (exited bool, code int64, err error) {
	return false, 0, nil
}

func (*unitRuntime) Logs(context.Context, string, time.Time) ([]ecs.LogLine, error) { return nil, nil }
func (*unitRuntime) Remove(context.Context, string)                                 {}
func (*unitRuntime) Close()                                                         {}

func TestSchedulerEnvironmentOrderAndDisabledState(t *testing.T) {
	s := jobsFixture(t)
	scope, _ := request.RegionalScope(batchContext())
	err := s.store.Update(batchContext(), func(tx storage.WriteTx) error {
		ce, e := read(tx, scope, kindCE, new("compute"))
		if e != nil {
			return e
		}

		second := common.Clone(ce)
		second.ID = "second"
		second.ARN = arn(scope, kindCE, second.ID)
		second.CE.ComputeEnvironmentArn = new(second.ARN)
		second.CE.ComputeEnvironmentName = new(second.ID)
		if saveErr := save(tx, scope, second); saveErr != nil {
			return saveErr
		}

		ce.CE.State = new(disabled)
		if saveErr := save(tx, scope, ce); saveErr != nil {
			return saveErr
		}

		q, e := read(tx, scope, kindQueue, new("queue"))
		if e != nil {
			return e
		}

		q.Queue.ComputeEnvironmentOrder = append(
			q.Queue.ComputeEnvironmentOrder,
			api.ComputeEnvironmentOrder{Order: new(int64(2)), ComputeEnvironment: new(second.ARN)},
		)
		return save(tx, scope, q)
	})
	if err != nil {
		t.Fatal(err)
	}

	id := submitUnit(t, s, nil, 0)
	s.bg.Stop()
	s.runtime = &unitRuntime{}
	s.visitJobs(context.Background())
	err = s.store.View(batchContext(), func(tx storage.ReadTx) error {
		r, e := read(tx, scope, kindJob, new(id))
		if e != nil {
			return e
		}

		if r.ComputeEnvironment != arn(scope, kindCE, "second") {
			t.Fatal("disabled first environment was selected")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionExecutionOptionsAndResourceOverrides(t *testing.T) {
	s := jobsFixture(t)
	in := definitionInput()
	in.RetryStrategy = &api.RetryStrategy{Attempts: new(int64(3))}
	in.Timeout = &api.JobTimeout{AttemptDurationSeconds: new(int64(120))}
	_, err := s.RegisterJobDefinition(batchContext(), in)
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.SubmitJob(batchContext(), &api.SubmitJobRequest{
		JobName:       new("override"),
		JobQueue:      new("queue"),
		JobDefinition: new("worker"),
		Parameters: map[string]string{
			"input": "replaced",
		},
		Timeout: &api.JobTimeout{AttemptDurationSeconds: new(int64(60))},
		ContainerOverrides: &api.ContainerOverrides{
			ResourceRequirements: []api.ResourceRequirement{{Type: new("VCPU"), Value: new("4")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	job := unitJob(t, s, *out.JobId)
	if *job.RetryStrategy.Attempts != 3 || *job.Timeout.AttemptDurationSeconds != 60 ||
		job.Container.Command[1] != "replaced" ||
		jobCPU(job.Container) != 4 ||
		jobMemory(job.Container) != 128 {
		t.Fatalf("definition/override merge: %+v", job)
	}
}

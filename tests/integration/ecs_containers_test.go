package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	logsapi "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	orchestration "github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// fakeContainer is a container the fake runtime runs.
type fakeContainer struct {
	health  string
	logs    []orchestration.LogLine
	spec    orchestration.ContainerSpec
	code    int64
	exited  bool
	removed bool
}

// containerRuntime runs task containers in memory. A container exits when the
// test says so, and an image named "missing" fails to start.
type containerRuntime struct {
	containers map[string]*fakeContainer
	mu         sync.Mutex
	next       int
	closed     bool
}

func (r *containerRuntime) Start(_ context.Context, spec *orchestration.ContainerSpec) (orchestration.Started, error) {
	if spec.Image == "missing" {
		return orchestration.Started{}, errors.New("pull access denied for missing")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := "c" + strconv.Itoa(r.next)
	r.containers[id] = &fakeContainer{spec: *spec}
	started := orchestration.Started{ID: id}
	for i, p := range spec.Ports {
		host := p.Host
		if host == 0 {
			host = int64(40000 + r.next*10 + i)
		}

		started.Ports = append(started.Ports, orchestration.PortSpec{
			Container: p.Container, Host: host, Protocol: p.Protocol,
		})
	}

	return started, nil
}

func (r *containerRuntime) State(_ context.Context, id string) (exited bool, code int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok || c.removed {
		return false, 0, orchestration.ErrGone
	}

	return c.exited, c.code, nil
}

func (r *containerRuntime) Logs(_ context.Context, id string, since time.Time) ([]orchestration.LogLine, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []orchestration.LogLine
	for _, l := range r.containers[id].logs {
		if l.Time.After(since) {
			out = append(out, l)
		}
	}

	return out, nil
}

func (r *containerRuntime) Remove(_ context.Context, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers[id].removed = true
}

func (r *containerRuntime) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

// find returns the container the runtime started for a task container.
func (r *containerRuntime) find(id string) fakeContainer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.containers[id]
}

func (r *containerRuntime) exit(id string, code int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers[id].exited, r.containers[id].code = true, code
}

func (r *containerRuntime) write(id, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers[id].logs = append(r.containers[id].logs, orchestration.LogLine{Time: time.Now(), Message: message})
}

// ecsServer serves ECS, running containers in rt, and CloudWatch Logs.
func ecsServer(t *testing.T, rt orchestration.Runtime) *httptest.Server {
	t.Helper()
	store := &memory.Store{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	logGroups := logs.New(store)
	tasks := orchestration.New(store)
	tasks.UseRuntime(rt, logGroups.Append, logger)
	h, err := gateway.New(config.Default(), logger, []catalog.Service{
		ecsapi.Descriptor(tasks), logsapi.Descriptor(logGroups),
	}...)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	t.Cleanup(func() {
		tasks.Stop()
		s.Close()
	})

	if _, err = ecsClient(s, "us-east-1").CreateCluster(context.Background(), &ecs.CreateClusterInput{}); err != nil {
		t.Fatal(err)
	}

	return s
}

func describeTask(ctx context.Context, t *testing.T, c *ecs.Client, arn *string) ecstypes.Task {
	t.Helper()
	out, err := c.DescribeTasks(ctx, &ecs.DescribeTasksInput{Tasks: []string{aws.ToString(arn)}})
	if err != nil || len(out.Tasks) != 1 {
		t.Fatalf("describe = %+v, %v", out, err)
	}

	return out.Tasks[0]
}

// waitStatus waits until a task's last status is status and returns it.
func waitStatus(ctx context.Context, t *testing.T, c *ecs.Client, arn *string, status string) ecstypes.Task {
	t.Helper()
	var got ecstypes.Task
	waitFor(t, "task "+status, func() bool {
		got = describeTask(ctx, t, c, arn)
		return aws.ToString(got.LastStatus) == status
	})

	return got
}

func TestECSTasksRunContainers(t *testing.T) {
	rt := &containerRuntime{containers: map[string]*fakeContainer{}}
	s := ecsServer(t, rt)
	c := ecsClient(s, "us-east-1")
	ctx := context.Background()
	_, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("web"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:         aws.String("web"),
				Image:        aws.String("nginx"),
				Memory:       aws.Int32(128),
				Command:      []string{"serve"},
				PortMappings: []ecstypes.PortMapping{{ContainerPort: aws.Int32(80)}},
				Environment:  []ecstypes.KeyValuePair{{Name: aws.String("MODE"), Value: aws.String("test")}},
				LogConfiguration: &ecstypes.LogConfiguration{
					LogDriver: ecstypes.LogDriverAwslogs,
					Options: map[string]string{
						"awslogs-group": "/ecs/web", "awslogs-stream-prefix": "app",
					},
				},
			},
			{
				Name:      aws.String("sidecar"),
				Image:     aws.String("busybox"),
				Memory:    aws.Int32(64),
				Essential: aws.Bool(false),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := c.RunTask(ctx, &ecs.RunTaskInput{
		TaskDefinition: aws.String("web"),
		Overrides: &ecstypes.TaskOverride{ContainerOverrides: []ecstypes.ContainerOverride{{
			Name: aws.String("web"), Command: []string{"serve", "--debug"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	taskARN := run.Tasks[0].TaskArn
	if aws.ToString(run.Tasks[0].LastStatus) != "PENDING" || run.Tasks[0].Containers[0].RuntimeId != nil {
		t.Fatalf("run = %+v", run.Tasks[0])
	}

	task := waitStatus(ctx, t, c, taskARN, "RUNNING")
	web, sidecar := task.Containers[0], task.Containers[1]
	spec := rt.find(aws.ToString(web.RuntimeId)).spec
	if len(web.NetworkBindings) != 1 || aws.ToInt32(web.NetworkBindings[0].HostPort) < 40000 ||
		spec.Environment["MODE"] != "test" || spec.Environment["AWS_REGION"] != "us-east-1" ||
		len(spec.Command) != 2 || spec.Labels["tinystack.ecs.task"] != aws.ToString(taskARN) {
		t.Fatalf("web = %+v, spec = %+v", web, spec)
	}

	// The awslogs driver ships the container's output.
	rt.write(aws.ToString(web.RuntimeId), "listening")
	logsAPI := logsClient(s, "test")
	stream := "app/web/" + aws.ToString(taskARN)[len(aws.ToString(taskARN))-32:]
	waitFor(t, "log events", func() bool {
		out, getErr := logsAPI.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName: aws.String("/ecs/web"), LogStreamName: &stream,
		})
		return getErr == nil && len(out.Events) == 1 && aws.ToString(out.Events[0].Message) == "listening"
	})

	// A non-essential container's exit leaves the task running.
	rt.exit(aws.ToString(sidecar.RuntimeId), 3)
	waitFor(t, "sidecar exit", func() bool {
		task = describeTask(ctx, t, c, taskARN)
		return aws.ToString(task.Containers[1].LastStatus) == "STOPPED"
	})

	if aws.ToString(task.LastStatus) != "RUNNING" || aws.ToInt32(task.Containers[1].ExitCode) != 3 {
		t.Fatalf("task = %+v", task)
	}

	// An essential container's exit stops it and removes its containers.
	rt.exit(aws.ToString(web.RuntimeId), 1)
	task = waitStatus(ctx, t, c, taskARN, "STOPPED")
	if task.StopCode != ecstypes.TaskStopCodeEssentialContainerExited ||
		aws.ToInt32(task.Containers[0].ExitCode) != 1 || aws.ToInt32(task.Containers[1].ExitCode) != 3 {
		t.Fatalf("stopped = %+v", task)
	}

	waitFor(t, "removed containers", func() bool {
		return rt.find(aws.ToString(web.RuntimeId)).removed && rt.find(aws.ToString(sidecar.RuntimeId)).removed
	})
}

func TestECSServiceReplacesExitedTasks(t *testing.T) {
	rt := &containerRuntime{containers: map[string]*fakeContainer{}}
	s := ecsServer(t, rt)
	c := ecsClient(s, "us-east-1")
	ctx := context.Background()
	for family, image := range map[string]string{"api": "api:1", "broken": "missing"} {
		if _, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
			Family: aws.String(family),
			ContainerDefinitions: []ecstypes.ContainerDefinition{{
				Name: aws.String("app"), Image: aws.String(image), Memory: aws.Int32(128),
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("secretive"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("api:1"), Memory: aws.Int32(128),
			Secrets: []ecstypes.Secret{{Name: aws.String("TOKEN"), ValueFrom: aws.String("/missing")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Tasks whose containers cannot start, or use what TinyStack cannot
	// run, fail to start.
	for _, family := range []string{"broken", "secretive"} {
		run, runErr := c.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: aws.String(family)})
		if runErr != nil {
			t.Fatal(runErr)
		}

		task := waitStatus(ctx, t, c, run.Tasks[0].TaskArn, "STOPPED")
		if task.StopCode != ecstypes.TaskStopCodeTaskFailedToStart || aws.ToString(task.StoppedReason) == "" ||
			task.Containers[0].ExitCode != nil {
			t.Fatalf("%s = %+v", family, task)
		}
	}

	created, err := c.CreateService(ctx, &ecs.CreateServiceInput{
		ServiceName: aws.String("api"), TaskDefinition: aws.String("api"), DesiredCount: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	if created.Service.PendingCount != 1 || created.Service.RunningCount != 0 {
		t.Fatalf("created = %+v", created.Service)
	}

	listed, err := c.ListTasks(ctx, &ecs.ListTasksInput{ServiceName: aws.String("api")})
	if err != nil || len(listed.TaskArns) != 1 {
		t.Fatalf("list = %+v, %v", listed, err)
	}

	first := waitStatus(ctx, t, c, &listed.TaskArns[0], "RUNNING")
	rt.exit(aws.ToString(first.Containers[0].RuntimeId), 137)
	waitStatus(ctx, t, c, first.TaskArn, "STOPPED")
	var replacement string
	waitFor(t, "replacement task", func() bool {
		out, listErr := c.ListTasks(ctx, &ecs.ListTasksInput{ServiceName: aws.String("api")})
		if listErr != nil || len(out.TaskArns) != 1 || out.TaskArns[0] == aws.ToString(first.TaskArn) {
			return false
		}

		replacement = out.TaskArns[0]
		return aws.ToString(describeTask(ctx, t, c, &replacement).LastStatus) == "RUNNING"
	})

	described, err := c.DescribeServices(ctx, &ecs.DescribeServicesInput{Services: []string{"api"}})
	if err != nil || described.Services[0].RunningCount != 1 || described.Services[0].PendingCount != 0 {
		t.Fatalf("service = %+v, %v", described, err)
	}

	// Stopping a task removes its containers.
	running := describeTask(ctx, t, c, &replacement)
	if _, err = c.StopTask(ctx, &ecs.StopTaskInput{Task: &replacement}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "removed container", func() bool {
		return rt.find(aws.ToString(running.Containers[0].RuntimeId)).removed
	})
}

func (r *containerRuntime) Health(_ context.Context, id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.containers[id].health == "" {
		return "UNKNOWN", nil
	}

	return r.containers[id].health, nil
}

func TestECSServiceReplacesUnhealthyTasks(t *testing.T) {
	rt := &containerRuntime{containers: map[string]*fakeContainer{}}
	s := ecsServer(t, rt)
	c := ecsClient(s, "us-east-1")
	ctx := context.Background()
	_, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: new("health"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name: new("web"), Image: new("test"), Memory: new(int32(64)),
			HealthCheck: &ecstypes.HealthCheck{Command: []string{"CMD", "true"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateService(ctx, &ecs.CreateServiceInput{
		ServiceName: new("health"), TaskDefinition: new("health"), DesiredCount: new(int32(1)),
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTasks(ctx, &ecs.ListTasksInput{ServiceName: new("health")})
	if err != nil || len(listed.TaskArns) != 1 {
		t.Fatalf("list=%+v err=%v", listed, err)
	}

	first := waitStatus(ctx, t, c, &listed.TaskArns[0], "RUNNING")
	id := aws.ToString(first.Containers[0].RuntimeId)
	rt.mu.Lock()
	rt.containers[id].health = "HEALTHY"
	rt.mu.Unlock()
	waitFor(t, "healthy status", func() bool {
		return describeTask(ctx, t, c, first.TaskArn).HealthStatus == ecstypes.HealthStatusHealthy
	})
	rt.mu.Lock()
	rt.containers[id].health = "UNHEALTHY"
	rt.mu.Unlock()
	stopped := waitStatus(ctx, t, c, first.TaskArn, "STOPPED")
	if stopped.HealthStatus != ecstypes.HealthStatusUnhealthy ||
		aws.ToString(stopped.StoppedReason) != "Essential container failed health checks" {
		t.Fatalf("stopped=%+v", stopped)
	}

	waitFor(t, "healthy service replacement", func() bool {
		out, listErr := c.ListTasks(ctx, &ecs.ListTasksInput{ServiceName: new("health")})
		return listErr == nil && len(out.TaskArns) == 1 && out.TaskArns[0] != aws.ToString(first.TaskArn)
	})
}

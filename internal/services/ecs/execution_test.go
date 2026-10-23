package ecs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	secretsapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	ssmapi "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/services/ssm"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestExecutionValidation(t *testing.T) {
	base := func() *api.RegisterTaskDefinitionRequest {
		return &api.RegisterTaskDefinitionRequest{Family: new("test"), ContainerDefinitions: []api.ContainerDefinition{
			{Name: new("web"), Image: new("busybox"), Memory: new(int64(64))},
			{Name: new("init"), Image: new("busybox"), Memory: new(int64(64)), Essential: new(false)},
		}}
	}
	tests := []struct {
		change func(*api.RegisterTaskDefinitionRequest)
		name   string
	}{
		{name: "cycle", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[0].DependsOn = []api.ContainerDependency{
				{ContainerName: new("init"), Condition: new("START")},
			}
			in.ContainerDefinitions[1].DependsOn = []api.ContainerDependency{
				{ContainerName: new("web"), Condition: new("START")},
			}
		}},
		{name: "missing health", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[0].DependsOn = []api.ContainerDependency{
				{ContainerName: new("init"), Condition: new("HEALTHY")},
			}
		}},
		{name: "essential job", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[1].DependsOn = []api.ContainerDependency{
				{ContainerName: new("web"), Condition: new("SUCCESS")},
			}
		}},
		{name: "health command", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[0].HealthCheck = &api.HealthCheck{Command: []string{"true"}}
		}},
		{name: "health interval", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[0].HealthCheck = &api.HealthCheck{
				Command:  []string{"CMD", "true"},
				Interval: new(int64(1)),
			}
		}},
		{name: "volume cycle", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[0].VolumesFrom = []api.VolumeFrom{{SourceContainer: new("init")}}
			in.ContainerDefinitions[1].VolumesFrom = []api.VolumeFrom{{SourceContainer: new("web")}}
		}},
		{name: "secret version", change: func(in *api.RegisterTaskDefinitionRequest) {
			in.ContainerDefinitions[0].Secrets = []api.Secret{{
				Name:      new("TOKEN"),
				ValueFrom: new("arn:aws:secretsmanager:us-east-1:123:secret:name:key:AWSCURRENT"),
			}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := base()
			test.change(in)
			if _, err := newDefinition(in); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

func TestSecretResolution(t *testing.T) {
	store := &memory.Store{}
	secrets := secretsmanager.New(store, kms.New(store), nil, delivery.New())
	parameters := ssm.New(store, kms.New(store))
	service := New(store)
	t.Cleanup(service.Stop)
	service.UseCaller(catalog.NewCaller(secretsapi.Descriptor(secrets), ssmapi.Descriptor(parameters)))
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	created, err := secrets.CreateSecret(ctx, &secretsapi.CreateSecretRequest{
		Name: new("credentials"), SecretString: new(`{"password":"hidden","port":5432,"optional":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = parameters.PutParameter(ctx, &ssmapi.PutParameterRequest{
		Name: new("/test/token"), Value: new("parameter-value"), Type: new("SecureString"),
	}); err != nil {
		t.Fatal(err)
	}

	tests := []struct{ ref, want string }{
		{common.Value(created.ARN), `{"password":"hidden","port":5432,"optional":null}`},
		{common.Value(created.ARN) + ":password", "hidden"},
		{common.Value(created.ARN) + ":port", "5432"},
		{common.Value(created.ARN) + ":optional", "null"},
		{"/test/token", "parameter-value"},
		{"arn:aws:ssm:us-east-1:123456789012:parameter/test/token", "parameter-value"},
	}
	for _, test := range tests {
		value, resolveErr := service.secret(ctx, scope, test.ref)
		if resolveErr != nil || value != test.want {
			t.Fatalf("resolve %s: %q, %v", test.ref, value, resolveErr)
		}
	}

	specs := []ContainerSpec{{Environment: map[string]string{}}}
	def := &taskDef{Def: api.TaskDefinition{ContainerDefinitions: []api.ContainerDefinition{{Secrets: []api.Secret{
		{Name: new("MISSING"), ValueFrom: new("/missing")},
	}}}}}
	if err = service.resolveSecrets(ctx, scope, specs, def); err == nil ||
		!strings.HasPrefix(err.Error(), "ResourceInitializationError:") {
		t.Fatalf("missing secret: %v", err)
	}
}

type executionRuntime struct {
	health string
	order  []string
	code   int64
}

func (r *executionRuntime) Start(_ context.Context, spec *ContainerSpec) (Started, error) {
	r.order = append(r.order, spec.Name)
	return Started{ID: spec.Name}, nil
}

func (r *executionRuntime) State(context.Context, string) (exited bool, code int64, err error) {
	return true, r.code, nil
}
func (r *executionRuntime) Health(context.Context, string) (string, error) { return r.health, nil }
func (r *executionRuntime) Logs(context.Context, string, time.Time) ([]LogLine, error) {
	return nil, nil
}
func (r *executionRuntime) Remove(context.Context, string) {}
func (r *executionRuntime) Close()                         {}

func TestDependencyStartup(t *testing.T) {
	for _, condition := range []string{"START", "COMPLETE", "SUCCESS", "HEALTHY"} {
		t.Run(condition, func(t *testing.T) {
			rt := &executionRuntime{health: "HEALTHY"}
			s := New(&memory.Store{})
			t.Cleanup(s.Stop)
			s.runtime = rt
			def := &taskDef{Def: api.TaskDefinition{ContainerDefinitions: []api.ContainerDefinition{
				{DependsOn: []api.ContainerDependency{{ContainerName: new("init"), Condition: new(condition)}}}, {},
			}}}
			// HEALTHY waits for a live container rather than an exited job.
			specs := []ContainerSpec{{Name: "web"}, {Name: "init"}}
			err := s.startOrdered(context.Background(), specs, def, make([]Started, 2))
			if condition == "HEALTHY" {
				if err == nil {
					t.Fatal("exited HEALTHY dependency accepted")
				}

				return
			}

			if err != nil || strings.Join(rt.order, ",") != "init,web" {
				t.Fatalf("order=%v err=%v", rt.order, err)
			}

			if condition == "SUCCESS" {
				rt.code = 1
				if err = s.startOrdered(context.Background(), specs, def, make([]Started, 2)); err == nil {
					t.Fatal("failed SUCCESS dependency accepted")
				}
			}
		})
	}
}

func TestTaskHealth(t *testing.T) {
	task := &task{HealthContainers: []string{"web"}, Containers: []api.Container{
		{Name: new("web"), HealthStatus: new("HEALTHY")}, {Name: new("sidecar"), HealthStatus: new("UNHEALTHY")},
	}}
	if taskHealth(task) != "HEALTHY" {
		t.Fatal("nonessential container changed task health")
	}

	task.Containers[0].HealthStatus = new("UNKNOWN")
	if taskHealth(task) != "UNKNOWN" {
		t.Fatal("starting container should be UNKNOWN")
	}

	task.Containers[0].HealthStatus = new("UNHEALTHY")
	if taskHealth(task) != "UNHEALTHY" {
		t.Fatal("essential container should be UNHEALTHY")
	}
}

func TestStartupCanceledWhenTaskStops(t *testing.T) {
	store := &memory.Store{}
	s := New(store)
	t.Cleanup(s.Stop)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	task := &task{ID: "starting", Cluster: "default", Last: pending}
	ctx := context.Background()
	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, taskKey(scope, task.Cluster, task.ID), task)
	}); err != nil {
		t.Fatal(err)
	}

	startup, cancel := s.startupContext(ctx, scope, task)
	defer cancel()
	task.Last = stopped
	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, taskKey(scope, task.Cluster, task.ID), task)
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-startup.Done():
	case <-time.After(time.Second):
		t.Fatal("stopping task did not cancel startup")
	}
}

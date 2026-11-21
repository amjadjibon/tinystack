package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	stssdk "github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	secretsapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	ssmapi "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/metadata"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/services/ssm"
	"github.com/amjadjibon/tinystack/internal/services/sts"
)

type capturedRuntime struct {
	unitRuntime
	containers []ecs.ContainerSpec
}

func (r *capturedRuntime) Start(_ context.Context, spec *ecs.ContainerSpec) (ecs.Started, error) {
	r.containers = append(r.containers, *spec)
	return ecs.Started{ID: spec.Environment["AWS_BATCH_JOB_ID"]}, nil
}

func TestFargateFractionalCapacity(t *testing.T) {
	ctx := batchContext()
	s, in := managedFixture(t)
	t.Cleanup(s.Stop)
	in.ComputeResources = &api.ComputeResource{
		Type: new("FARGATE_SPOT"), MaxvCpus: new(int64(1)),
		Subnets: in.ComputeResources.Subnets, SecurityGroupIds: in.ComputeResources.SecurityGroupIds,
	}
	ce, err := s.CreateComputeEnvironment(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateJobQueue(ctx, &api.CreateJobQueueRequest{
		JobQueueName: new("queue"), Priority: new(int64(1)),
		ComputeEnvironmentOrder: []api.ComputeEnvironmentOrder{{
			Order:              new(int64(1)),
			ComputeEnvironment: ce.ComputeEnvironmentArn,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	def := definitionInput()
	def.PlatformCapabilities = []string{fargatePlatform}
	def.ContainerProperties.JobRoleArn = nil
	def.ContainerProperties.ResourceRequirements[0].Value = new("0.25")
	def.ContainerProperties.ResourceRequirements[1].Value = new("512")
	_, err = s.RegisterJobDefinition(ctx, def)
	if err != nil {
		t.Fatal(err)
	}

	rt := &capturedRuntime{}
	s.runtime = rt
	s.bg.Stop()
	var ids []string
	for range 5 {
		ids = append(ids, submitUnit(t, s, nil, 0))
	}

	s.visitJobs(ctx)
	scope, _ := request.RegionalScope(ctx)
	waiting := ""
	for _, id := range ids {
		if common.Value(unitJob(t, s, id).Status) == statusRunnable {
			waiting = id
			continue
		}

		if stepErr := s.stepJob(ctx, &record{Scope: scope, ID: id}); stepErr != nil {
			t.Fatal(stepErr)
		}
	}

	if waiting == "" || len(rt.containers) != 4 {
		t.Fatalf("started %d containers, want 4 and one waiting", len(rt.containers))
	}

	for _, spec := range rt.containers {
		if spec.CPU != 256 || spec.Memory != 512 || spec.Platform != "linux/amd64" {
			t.Fatalf("Fargate limits and architecture: %+v", spec)
		}
	}

	// A running attempt continues to reserve its fractional capacity.
	s.visitJobs(ctx)
	if common.Value(unitJob(t, s, waiting).Status) != statusRunnable {
		t.Fatal("job exceeded the environment's concurrent capacity")
	}

	def.ContainerProperties.ResourceRequirements[0].Value = new("1")
	def.ContainerProperties.ResourceRequirements[1].Value = new("2048")
	def.ContainerProperties.RuntimePlatform = &api.RuntimePlatform{
		OperatingSystemFamily: new("WINDOWS_SERVER_2022_CORE"),
	}
	_, err = s.RegisterJobDefinition(ctx, def)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.SubmitJob(ctx, &api.SubmitJobRequest{
		JobName: new("windows"), JobDefinition: new("worker"), JobQueue: new("queue"),
	})
	clientError(t, err)
}

func TestContainerSecretsMountsAndJobRole(t *testing.T) {
	ctx := batchContext()
	scope, _ := request.RegionalScope(ctx)
	s := jobsFixture(t)
	roles := iam.New(s.store)
	tokens := sts.New(s.store, roles)
	_, err := roles.CreateRole(ctx, &iamapi.CreateRoleRequest{
		RoleName: new("job"),
		AssumeRolePolicyDocument: new(`{"Statement":[{"Effect":"Allow","Principal":` +
			`{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	keys := kms.New(s.store)
	secrets := secretsmanager.New(s.store, keys, nil, delivery.New())
	params := ssm.New(s.store, keys)
	s.UseCaller(catalog.NewCaller(secretsapi.Descriptor(secrets), ssmapi.Descriptor(params)))
	secret, err := secrets.CreateSecret(ctx, &secretsapi.CreateSecretRequest{
		Name: new("runtime-secret"), SecretString: new(`{"user":"local"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	def := definitionInput()
	c := def.ContainerProperties
	c.Secrets = []api.Secret{
		{Name: new("TOKEN"), ValueFrom: new("/test/value")},
		{Name: new("USER"), ValueFrom: new(*secret.ARN + ":user")},
	}
	c.Volumes = []api.Volume{{Name: new("bind"), Host: &api.Host{SourcePath: new("/data")}}, {Name: new("scratch")}}
	c.MountPoints = []api.MountPoint{
		{SourceVolume: new("bind"), ContainerPath: new("/input"), ReadOnly: new(true)},
		{SourceVolume: new("scratch"), ContainerPath: new("/work")},
	}
	_, err = s.RegisterJobDefinition(ctx, def)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	handler, err := gateway.New(cfg, nil, stsapi.Descriptor(tokens))
	if err != nil {
		t.Fatal(err)
	}

	handler.UseCredentials(tokens.Credentials)
	handler.HandlePath("/_tinystack/ecs/", metadata.New(nil, nil, metadata.WithBatchContainerRole(s.ContainerRole),
		metadata.WithAssumeRole(tokens.AssumeRoleForService)).ECSHandler())
	endpoint := httptest.NewServer(handler)
	t.Cleanup(endpoint.Close)
	s.UseEndpoint(endpoint.URL)
	rt := &capturedRuntime{}
	s.runtime = rt
	s.bg.Stop()
	missingID := submitUnit(t, s, nil, 0)
	s.visitJobs(ctx)
	if stepErr := s.stepJob(ctx, &record{Scope: scope, ID: missingID}); stepErr != nil {
		t.Fatal(stepErr)
	}

	if missing := unitJob(t, s, missingID); common.Value(missing.Status) != statusFailed ||
		common.Value(missing.StatusReason) != "ResourceInitializationError: unable to resolve secret for TOKEN" ||
		len(rt.containers) != 0 {
		t.Fatalf("unresolvable secret started a container: %+v", missing)
	}

	id := submitUnit(t, s, nil, 0)
	_, err = params.PutParameter(ctx, &ssmapi.PutParameterRequest{
		Name: new("/test/value"), Type: new("SecureString"), Value: new("loaded-at-start"),
	})
	if err != nil {
		t.Fatal(err)
	}

	s.visitJobs(ctx)
	if stepErr := s.stepJob(ctx, &record{Scope: scope, ID: id}); stepErr != nil {
		t.Fatal(stepErr)
	}

	if len(rt.containers) != 1 {
		t.Fatalf("started %d containers, want 1", len(rt.containers))
	}

	spec := rt.containers[0]
	if spec.Environment["TOKEN"] != "loaded-at-start" || spec.Environment["USER"] != "local" ||
		spec.Environment["AWS_ENDPOINT_URL"] != endpoint.URL {
		t.Fatalf("unexpected container environment")
	}

	wantMounts := []ecs.MountSpec{{Source: "/data", Target: "/input", ReadOnly: true}, {Target: "/work"}}
	if !reflect.DeepEqual(spec.Mounts, wantMounts) {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}

	if job := unitJob(t, s, id); len(job.Container.Environment) != 1 ||
		common.Value(job.Container.Environment[0].Name) != "LOCAL" {
		t.Fatal("job description exposed runtime environment values")
	}

	provider := endpointcreds.New(
		spec.Environment["AWS_CONTAINER_CREDENTIALS_FULL_URI"],
		func(o *endpointcreds.Options) {
			o.HTTPClient = endpoint.Client()
			o.AuthorizationToken = spec.Environment["AWS_CONTAINER_AUTHORIZATION_TOKEN"]
		},
	)
	sdk := stssdk.New(stssdk.Options{
		Region: scope.Region, BaseEndpoint: new(endpoint.URL), Credentials: provider, Retryer: aws.NopRetryer{},
	})
	caller, err := sdk.GetCallerIdentity(ctx, &stssdk.GetCallerIdentityInput{})
	wantARN := "arn:aws:sts::000000000000:assumed-role/job/batch-job-" + id
	if err != nil || common.Value(caller.Arn) != wantARN {
		t.Fatalf("verified caller identity = %+v, want %s (%v)", caller, wantARN, err)
	}

	_, err = s.TerminateJob(ctx, &api.TerminateJobRequest{JobId: new(id), Reason: new("finished")})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/_tinystack/ecs/batch-credentials", http.NoBody)
	req.Header.Set("Authorization", spec.Environment["AWS_CONTAINER_AUTHORIZATION_TOKEN"])
	res := httptest.NewRecorder()
	endpoint.Config.Handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("terminated attempt's credential token returned %d", res.Code)
	}
}

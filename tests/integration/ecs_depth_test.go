package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestECSDepthRecordsAndValidation(t *testing.T) {
	s := server(t)
	c := ecsClient(s, "us-east-1")
	if _, err := c.CreateCluster(context.Background(), &ecs.CreateClusterInput{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := &ecs.RegisterTaskDefinitionInput{
		Family: new("depth"), Volumes: []ecstypes.Volume{
			{
				Name: new("data"),
				DockerVolumeConfiguration: &ecstypes.DockerVolumeConfiguration{
					Scope:  ecstypes.ScopeTask,
					Driver: new("local"),
				},
			},
		},
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name: new("web"), Image: new("never-pull-this-image"), Memory: new(int32(64)),
				HealthCheck: &ecstypes.HealthCheck{Command: []string{"CMD", "false"}},
				Secrets:     []ecstypes.Secret{{Name: new("TOKEN"), ValueFrom: new("/not-created")}},
				MountPoints: []ecstypes.MountPoint{{SourceVolume: new("data"), ContainerPath: new("/data")}},
			},
		},
	}
	if _, err := c.RegisterTaskDefinition(ctx, in); err != nil {
		t.Fatal(err)
	}

	run, err := c.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: new("depth")})
	if err != nil {
		t.Fatal(err)
	}

	got := describeTask(ctx, t, c, run.Tasks[0].TaskArn)
	if aws.ToString(got.LastStatus) != "RUNNING" || got.HealthStatus != ecstypes.HealthStatusUnknown {
		t.Fatalf("records task=%+v", got)
	}

	in.Family = new("cycle")
	in.ContainerDefinitions[0].DependsOn = []ecstypes.ContainerDependency{
		{ContainerName: new("web"), Condition: ecstypes.ContainerConditionStart},
	}
	_, err = c.RegisterTaskDefinition(ctx, in)
	code(t, err, "InvalidParameterException")
	in.ContainerDefinitions[0].DependsOn = nil
	in.Volumes = []ecstypes.Volume{{Name: new("data"), EfsVolumeConfiguration: &ecstypes.EFSVolumeConfiguration{
		FileSystemId: new("fs-123"),
	}}}
	_, err = c.RegisterTaskDefinition(ctx, in)
	code(t, err, "ValidationException")
	in.Volumes = []ecstypes.Volume{
		{
			Name: new("data"),
			FsxWindowsFileServerVolumeConfiguration: &ecstypes.FSxWindowsFileServerVolumeConfiguration{
				FileSystemId: new("fs-123"),
				AuthorizationConfig: &ecstypes.FSxWindowsFileServerAuthorizationConfig{
					CredentialsParameter: new("/fsx/creds"), Domain: new("example.test"),
				},
				RootDirectory: new("share"),
			},
		},
	}
	_, err = c.RegisterTaskDefinition(ctx, in)
	code(t, err, "ValidationException")
}

func TestECSDockerDepth(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run task containers in Docker")
	}

	cfg := testConfig()
	cfg.ECS.Containers = config.ContainersDocker
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	c := ecsClient(s, "us-east-1")
	if _, err := c.CreateCluster(context.Background(), &ecs.CreateClusterInput{}); err != nil {
		t.Fatal(err)
	}
	parameters := ssm.New(
		ssm.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: aws.CredentialsProviderFunc(
			func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
			})},
	)
	if _, err := parameters.PutParameter(ctx, &ssm.PutParameterInput{
		Name: new("/ecs/token"), Type: ssmtypes.ParameterTypeString, Value: new("resolved"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:  new("docker-depth"),
		Volumes: []ecstypes.Volume{{Name: new("data")}},
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:   new("web"),
				Image:  new("busybox"),
				Memory: new(int32(32)),
				Command: []string{"sh", "-c", "test $TOKEN = resolved; mkdir -p /www; echo ok >/www/index.html; " +
					"echo ok >/data/ready; httpd -f -p 8080 -h /www"},
				Secrets: []ecstypes.Secret{
					{Name: new("TOKEN"), ValueFrom: new("arn:aws:ssm:us-east-1:000000000000:parameter/ecs/token")},
				},
				MountPoints: []ecstypes.MountPoint{{SourceVolume: new("data"), ContainerPath: new("/data")}},
				HealthCheck: &ecstypes.HealthCheck{
					Command:  []string{"CMD-SHELL", "test -f /data/ready"},
					Interval: new(int32(5)), Retries: new(int32(1)),
				},
			},
			{
				Name:      new("client"),
				Image:     new("busybox"),
				Memory:    new(int32(32)),
				Essential: new(false),
				DependsOn: []ecstypes.ContainerDependency{
					{ContainerName: new("web"), Condition: ecstypes.ContainerConditionHealthy},
				},
				VolumesFrom: []ecstypes.VolumeFrom{{SourceContainer: new("web"), ReadOnly: new(true)}},
				Command:     []string{"sh", "-c", "test $(cat /data/ready) = ok && wget -qO- http://localhost:8080"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := c.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: new("docker-depth")})
	if err != nil {
		t.Fatal(err)
	}

	task := waitStatus(ctx, t, c, run.Tasks[0].TaskArn, "RUNNING")
	waitFor(t, "healthy task and successful localhost client", func() bool {
		task = describeTask(ctx, t, c, task.TaskArn)
		return task.HealthStatus == ecstypes.HealthStatusHealthy && task.Containers[1].ExitCode != nil
	})
	if aws.ToInt32(task.Containers[1].ExitCode) != 0 {
		t.Fatalf("client=%+v", task.Containers[1])
	}

	if _, err = c.StopTask(ctx, &ecs.StopTaskInput{Task: task.TaskArn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: new("missing-secret"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name: new("web"), Image: new("busybox"), Memory: new(int32(32)),
			Secrets: []ecstypes.Secret{{Name: new("TOKEN"), ValueFrom: new("/missing")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err = c.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: new("missing-secret")})
	if err != nil {
		t.Fatal(err)
	}

	task = waitStatus(ctx, t, c, run.Tasks[0].TaskArn, "STOPPED")
	if !strings.HasPrefix(aws.ToString(task.StoppedReason), "ResourceInitializationError:") {
		t.Fatalf("reason=%v", task.StoppedReason)
	}
}

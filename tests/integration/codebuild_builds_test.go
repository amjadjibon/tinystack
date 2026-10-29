package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	sdk "github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestCodeBuildBuildsRecordsAndProfile(t *testing.T) {
	s := server(t)
	c := codebuildClient(s, "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateProject(ctx, codebuildInput())
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: new("records-project")})
	code(t, err, "InvalidInputException")
	for _, body := range []string{
		`{}`,
		`{"projectName":"x","timeoutInMinutesOverride":0}`,
		`{"projectName":"x","environmentVariablesOverride":[{"name":"x"}]}`,
	} {
		status, out := codebuildWire(t, s, "POST", "/", "CodeBuild_20161006.StartBuild", body)
		if status != 400 || !strings.Contains(out["__type"].(string), "InvalidInputException") {
			t.Fatal(status, out)
		}
	}

	for _, body := range []string{
		`{"projectName":"x","sourceTypeOverride":"GITHUB"}`,
		`{"projectName":"x","cacheOverride":{}}`,
		`{"projectName":"x","logsConfigOverride":{"s3Logs":{}}}`,
	} {
		status, out := codebuildWire(t, s, "POST", "/", "CodeBuild_20161006.StartBuild", body)
		if status != 400 || !strings.Contains(out["__type"].(string), "ValidationException") {
			t.Fatal(status, out)
		}
	}

	out, err := c.BatchGetBuilds(ctx, &sdk.BatchGetBuildsInput{Ids: []string{"missing:id"}})
	if err != nil || len(out.BuildsNotFound) != 1 {
		t.Fatal(out, err)
	}

	_, err = c.StopBuild(ctx, &sdk.StopBuildInput{Id: new("missing:id")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.ListBuildsForProject(ctx, &sdk.ListBuildsForProjectInput{ProjectName: new("missing")})
	code(t, err, "ResourceNotFoundException")
}

func codebuildDockerServer(t *testing.T) (*httptest.Server, func(), config.Config, *sqlite.Store) {
	t.Helper()
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for CodeBuild containers")
	}

	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { store.Close() })
	cfg := testConfig()
	cfg.CodeBuild.Builds = config.BuildsDocker
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	return s, stop, cfg, store
}

func dockerBuildInput(name, spec string) *sdk.CreateProjectInput {
	in := codebuildInput()
	in.Name = new(name)
	in.Source = &types.ProjectSource{Type: types.SourceTypeNoSource, Buildspec: new(spec)}
	in.SourceVersion = nil
	in.Artifacts = &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts}
	in.Environment.Image = new("alpine:3.21")
	in.Environment.EnvironmentVariables = nil
	in.Environment.PrivilegedMode = new(false)
	return in
}

func waitCodeBuild(t *testing.T, c *sdk.Client, id string, status types.StatusType) types.Build {
	t.Helper()
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := c.BatchGetBuilds(context.Background(), &sdk.BatchGetBuildsInput{Ids: []string{id}})
		if err != nil || len(out.Builds) != 1 {
			t.Fatal(out, err)
		}

		b := out.Builds[0]
		if b.BuildComplete {
			if b.BuildStatus != status {
				for _, phase := range b.Phases {
					for _, failure := range phase.Contexts {
						t.Logf("%s: %s", phase.PhaseType, *failure.Message)
					}
				}

				t.Fatalf("build %s: %s phases=%+v", id, b.BuildStatus, b.Phases)
			}

			return b
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("build did not complete")
	return types.Build{}
}

func buildLogText(t *testing.T, s *httptest.Server, b *types.Build) string {
	t.Helper()
	out, err := logsClient(s, "test").GetLogEvents(context.Background(), &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: b.Logs.GroupName, LogStreamName: b.Logs.StreamName, StartFromHead: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	for _, event := range out.Events {
		text.WriteString(*event.Message)
		text.WriteByte('\n')
	}

	return text.String()
}

func TestCodeBuildDockerInlineLogsEnvironmentAndHistory(t *testing.T) {
	s, stop, _, _ := codebuildDockerServer(t)
	defer stop()
	ctx := context.Background()
	c := codebuildClient(s, "test", "us-east-1")
	events := eventsClient(s, "test")
	queues := sqsClient(s, "test")
	queueURL, queue := queueARN(t, queues, "build-events")
	putRule(t, events, "build-events", `{"source":["aws.codebuild"]}`)
	_, err := events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:    new("build-events"),
		Targets: []ebtypes.Target{{Id: new("queue"), Arn: new(queue)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = ssmClient(
		s,
		"test",
		"us-east-1",
	).PutParameter(ctx, &ssm.PutParameterInput{
		Name: new("/build/key"), Value: new("parameter-value"), Type: ssmtypes.ParameterTypeSecureString,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = secretsClient(
		s,
		"test",
		"us-east-1",
	).CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: new("build-secret"), SecretString: new(`{"password":"secret-value"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	spec := `version: 0.2
env:
  variables:
    MODE: buildspec
  parameter-store:
    PARAM: /build/key
  secrets-manager:
    SECRET: build-secret:password
phases:
  install:
    commands:
      - export KEEP=retained
      - mkdir sub
      - cd sub
  build:
    commands:
      - test "$KEEP" = retained
      - test "$MODE" = override
      - test "$PARAM" = parameter-value
      - test "$SECRET" = secret-value
      - test "$CODEBUILD_BUILD_SUCCEEDING" = 1
      - test "$AWS_REGION" = us-east-1
      - test "$AWS_DEFAULT_REGION" = us-east-1
      - test "$CODEBUILD_BUILD_NUMBER" = 1
      - test -n "$CODEBUILD_BUILD_ARN"
      - test -n "$AWS_ENDPOINT_URL"
      - wget -q -T 10 -O /dev/null "$AWS_ENDPOINT_URL/_tinystack/health"
      - echo inline-success
  post_build:
    commands:
      - echo post-success
`
	in := dockerBuildInput("inline-build", spec)
	in.Environment.EnvironmentVariables = []types.EnvironmentVariable{
		{Name: new("MODE"), Value: new("project"), Type: types.EnvironmentVariableTypePlaintext},
	}

	_, err = c.CreateProject(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.StartBuild(
		ctx,
		&sdk.StartBuildInput{
			ProjectName: in.Name,
			EnvironmentVariablesOverride: []types.EnvironmentVariable{
				{Name: new("MODE"), Value: new("override"), Type: types.EnvironmentVariableTypePlaintext},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	b := waitCodeBuild(t, c, *out.Build.Id, types.StatusTypeSucceeded)
	if text := buildLogText(
		t,
		s,
		new(b),
	); !strings.Contains(text, "inline-success") ||
		!strings.Contains(text, "post-success") {
		t.Fatal(text)
	}

	messages, err := queues.ReceiveMessage(
		ctx,
		&sqs.ReceiveMessageInput{QueueUrl: new(queueURL), MaxNumberOfMessages: 10},
	)
	if err != nil {
		t.Fatal(err)
	}

	var eventText strings.Builder
	for _, message := range messages.Messages {
		eventText.WriteString(*message.Body)
	}

	if !strings.Contains(eventText.String(), "CodeBuild Build State Change") ||
		!strings.Contains(eventText.String(), "CodeBuild Build Phase Change") {
		t.Fatal("missing build events", eventText.String())
	}

	list, err := c.ListBuildsForProject(ctx, &sdk.ListBuildsForProjectInput{ProjectName: in.Name})
	if err != nil || len(list.Ids) != 1 || list.Ids[0] != *b.Id {
		t.Fatal(list, err)
	}

	all, err := c.ListBuilds(ctx, &sdk.ListBuildsInput{})
	if err != nil || len(all.Ids) != 1 {
		t.Fatal(all, err)
	}

	foreign, err := codebuildClient(
		s,
		"team-b",
		"us-east-1",
	).BatchGetBuilds(ctx, &sdk.BatchGetBuildsInput{Ids: []string{*b.Id}})
	if err != nil || len(foreign.BuildsNotFound) != 1 {
		t.Fatal(foreign, err)
	}

	deleted, err := c.BatchDeleteBuilds(ctx, &sdk.BatchDeleteBuildsInput{Ids: []string{*b.Id}})
	if err != nil || len(deleted.BuildsDeleted) != 1 {
		t.Fatal(deleted, err)
	}

	missing, err := c.BatchGetBuilds(ctx, &sdk.BatchGetBuildsInput{Ids: []string{*b.Id}})
	if err != nil || len(missing.BuildsNotFound) != 1 {
		t.Fatal(missing, err)
	}
}

func TestCodeBuildDockerS3SourceAndArtifact(t *testing.T) {
	s, stop, _, _ := codebuildDockerServer(t)
	defer stop()
	ctx := context.Background()
	c := codebuildClient(s, "test", "us-east-1")
	objects := s3Client(s, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("build-files")})
	if err != nil {
		t.Fatal(err)
	}

	var source bytes.Buffer
	archive := zip.NewWriter(&source)
	for name, value := range map[string]string{"config/build.yml": `version: 0.2
phases:
  build:
    commands: ['mkdir -p out/nested', 'cp input.txt out/nested/result.txt']
artifacts:
  files: ['**/*']
  base-directory: out
  discard-paths: true
`, "input.txt": "source-content"} {
		writer, e := archive.Create(name)
		if e != nil {
			t.Fatal(e)
		}

		if _, e = io.WriteString(writer, value); e != nil {
			t.Fatal(e)
		}
	}

	if err = archive.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = objects.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new("build-files"), Key: new("source.zip"), Body: bytes.NewReader(source.Bytes())},
	)
	if err != nil {
		t.Fatal(err)
	}

	in := dockerBuildInput("s3-build", "")
	in.Source = &types.ProjectSource{
		Type:      types.SourceTypeS3,
		Location:  new("build-files/source.zip"),
		Buildspec: new("config/build.yml"),
	}

	in.Artifacts = &types.ProjectArtifacts{
		Type:          types.ArtifactsTypeS3,
		Location:      new("build-files"),
		Name:          new("output.zip"),
		Path:          new("artifacts"),
		NamespaceType: types.ArtifactNamespaceBuildId,
		Packaging:     types.ArtifactPackagingZip,
	}

	_, err = c.CreateProject(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	started, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: in.Name})
	if err != nil {
		t.Fatal(err)
	}

	b := waitCodeBuild(t, c, *started.Build.Id, types.StatusTypeSucceeded)
	key := strings.TrimPrefix(*b.Artifacts.Location, "arn:aws:s3:::build-files/")
	output, err := objects.GetObject(ctx, &s3.GetObjectInput{Bucket: new("build-files"), Key: new(key)})
	if err != nil {
		t.Fatal(err)
	}

	defer output.Body.Close()
	data, err := io.ReadAll(output.Body)
	if err != nil {
		t.Fatal(err)
	}

	zipOut, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zipOut.File) != 1 || zipOut.File[0].Name != "result.txt" {
		t.Fatal(zipOut, err)
	}

	reader, err := zipOut.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}

	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil || string(content) != "source-content" {
		t.Fatal(string(content), err)
	}
}

func TestCodeBuildDockerFailureStopAndRestart(t *testing.T) {
	s, stop, cfg, store := codebuildDockerServer(t)
	//nolint:gocritic // Follow the replacement stop function after each server restart.
	defer func() { stop() }()
	ctx := context.Background()
	c := codebuildClient(s, "test", "us-east-1")
	in := dockerBuildInput("failure-build", `version: 0.2
phases:
  build:
    commands: ['echo before-failure', 'false', 'echo unreachable']
    finally: ['echo finally-ran']
  post_build:
    commands: ['echo post-$CODEBUILD_BUILD_SUCCEEDING']
`)
	_, err := c.CreateProject(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	started, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: in.Name})
	if err != nil {
		t.Fatal(err)
	}

	b := waitCodeBuild(t, c, *started.Build.Id, types.StatusTypeFailed)
	text := buildLogText(t, s, new(b))
	if !strings.Contains(text, "post-0") || !strings.Contains(text, "finally-ran") ||
		strings.Contains(text, "unreachable") {
		t.Fatal(text)
	}

	stop()
	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = codebuildClient(s, "test", "us-east-1")
	persisted, err := c.BatchGetBuilds(ctx, &sdk.BatchGetBuildsInput{Ids: []string{*b.Id}})
	if err != nil || len(persisted.Builds) != 1 || persisted.Builds[0].BuildStatus != types.StatusTypeFailed {
		t.Fatal(persisted, err)
	}

	longSpec := "version: 0.2\nphases:\n  build:\n    commands: ['echo started', 'sleep 600']\n"
	started, err = c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: in.Name, BuildspecOverride: new(longSpec)})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for BUILD so this exercises killing an actual container.
	deadline := time.Now().Add(time.Minute)
	for {
		got, e := c.BatchGetBuilds(ctx, &sdk.BatchGetBuildsInput{Ids: []string{*started.Build.Id}})
		if e != nil {
			t.Fatal(e)
		}

		if *got.Builds[0].CurrentPhase == "BUILD" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("container did not start", got)
		}

		time.Sleep(100 * time.Millisecond)
	}

	_, err = c.StopBuild(ctx, &sdk.StopBuildInput{Id: started.Build.Id})
	if err != nil {
		t.Fatal(err)
	}

	waitCodeBuild(t, c, *started.Build.Id, types.StatusTypeStopped)
	// Shutdown interrupts another running build; restart retains FAILED with an explanatory context.
	running, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: in.Name, BuildspecOverride: new(longSpec)})
	if err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(time.Minute)
	for {
		got, getErr := c.BatchGetBuilds(ctx, &sdk.BatchGetBuildsInput{Ids: []string{*running.Build.Id}})
		if getErr != nil {
			t.Fatal(getErr)
		}

		if *got.Builds[0].CurrentPhase == "BUILD" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("container did not start", got)
		}

		time.Sleep(100 * time.Millisecond)
	}

	stop()
	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = codebuildClient(s, "test", "us-east-1")
	waitCodeBuild(t, c, *running.Build.Id, types.StatusTypeFailed)
	// Reset clears history after canceling workers and removing their containers.
	req, err := http.NewRequest(http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
}

func TestCodeBuildDockerTimeout(t *testing.T) {
	s, stop, _, _ := codebuildDockerServer(t)
	defer stop()
	ctx := context.Background()
	c := codebuildClient(s, "test", "us-east-1")
	in := dockerBuildInput("timeout-build", "version: 0.2\nphases:\n  build:\n    commands: ['sleep 600']\n")
	_, err := c.CreateProject(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	started, err := c.StartBuild(
		ctx,
		&sdk.StartBuildInput{ProjectName: in.Name, TimeoutInMinutesOverride: new(int32(5))},
	)
	if err != nil {
		t.Fatal(err)
	}

	b := waitCodeBuild(t, c, *started.Build.Id, types.StatusTypeTimedOut)
	if b.EndTime == nil || b.EndTime.Sub(*b.StartTime) < 5*time.Minute {
		t.Fatal("timeout fired too early", b)
	}
}

func TestCodeBuildDockerValidationAndMissingInputs(t *testing.T) {
	s, stop, _, _ := codebuildDockerServer(t)
	defer stop()
	c := codebuildClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := dockerBuildInput("validation-build", "version: 0.2\nphases: {}\n")
	if _, err := c.CreateProject(ctx, in); err != nil {
		t.Fatal(err)
	}

	for _, spec := range []string{
		"version: 0.1\nphases: {}", "version: 0.2\nreports: {}",
		"version: 0.2\nphases:\n  build:\n    on-failure: RETRY",
	} {
		_, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: in.Name, BuildspecOverride: new(spec)})
		code(t, err, "InvalidInputException")
	}

	_, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: new("missing")})
	code(t, err, "ResourceNotFoundException")
	for _, kind := range []types.SourceType{types.SourceTypeGithub, types.SourceTypeCodecommit} {
		in := codebuildInput()
		in.Name = new("unsupported-" + strings.ToLower(string(kind)))
		in.Source.Type = kind
		if kind == types.SourceTypeCodecommit {
			in.Source.Location = new("https://git-codecommit.us-east-1.amazonaws.com/v1/repos/example")
		}

		if _, err = c.CreateProject(ctx, in); err != nil {
			t.Fatal(err)
		}

		_, err = c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: in.Name})
		code(t, err, "InvalidInputException")
	}

	for _, kind := range []types.EnvironmentVariableType{
		types.EnvironmentVariableTypeParameterStore, types.EnvironmentVariableTypeSecretsManager,
	} {
		started, e := c.StartBuild(ctx, &sdk.StartBuildInput{
			ProjectName: new("validation-build"),
			EnvironmentVariablesOverride: []types.EnvironmentVariable{
				{Name: new("MISSING"), Value: new("missing-value"), Type: kind},
			},
		})
		if e != nil {
			t.Fatal(e)
		}

		b := waitCodeBuild(t, c, *started.Build.Id, types.StatusTypeFailed)
		found := false
		for _, phase := range b.Phases {
			if phase.PhaseType == types.BuildPhaseTypeDownloadSource && phase.PhaseStatus == types.StatusTypeFailed {
				found = true
			}
		}

		if !found {
			t.Fatal("missing value did not fail DOWNLOAD_SOURCE", b.Phases)
		}
	}
}

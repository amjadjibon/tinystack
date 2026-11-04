package codebuild

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// shellRuntime replaces Docker with a local shell; it exercises the same generated script and record transitions.
type shellRuntime struct{ output []string }

func (r *shellRuntime) Clean(context.Context, string) error { return nil }
func (r *shellRuntime) Run(ctx context.Context, spec *RunSpec, emit func(string) error) error {
	cmd := exec.CommandContext(ctx, "/bin/sh")
	cmd.Stdin = strings.NewReader(spec.Script)
	cmd.Env = os.Environ()
	for k, v := range spec.Environment {
		if k == "CODEBUILD_SRC_DIR" {
			v = spec.Directory
		}

		cmd.Env = append(cmd.Env, k+"="+v)
	}

	data, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(data), "\n") {
		r.output = append(r.output, line)
		if e := emit(line); e != nil {
			return e
		}
	}

	return err
}

func TestBuildspecStrictParsing(t *testing.T) {
	for _, value := range []string{
		"version: 0.1", "version: 0.2\nreports: {bad: {files: []}}", "version: 0.2\ncache: {}",
		"version: 0.2\nenv:\n  shell: powershell", "version: 0.2\nphases:\n  build:\n    on-failure: RETRY",
		"version: 0.2\nphases:\n  install:\n    runtime-versions: {nodejs: 20}",
		"version: 0.2\nenv:\n  variables: {CODEBUILD_BUILD_ID: forged}",
		"version: 0.2\nartifacts:\n  files: ['../secret']", "version: 0.2\n---\nversion: 0.2",
	} {
		if _, err := parseBuildspec([]byte(value)); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestBuildspecPhaseSemantics(t *testing.T) {
	for _, test := range []struct {
		name, phases, want, absent string
		status                     string
	}{
		{"build failure", `  install:
    commands: ['export KEEP=yes', 'mkdir sub', 'cd sub']
  build:
    commands: ['test "$KEEP" = yes', 'false', 'echo unreachable']
    finally: ['echo finally-build']
  post_build:
    commands: ['echo post-$CODEBUILD_BUILD_SUCCEEDING', 'pwd']`, "post-0", "unreachable", "FAILED"},
		{"install failure", `  install:
    commands: ['false']
    finally: ['echo cleanup']
  build:
    commands: ['echo unreachable']
  post_build:
    commands: ['echo post-$CODEBUILD_BUILD_SUCCEEDING']`, "cleanup", "unreachable", "FAILED"},
		{"continue", `  install:
    on-failure: CONTINUE
    commands: ['false']
  build:
    commands: ['echo continued-$CODEBUILD_BUILD_SUCCEEDING']`, "continued-0", "unreachable", "FAILED"},
		{"success", `  build:
    commands: ['export KEEP=works']
  post_build:
    commands: ['echo $KEEP-$CODEBUILD_BUILD_SUCCEEDING']`, "works-1", "unreachable", "SUCCEEDED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := New(&memory.Store{})
			rt := &shellRuntime{}
			if err := s.UseRuntime(context.Background(), rt, "http://host.docker.internal:12345", nil); err != nil {
				t.Fatal(err)
			}

			defer s.Stop()
			in := projectInput()
			in.Source = &api.ProjectSource{Type: new(noSource), Buildspec: new("version: 0.2\nphases:\n" + test.phases)}
			in.SourceVersion = nil
			in.Artifacts = &api.ProjectArtifacts{Type: new(noArtifacts)}
			in.Environment.EnvironmentVariables = nil
			if _, err := s.CreateProject(localContext(), in); err != nil {
				t.Fatal(err)
			}

			out, err := s.StartBuild(localContext(), &api.StartBuildInput{ProjectName: in.Name})
			if err != nil {
				t.Fatal(err)
			}

			s.claim(
				context.Background(),
				indexKey(request.Scope{AccountID: "000000000000", Region: "us-east-1"}, common.Value(out.Build.Id)),
			)
			got, err := s.BatchGetBuilds(localContext(), &api.BatchGetBuildsInput{Ids: []string{*out.Build.Id}})
			if err != nil {
				t.Fatal(err)
			}

			if common.Value(got.Builds[0].BuildStatus) != test.status {
				t.Fatalf("%+v", got.Builds[0])
			}

			output := strings.Join(rt.output, "\n")
			if !strings.Contains(output, test.want) || strings.Contains(output, test.absent) {
				t.Fatal(output)
			}

			for _, phase := range got.Builds[0].Phases {
				if common.Value(phase.PhaseType) != "COMPLETED" &&
					(phase.EndTime == nil || phase.DurationInSeconds == nil) {
					t.Fatalf("unfinished phase %+v", phase)
				}
			}
		})
	}
}

func TestBuildModelConstraints(t *testing.T) {
	s := New(&memory.Store{})
	for _, test := range []struct {
		in any
		op string
	}{
		{&api.StartBuildInput{}, "StartBuild"},
		{&api.StartBuildInput{ProjectName: new("project"), TimeoutInMinutesOverride: new(int64(0))}, "StartBuild"},
		{&api.StartBuildInput{ProjectName: new("project"), EnvironmentVariablesOverride: []api.EnvironmentVariable{
			{Name: new("MISSING_VALUE")},
		}}, "StartBuild"},
		{&api.BatchGetBuildsInput{}, "BatchGetBuilds"},
		{&api.BatchDeleteBuildsInput{Ids: make([]string, 101)}, "BatchDeleteBuilds"},
		{&api.StopBuildInput{}, "StopBuild"},
		{&api.ListBuildsForProjectInput{}, "ListBuildsForProject"},
		{&api.ListBuildsInput{SortOrder: new("INVALID")}, "ListBuilds"},
	} {
		_, err := api.Descriptor(s).Operations[test.op].Invoke(localContext(), test.in)
		wantError(t, err, "InvalidInputException")
	}
}

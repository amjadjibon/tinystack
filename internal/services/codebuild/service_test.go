package codebuild

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func buildContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func localContext() context.Context { return buildContext("000000000000", "us-east-1") }
func projectInput() *api.CreateProjectInput {
	return &api.CreateProjectInput{
		Name: new(
			"project",
		),
		Description: new("retained"),
		ServiceRole: new("arn:aws:iam::000000000000:role/build/path/worker"),
		Source: &api.ProjectSource{
			Type:          new(githubSource),
			Location:      new("https://github.com/example/repository.git"),
			Buildspec:     new("config/buildspec.yml"),
			GitCloneDepth: new(int64(3)),
		},
		SourceVersion: new(
			"main",
		),
		Artifacts: &api.ProjectArtifacts{
			Type:                 new(s3Type),
			Location:             new("output-bucket"),
			Name:                 new("artifact.zip"),
			Path:                 new("results"),
			NamespaceType:        new("BUILD_ID"),
			Packaging:            new("ZIP"),
			OverrideArtifactName: new(false),
			EncryptionDisabled:   new(true),
		},
		Environment: &api.ProjectEnvironment{
			Type:           new("LINUX_CONTAINER"),
			ComputeType:    new("BUILD_GENERAL1_MEDIUM"),
			Image:          new("aws/codebuild/standard:7.0"),
			PrivilegedMode: new(true),
			EnvironmentVariables: []api.EnvironmentVariable{
				{Name: new("MODE"), Value: new("local")},
				{Name: new("PARAM"), Value: new("/local/key"), Type: new("PARAMETER_STORE")},
				{Name: new("SECRET"), Value: new("local-secret:password"), Type: new("SECRETS_MANAGER")},
			},
		},
		TimeoutInMinutes: new(
			int64(35),
		),
		QueuedTimeoutInMinutes: new(int64(25)),
		Tags:                   []api.Tag{{Key: new("owner"), Value: new("platform")}},
	}
}

func wantError(t *testing.T, e error, code string) {
	t.Helper()
	var aws *awserror.Error
	if !errors.As(e, &aws) || aws.Code != code || aws.StatusCode != 400 {
		t.Fatalf("want %s/400, got %v", code, e)
	}
}

func getProject(t *testing.T, s *Service, ctx context.Context, name string) *api.Project {
	t.Helper()
	out, e := s.BatchGetProjects(ctx, &api.BatchGetProjectsInput{Names: []string{name}})
	if e != nil || len(out.Projects) != 1 {
		t.Fatalf("get %v %v", out, e)
	}

	return &out.Projects[0]
}

func TestProjectCRUDDefaultsTagsAndAtomicUpdates(t *testing.T) {
	s := New(&memory.Store{})
	ctx := localContext()
	in := projectInput()
	out, e := s.CreateProject(ctx, in)
	if e != nil {
		t.Fatal(e)
	}

	before := common.Clone(out.Project)
	if *before.Arn != "arn:aws:codebuild:us-east-1:000000000000:project/project" ||
		*before.Created != *before.LastModified ||
		*before.Created <= 0 {
		t.Fatal(before)
	}

	if !reflect.DeepEqual(getProject(t, s, ctx, *before.Arn), before) {
		t.Fatal("read changed complete metadata")
	}

	*in.Source.Location = "mutated"
	*out.Project.Environment.EnvironmentVariables[0].Value = "mutated"
	if !reflect.DeepEqual(getProject(t, s, ctx, "project"), before) {
		t.Fatal("input/output aliased stored record")
	}

	_, e = s.CreateProject(ctx, projectInput())
	wantError(t, e, "ResourceAlreadyExistsException")
	_, e = s.UpdateProject(ctx, &api.UpdateProjectInput{Name: new("missing"), Description: new("x")})
	wantError(t, e, "ResourceNotFoundException")
	_, e = s.UpdateProject(
		ctx,
		&api.UpdateProjectInput{
			Name:             new("project"),
			Description:      new("bad"),
			TimeoutInMinutes: new(int64(4)),
			Tags:             []api.Tag{{Key: new("changed"), Value: new("bad")}},
		},
	)
	wantError(t, e, "InvalidInputException")
	if !reflect.DeepEqual(getProject(t, s, ctx, "project"), before) {
		t.Fatal("failed effective-candidate update mutated metadata/timestamp")
	}

	updated, e := s.UpdateProject(
		ctx,
		&api.UpdateProjectInput{
			Name:        new("project"),
			Description: new(""),
			Tags:        []api.Tag{{Key: new("new"), Value: new("")}},
		},
	)
	if e != nil {
		t.Fatal(e)
	}

	if len(updated.Project.Tags) != 1 || *updated.Project.Tags[0].Key != "new" ||
		*updated.Project.Created != *before.Created ||
		*updated.Project.LastModified <= *before.LastModified ||
		*updated.Project.Arn != *before.Arn {
		t.Fatal("update failed identity/tag replacement", updated)
	}

	cleared, e := s.UpdateProject(ctx, &api.UpdateProjectInput{Name: new("project"), Tags: []api.Tag{}})
	if e != nil || len(cleared.Project.Tags) != 0 {
		t.Fatalf("clear tags %v %v", cleared, e)
	}

	if _, e = s.DeleteProject(ctx, &api.DeleteProjectInput{Name: new("project")}); e != nil {
		t.Fatal(e)
	}

	if _, e = s.DeleteProject(ctx, &api.DeleteProjectInput{Name: new("project")}); e != nil {
		t.Fatal("missing delete must be idempotent", e)
	}

	defaults := projectInput()
	defaults.Name = new("defaults")
	defaults.TimeoutInMinutes = nil
	defaults.QueuedTimeoutInMinutes = nil
	defaults.Source = &api.ProjectSource{Type: new(noSource), Buildspec: new("version: 0.2\nphases: {}")}
	defaults.SourceVersion = nil
	defaults.Artifacts = &api.ProjectArtifacts{Type: new(noArtifacts)}
	defaults.Environment.PrivilegedMode = nil
	d, e := s.CreateProject(ctx, defaults)
	if e != nil {
		t.Fatal(e)
	}

	if *d.Project.TimeoutInMinutes != 60 || *d.Project.QueuedTimeoutInMinutes != 60 ||
		*d.Project.Environment.PrivilegedMode ||
		*d.Project.Environment.EnvironmentVariables[0].Type != plaintext {
		t.Fatal("incorrect defaults", d)
	}
}

func TestProjectValidation(t *testing.T) {
	tests := map[string]func(*api.CreateProjectInput){
		"missing name": func(p *api.CreateProjectInput) {
			p.Name = nil
		},
		"short name": func(p *api.CreateProjectInput) {
			p.Name = new("a")
		},
		"leading hyphen": func(p *api.CreateProjectInput) {
			p.Name = new("-name")
		},
		"long name": func(p *api.CreateProjectInput) {
			p.Name = new(strings.Repeat("a", 151))
		},
		"role missing": func(p *api.CreateProjectInput) {
			p.ServiceRole = nil
		},
		"foreign role": func(p *api.CreateProjectInput) {
			p.ServiceRole = new("arn:aws:iam::111111111111:role/build")
		},
		"role name": func(p *api.CreateProjectInput) {
			p.ServiceRole = new("build")
		},
		"role malformed": func(p *api.CreateProjectInput) {
			p.ServiceRole = new("arn:aws:iam::000000000000:role/../worker")
		},
		"description": func(p *api.CreateProjectInput) {
			p.Description = new(strings.Repeat("x", 256))
		},
		"timeout": func(p *api.CreateProjectInput) {
			p.TimeoutInMinutes = new(int64(2161))
		},
		"queued timeout": func(p *api.CreateProjectInput) {
			p.QueuedTimeoutInMinutes = new(int64(481))
		},
		"source missing": func(p *api.CreateProjectInput) {
			p.Source = nil
		},
		"source unsupported": func(p *api.CreateProjectInput) {
			p.Source.Type = new("CODEPIPELINE")
		},
		"source location": func(p *api.CreateProjectInput) {
			p.Source.Location = nil
		},
		"source url": func(p *api.CreateProjectInput) {
			p.Source.Location = new("https://github.com/owner/repo?token=x")
		},
		"git depth": func(p *api.CreateProjectInput) {
			p.Source.GitCloneDepth = new(int64(-1))
		},
		"no source buildspec": func(p *api.CreateProjectInput) {
			p.Source = &api.ProjectSource{Type: new(noSource)}
		},
		"no source path": func(p *api.CreateProjectInput) {
			p.Source = &api.ProjectSource{Type: new(noSource), Buildspec: new("buildspec.yml")}
		},
		"no source version": func(p *api.CreateProjectInput) {
			p.Source = &api.ProjectSource{Type: new(noSource), Buildspec: new("version: 0.2\nphases: {}")}
		},
		"foreign buildspec": func(p *api.CreateProjectInput) {
			p.Source.Buildspec = new("arn:aws-cn:s3:::bucket/config.yml")
		},
		"s3 source": func(p *api.CreateProjectInput) {
			p.Source = &api.ProjectSource{Type: new(s3Type), Location: new("arn:aws:s3:::bucket/input.zip")}
		},
		"s3 git depth": func(p *api.CreateProjectInput) {
			p.Source = &api.ProjectSource{
				Type:          new(s3Type),
				Location:      new("source-bucket/input.zip"),
				GitCloneDepth: new(int64(0)),
			}
		},
		"foreign codecommit": func(p *api.CreateProjectInput) {
			p.Source = &api.ProjectSource{
				Type:     new(commitSource),
				Location: new("https://git-codecommit.us-west-2.amazonaws.com/v1/repos/repository"),
			}
		},
		"artifacts missing": func(p *api.CreateProjectInput) {
			p.Artifacts = nil
		},
		"artifact type": func(p *api.CreateProjectInput) {
			p.Artifacts.Type = new("CODEPIPELINE")
		},
		"artifact bucket": func(p *api.CreateProjectInput) {
			p.Artifacts.Location = new("arn:aws:s3:::output-bucket")
		},
		"artifact name": func(p *api.CreateProjectInput) {
			p.Artifacts.Name = nil
		},
		"artifact path": func(p *api.CreateProjectInput) {
			p.Artifacts.Path = new("../bad")
		},
		"artifact namespace": func(p *api.CreateProjectInput) {
			p.Artifacts.NamespaceType = new("bad")
		},
		"artifact packaging": func(p *api.CreateProjectInput) {
			p.Artifacts.Packaging = new("bad")
		},
		"no artifact false": func(p *api.CreateProjectInput) {
			p.Artifacts = &api.ProjectArtifacts{Type: new(noArtifacts), EncryptionDisabled: new(false)}
		},
		"environment missing": func(p *api.CreateProjectInput) {
			p.Environment = nil
		},
		"environment type": func(p *api.CreateProjectInput) {
			p.Environment.Type = new("ARM_CONTAINER")
		},
		"compute combination": func(p *api.CreateProjectInput) {
			p.Environment.ComputeType = new("BUILD_LAMBDA_1GB")
		},
		"image missing": func(p *api.CreateProjectInput) {
			p.Environment.Image = nil
		},
		"image invalid": func(p *api.CreateProjectInput) {
			p.Environment.Image = new("https://image:tag")
		},
		"variable value": func(p *api.CreateProjectInput) {
			p.Environment.EnvironmentVariables[0].Value = nil
		},
		"variable reserved": func(p *api.CreateProjectInput) {
			p.Environment.EnvironmentVariables[0].Name = new("CODEBUILD_KEY")
		},
		"variable duplicate": func(p *api.CreateProjectInput) {
			p.Environment.EnvironmentVariables = append(
				p.Environment.EnvironmentVariables,
				p.Environment.EnvironmentVariables[0],
			)
		},
		"variable type": func(p *api.CreateProjectInput) {
			p.Environment.EnvironmentVariables[0].Type = new("UNKNOWN")
		},
		"parameter ARN": func(p *api.CreateProjectInput) {
			p.Environment.EnvironmentVariables[1].Value = new("arn:aws:ssm:us-east-1:111111111111:parameter/key")
		},
		"tag reserved": func(p *api.CreateProjectInput) {
			p.Tags[0].Key = new("AWS:key")
		},
		"tag duplicate": func(p *api.CreateProjectInput) {
			p.Tags = append(p.Tags, p.Tags[0])
		},
		"tag missing": func(p *api.CreateProjectInput) {
			p.Tags[0].Value = nil
		},
		"tag key length": func(p *api.CreateProjectInput) {
			p.Tags[0].Key = new(strings.Repeat("x", 128))
		},
		"tag value length": func(p *api.CreateProjectInput) {
			p.Tags[0].Value = new(strings.Repeat("x", 256))
		},
		"tag chars": func(p *api.CreateProjectInput) {
			p.Tags[0].Value = new("bad!")
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := New(&memory.Store{})
			p := projectInput()
			mutate(p)
			_, e := s.CreateProject(localContext(), p)
			wantError(t, e, "InvalidInputException")
			out, e := s.ListProjects(localContext(), &api.ListProjectsInput{})
			if e != nil || len(out.Projects) != 0 {
				t.Fatal("invalid create leaked state", out, e)
			}
		})
	}
}

func TestProjectTagLimitsAndSourceVariants(t *testing.T) {
	s := New(&memory.Store{})
	ctx := localContext()
	in := projectInput()
	in.Tags = nil
	for i := range 50 {
		in.Tags = append(in.Tags, api.Tag{Key: new(fmt.Sprintf("key%02d", i)), Value: new("value")})
	}

	out, e := s.CreateProject(ctx, in)
	if e != nil {
		t.Fatal(e)
	}

	tags := []api.Tag{}
	for i := range 50 {
		tags = append(tags, api.Tag{Key: new(fmt.Sprintf("other%02d", i)), Value: new("new")})
	}

	u, e := s.UpdateProject(ctx, &api.UpdateProjectInput{Name: in.Name, Tags: tags})
	if e != nil || len(u.Project.Tags) != 50 {
		t.Fatalf("disjoint replacement %v %v", u, e)
	}

	before := common.Clone(u.Project)
	tags = append(tags, api.Tag{Key: new("overflow"), Value: new("")})
	_, e = s.UpdateProject(ctx, &api.UpdateProjectInput{Name: in.Name, Description: new("fail"), Tags: tags})
	wantError(t, e, "InvalidInputException")
	if !reflect.DeepEqual(before, getProject(t, s, ctx, *out.Project.Name)) {
		t.Fatal("failed tags changed config")
	}

	sources := []*api.ProjectSource{
		{Type: new(s3Type), Location: new("source-bucket/folder/")},
		{
			Type: new(commitSource), Location: new("https://git-codecommit.us-east-1.amazonaws.com/v1/repos/worker"),
			GitCloneDepth: new(int64(0)),
		},
	}

	for _, source := range sources {
		u, e = s.UpdateProject(ctx, &api.UpdateProjectInput{Name: in.Name, Source: source, SourceVersion: new("")})
		if e != nil || !reflect.DeepEqual(u.Project.Source, source) {
			t.Fatalf("source retention %v %v", u, e)
		}
	}

	_, e = s.DeleteProject(ctx, &api.DeleteProjectInput{Name: out.Project.Arn})
	wantError(t, e, "InvalidInputException")
	_, e = s.UpdateProject(ctx, &api.UpdateProjectInput{Name: out.Project.Arn})
	wantError(t, e, "InvalidInputException")
}

func TestProjectPaginationScopeSortingAndConcurrency(t *testing.T) {
	s := New(&memory.Store{})
	ctx := localContext()
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Go(func() { _, e := s.CreateProject(ctx, projectInput()); results <- e })
	}

	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else {
			wantError(t, e, "ResourceAlreadyExistsException")
		}
	}

	if success != 1 {
		t.Fatal("concurrent name uniqueness", success)
	}

	for i := range 201 {
		p := projectInput()
		p.Name = new(fmt.Sprintf("p%03d", i))
		if _, e := s.CreateProject(ctx, p); e != nil {
			t.Fatal(e)
		}
	}

	first, e := s.ListProjects(ctx, &api.ListProjectsInput{})
	if e != nil || len(first.Projects) != 100 || first.NextToken == nil || first.Projects[0] != "p000" {
		t.Fatalf("page1 %v %v", first, e)
	}

	if _, e = s.DeleteProject(ctx, &api.DeleteProjectInput{Name: new("p099")}); e != nil {
		t.Fatal(e)
	}

	next, e := s.ListProjects(ctx, &api.ListProjectsInput{NextToken: first.NextToken})
	if e != nil || len(next.Projects) != 100 || next.Projects[0] != "p100" {
		t.Fatalf("resume deleted cursor %v %v", next, e)
	}

	last, e := s.ListProjects(ctx, &api.ListProjectsInput{NextToken: next.NextToken})
	if e != nil || len(last.Projects) != 2 || last.NextToken != nil {
		t.Fatalf("last page %v %v", last, e)
	}

	for _, other := range []context.Context{
		buildContext("111111111111", "us-east-1"),
		buildContext("000000000000", "us-west-2"),
	} {
		_, e = s.ListProjects(other, &api.ListProjectsInput{NextToken: first.NextToken})
		wantError(t, e, "InvalidInputException")
		empty, err := s.ListProjects(other, &api.ListProjectsInput{})
		if err != nil || len(empty.Projects) != 0 {
			t.Fatal("scope leak")
		}
	}

	_, e = s.ListProjects(ctx, &api.ListProjectsInput{SortOrder: new("DESCENDING"), NextToken: first.NextToken})
	wantError(t, e, "InvalidInputException")
	_, e = s.ListProjects(ctx, &api.ListProjectsInput{SortBy: new("CREATED_TIME"), NextToken: first.NextToken})
	wantError(t, e, "InvalidInputException")
	_, e = s.ListProjects(ctx, &api.ListProjectsInput{NextToken: new("bad")})
	wantError(t, e, "InvalidInputException")
	for _, by := range []string{sortName, "CREATED_TIME", "LAST_MODIFIED_TIME"} {
		out, err := s.ListProjects(ctx, &api.ListProjectsInput{SortBy: new(by), SortOrder: new("DESCENDING")})
		if err != nil || len(out.Projects) != 100 {
			t.Fatal(out, err)
		}

		if by != sortName && out.Projects[0] != "p200" {
			t.Fatal("incorrect time sort", out)
		}
	}

	mixed, e := s.BatchGetProjects(
		ctx,
		&api.BatchGetProjectsInput{
			Names: []string{
				"project",
				"missing",
				"project",
				"arn:aws:codebuild:us-west-2:000000000000:project/project",
			},
		},
	)
	if e != nil || len(mixed.Projects) != 1 || len(mixed.ProjectsNotFound) != 2 {
		t.Fatalf("mixed duplicates/foreign %v %v", mixed, e)
	}

	for _, names := range [][]string{
		nil, make([]string, 101), {"arn:aws-cn:codebuild:us-east-1:000000000000:project/project"}, {""},
	} {
		_, e = s.BatchGetProjects(ctx, &api.BatchGetProjectsInput{Names: names})
		wantError(t, e, "InvalidInputException")
	}

	wg = sync.WaitGroup{}
	for i := range 10 {
		wg.Go(func() {
			_, err := s.UpdateProject(
				ctx,
				&api.UpdateProjectInput{Name: new("project"), Description: new(fmt.Sprintf("update-%d", i))},
			)
			if err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	p := getProject(t, s, ctx, "project")
	if *p.LastModified <= *p.Created {
		t.Fatal("concurrent updates lost timestamp")
	}
}

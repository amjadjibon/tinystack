package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func codebuildClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func codebuildInput() *sdk.CreateProjectInput {
	return &sdk.CreateProjectInput{
		Name: new(
			"records-project",
		),
		Description: new("retained metadata"),
		ServiceRole: new("arn:aws:iam::000000000000:role/codebuild/worker"),
		Source: &types.ProjectSource{
			Type:          types.SourceTypeGithub,
			Location:      new("https://github.com/example/repository.git"),
			Buildspec:     new("config/buildspec.yml"),
			GitCloneDepth: new(int32(3)),
		},
		SourceVersion: new("main"),
		Artifacts: &types.ProjectArtifacts{
			Type:                 types.ArtifactsTypeS3,
			Location:             new("output-bucket"),
			Name:                 new("artifact.zip"),
			Path:                 new("results"),
			NamespaceType:        types.ArtifactNamespaceBuildId,
			Packaging:            types.ArtifactPackagingZip,
			OverrideArtifactName: new(false),
			EncryptionDisabled:   new(true),
		},
		Environment: &types.ProjectEnvironment{
			Type:           types.EnvironmentTypeLinuxContainer,
			ComputeType:    types.ComputeTypeBuildGeneral1Medium,
			Image:          new("aws/codebuild/standard:7.0"),
			PrivilegedMode: new(true),
			EnvironmentVariables: []types.EnvironmentVariable{
				{Name: new("MODE"), Value: new("local"), Type: types.EnvironmentVariableTypePlaintext},
				{Name: new("PARAM"), Value: new("/local/parameter"), Type: types.EnvironmentVariableTypeParameterStore},
			},
		},
		TimeoutInMinutes: new(
			int32(35),
		),
		QueuedTimeoutInMinutes: new(int32(25)),
		Tags:                   []types.Tag{{Key: new("owner"), Value: new("platform")}},
	}
}

func codebuildProject(t *testing.T, c *sdk.Client, name string) types.Project {
	t.Helper()
	out, e := c.BatchGetProjects(context.Background(), &sdk.BatchGetProjectsInput{Names: []string{name}})
	if e != nil || len(out.Projects) != 1 {
		t.Fatalf("read %v %v", out, e)
	}
	return out.Projects[0]
}

func TestCodeBuildSDKLifecycleIsolationAndTags(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codebuildClient(s, "test", "us-east-1")
	in := codebuildInput()
	created, e := c.CreateProject(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	p := created.Project
	if p == nil || p.Badge != nil || p.Webhook != nil || p.EncryptionKey != nil ||
		*p.Arn != "arn:aws:codebuild:us-east-1:000000000000:project/records-project" ||
		p.Created == nil ||
		!p.Created.Equal(*p.LastModified) {
		t.Fatal("unexpected identity/runtime metadata", p)
	}
	read := codebuildProject(t, c, *p.Arn)
	if !reflect.DeepEqual(*p, read) {
		t.Fatal("SDK did not roundtrip complete config")
	}
	_, e = c.CreateProject(ctx, in)
	var duplicate *types.ResourceAlreadyExistsException
	if !errors.As(e, &duplicate) {
		t.Fatal("SDK duplicate type", e)
	}
	code(t, e, "ResourceAlreadyExistsException")
	var response *smithyhttp.ResponseError
	if !errors.As(e, &response) || response.HTTPStatusCode() != 400 {
		t.Fatal("error status", e)
	}
	for _, other := range []*sdk.Client{
		codebuildClient(s, "team-b", "us-east-1"),
		codebuildClient(s, "test", "us-west-2"),
	} {
		out, err := other.BatchGetProjects(ctx, &sdk.BatchGetProjectsInput{Names: []string{*p.Name, *p.Arn}})
		if err != nil || len(out.Projects) != 0 || len(out.ProjectsNotFound) != 2 {
			t.Fatal("scope leakage", out, err)
		}
	}
	mixed, e := c.BatchGetProjects(ctx, &sdk.BatchGetProjectsInput{Names: []string{*p.Name, "missing", *p.Name}})
	if e != nil || len(mixed.Projects) != 1 || !reflect.DeepEqual(mixed.ProjectsNotFound, []string{"missing"}) {
		t.Fatalf("mixed %v %v", mixed, e)
	}
	list, e := c.ListProjects(
		ctx,
		&sdk.ListProjectsInput{SortBy: types.ProjectSortByTypeName, SortOrder: types.SortOrderTypeDescending},
	)
	if e != nil || !reflect.DeepEqual(list.Projects, []string{*p.Name}) {
		t.Fatal(list, e)
	}
	_, e = c.UpdateProject(
		ctx,
		&sdk.UpdateProjectInput{
			Name:                   p.Name,
			Description:            new("bad"),
			QueuedTimeoutInMinutes: new(int32(4)),
			Tags:                   []types.Tag{{Key: new("fail"), Value: new("bad")}},
		},
	)
	code(t, e, "InvalidInputException")
	if !reflect.DeepEqual(read, codebuildProject(t, c, *p.Name)) {
		t.Fatal("failed SDK update mutated record")
	}
	updated, e := c.UpdateProject(
		ctx,
		&sdk.UpdateProjectInput{
			Name:        p.Name,
			Description: new(""),
			Tags:        []types.Tag{{Key: new("replacement"), Value: new("")}},
			Environment: &types.ProjectEnvironment{
				Type:                 types.EnvironmentTypeLinuxContainer,
				Image:                new("example/image:tag"),
				ComputeType:          types.ComputeTypeBuildGeneral1Small,
				PrivilegedMode:       new(false),
				EnvironmentVariables: []types.EnvironmentVariable{{Name: new("EMPTY"), Value: new("")}},
			},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	if len(updated.Project.Tags) != 1 || *updated.Project.Tags[0].Key != "replacement" ||
		*updated.Project.Environment.PrivilegedMode ||
		!updated.Project.Created.Equal(*p.Created) ||
		!updated.Project.LastModified.After(*p.LastModified) {
		t.Fatal(updated)
	}
	rg := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	tagged, e := rg.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*p.Arn},
			Tags:            map[string]string{"external": "kept"},
		},
	)
	if e != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, e)
	}
	if len(codebuildProject(t, c, *p.Name).Tags) != 2 {
		t.Fatal("RG tag must merge")
	}
	untagged, e := rg.UntagResources(
		ctx,
		&resourcegroupstaggingapi.UntagResourcesInput{ResourceARNList: []string{*p.Arn}, TagKeys: []string{"external"}},
	)
	if e != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal(untagged, e)
	}
	cleared, e := c.UpdateProject(ctx, &sdk.UpdateProjectInput{Name: p.Name, Tags: []types.Tag{}})
	if e != nil || len(cleared.Project.Tags) != 0 {
		t.Fatalf("SDK empty tags clear %v %v", cleared, e)
	}
	_, e = c.UpdateProject(ctx, &sdk.UpdateProjectInput{Name: p.Arn})
	code(t, e, "InvalidInputException")
	if _, e = c.DeleteProject(ctx, &sdk.DeleteProjectInput{Name: p.Name}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.DeleteProject(ctx, &sdk.DeleteProjectInput{Name: p.Name}); e != nil {
		t.Fatal(e)
	}
	_, e = c.UpdateProject(ctx, &sdk.UpdateProjectInput{Name: p.Name})
	var absent *types.ResourceNotFoundException
	if !errors.As(e, &absent) {
		t.Fatal("SDK missing type", e)
	}
}

func codebuildWire(
	t *testing.T,
	s *httptest.Server,
	method, path, target, body string,
) (status int, response map[string]any) {
	t.Helper()
	req, e := http.NewRequestWithContext(context.Background(), method, s.URL+path, bytes.NewBufferString(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261011/us-east-1/codebuild/aws4_request, SignedHeaders=host, Signature=dummy",
	)
	resp, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatalf("wire JSON %s %v", raw, e)
		}
	}
	return resp.StatusCode, out
}

func TestCodeBuildWireProtocolUnsupportedAndErrors(t *testing.T) {
	s := server(t)
	c := codebuildClient(s, "test", "us-east-1")
	ctx := context.Background()
	created, e := c.CreateProject(ctx, codebuildInput())
	if e != nil {
		t.Fatal(e)
	}
	before := *created.Project
	target := "CodeBuild_20161006."
	status, out := codebuildWire(
		t,
		s,
		"POST",
		"/",
		target+"BatchGetProjects",
		`{"names":["records-project","missing"]}`,
	)
	if status != 200 || len(out["projects"].([]any)) != 1 || out["projectsNotFound"].([]any)[0] != "missing" {
		t.Fatal(status, out)
	}
	unsupported := []string{
		`{"badgeEnabled":false}`,
		`{"cache":null}`,
		`{"logsConfig":{"s3Logs":{}}}`,
		`{"secondarySources":[]}`,
		`{"encryptionKey":""}`,
		`{"source":{"type":"GITHUB","auth":null}}`,
		`{"source":{"type":"GITHUB","insecureSsl":false}}`,
		`{"source":{"type":"GITHUB","gitSubmodulesConfig":{}}}`,
		`{"environment":{"registryCredential":null}}`,
		`{"environment":{"imagePullCredentialsType":""}}`,
		`{"artifacts":{"bucketOwnerAccess":""}}`,
	}
	for _, extra := range unsupported {
		body := `{"name":"records-project",` + strings.TrimPrefix(extra, "{")
		status, out = codebuildWire(t, s, "POST", "/", target+"UpdateProject", body)
		if status != 400 || !strings.Contains(out["__type"].(string), "ValidationException") {
			t.Fatalf("unsupported %s: %d %v", body, status, out)
		}
	}
	status, out = codebuildWire(
		t,
		s,
		"POST",
		"/",
		target+"UpdateProject",
		`{"name":"records-project","timeoutInMinutes":4}`,
	)
	if status != 400 || !strings.Contains(out["__type"].(string), "InvalidInputException") || out["message"] == nil {
		t.Fatal("modeled wire error", status, out)
	}
	status, out = codebuildWire(t, s, "POST", "/", target+"CreateProject", `{}`)
	if status != 400 || !strings.Contains(out["__type"].(string), "InvalidInputException") {
		t.Fatal(status, out)
	}
	status, out = codebuildWire(t, s, "POST", "/", target+"ListProjects", `{"maxResults":1}`)
	if status != 400 || !strings.Contains(out["__type"].(string), "ValidationException") {
		t.Fatal("invented pagination accepted", status, out)
	}
	// JSON 1.1 has no modeled URI labels or query members: extra query inputs fail routing.
	status, out = codebuildWire(t, s, "POST", "/?sortBy=INVALID&maxResults=1", target+"ListProjects", `{}`)
	if status != 400 || !strings.Contains(out["__type"].(string), "ValidationException") {
		t.Fatal("unmodeled query inputs accepted", status, out)
	}
	status, out = codebuildWire(t, s, "POST", "/?names=records-project", target+"BatchGetProjects", `{}`)
	if status != 400 || !strings.Contains(out["__type"].(string), "ValidationException") {
		t.Fatal("query supplied required body-only names", status, out)
	}
	for _, op := range []string{
		"CreateWebhook",
		"ImportSourceCredentials", "TagResource", "UntagResource",
	} {
		status, out = codebuildWire(t, s, "POST", "/", target+op, `{}`)
		if status != 400 || !strings.Contains(out["__type"].(string), "UnknownOperationException") {
			t.Fatal("unimplemented operation", op, status, out)
		}
	}
	for _, route := range []struct{ method, path string }{{"GET", "/"}, {"POST", "/v1/projects"}} {
		status, _ = codebuildWire(t, s, route.method, route.path, target+"ListProjects", `{}`)
		if status == 200 {
			t.Fatal("non-modeled route accepted", route)
		}
	}
	if !reflect.DeepEqual(before, codebuildProject(t, c, "records-project")) {
		t.Fatal("wire failures mutated metadata")
	}
}

func TestCodeBuildVerifySignatures(t *testing.T) {
	s := verifyingServer(t)
	ctx := context.Background()
	c := codebuildClient(s, "test", "us-east-1")
	p, e := c.CreateProject(ctx, codebuildInput())
	if e != nil {
		t.Fatal(e)
	}
	codebuildProject(t, c, *p.Project.Arn)
	if _, e = c.ListProjects(ctx, &sdk.ListProjectsInput{}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.UpdateProject(
		ctx,
		&sdk.UpdateProjectInput{Name: p.Project.Name, Description: new("signed")},
	); e != nil {
		t.Fatal(e)
	}
	_, e = codebuildClient(s, "wrong", "us-east-1").ListProjects(ctx, &sdk.ListProjectsInput{})
	code(t, e, "InvalidSignatureException")
	if _, e = c.DeleteProject(ctx, &sdk.DeleteProjectInput{Name: p.Project.Name}); e != nil {
		t.Fatal(e)
	}
}

func TestCodeBuildPersistenceFixtureImportAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := codebuildClient(s, "test", "us-east-1")
	created, e := c.CreateProject(ctx, codebuildInput())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.UpdateProject(
		ctx,
		&sdk.UpdateProjectInput{
			Name:             created.Project.Name,
			Description:      new("updated and persisted"),
			TimeoutInMinutes: new(int32(91)),
			Tags:             []types.Tag{{Key: new("snapshot"), Value: new("exact")}},
		},
	); e != nil {
		t.Fatal(e)
	}
	before := codebuildProject(t, c, *created.Project.Name)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	if !reflect.DeepEqual(before, codebuildProject(t, codebuildClient(s, "test", "us-east-1"), *before.Arn)) {
		t.Fatal("SQLite restart lost exact metadata")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "codebuild.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	if !reflect.DeepEqual(before, codebuildProject(t, codebuildClient(s, "test", "us-east-1"), *before.Arn)) {
		t.Fatal("import lost exact metadata")
	}
	stop()
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = codebuildClient(s, "test", "us-east-1")
	if !reflect.DeepEqual(before, codebuildProject(t, c, *before.Name)) {
		t.Fatal("normal imported restart lost metadata")
	}
	resp, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || !strings.Contains(string(raw), *before.Arn) || !strings.Contains(string(raw), "RecordsOnly") ||
		!strings.Contains(string(raw), "ExecutionReady") {
		t.Fatalf("inventory %s %v", raw, e)
	}
	status, body := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatalf("reset %d %s", status, body)
	}
	list, e := c.ListProjects(ctx, &sdk.ListProjectsInput{})
	if e != nil || len(list.Projects) != 0 {
		t.Fatal("reset retained projects", list, e)
	}
	resp, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || strings.Contains(string(raw), *before.Arn) {
		t.Fatal("reset retained inventory", e)
	}
}

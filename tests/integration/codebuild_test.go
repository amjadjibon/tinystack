package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	cbapi "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/gateway"
	cbservice "github.com/amjadjibon/tinystack/internal/services/codebuild"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
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

// reportFileRuntime exercises the production copy-back contract without running Docker.
type reportFileRuntime struct{}

func (*reportFileRuntime) Clean(context.Context, string) error { return nil }
func (*reportFileRuntime) Run(_ context.Context, spec *cbservice.RunSpec, _ func(string) error) error {
	if strings.Contains(spec.Script, "many-cases") {
		data := "<testsuite>" + strings.Repeat(
			`<testcase name="pass"/>`,
			500,
		) + `<testcase name="last-failure"><failure/></testcase></testsuite>`
		return os.WriteFile(filepath.Join(spec.Directory, "junit.xml"), []byte(data), 0o600)
	}

	if err := os.WriteFile(
		filepath.Join(spec.Directory, "cucumber.json"),
		[]byte(
			`[{"name":"feature","elements":[{"name":"scenario","type":"scenario",`+
				`"steps":[{"result":{"status":"passed","duration":10}}]}]}]`,
		),
		0o600,
	); err != nil {
		return err
	}

	return os.WriteFile(
		filepath.Join(spec.Directory, "junit.xml"),
		[]byte(
			`<testsuite><testcase name="passes" classname="suite" time="0.25"/>`+
				`<testcase name="fails"><failure message="assertion"/></testcase>`+
				`<testcase name="skips"><skipped/></testcase></testsuite>`,
		),
		0o600,
	)
}

func TestCodeBuildReportsAndRetrySDK(t *testing.T) {
	ctx := context.Background()
	service := cbservice.New(&memory.Store{})
	h, err := gateway.New(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), cbapi.Descriptor(service))
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	if err = service.UseRuntime(ctx, &reportFileRuntime{}, s.URL, nil); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(service.Stop)
	c := codebuildClient(s, "test", "us-east-1")
	group, err := c.CreateReportGroup(
		ctx,
		&sdk.CreateReportGroupInput{
			Name:         new("test-group"),
			Type:         types.ReportTypeTest,
			ExportConfig: &types.ReportExportConfig{ExportConfigType: types.ReportExportConfigTypeNoExport},
			Tags:         []types.Tag{{Key: new("team"), Value: new("one")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	arn := group.ReportGroup.Arn
	_, err = c.CreateReportGroup(
		ctx,
		&sdk.CreateReportGroupInput{
			Name:         new("test-group"),
			Type:         types.ReportTypeTest,
			ExportConfig: &types.ReportExportConfig{ExportConfigType: types.ReportExportConfigTypeNoExport},
		},
	)
	code(t, err, "ResourceAlreadyExistsException")
	updated, err := c.UpdateReportGroup(ctx, &sdk.UpdateReportGroupInput{Arn: arn, Tags: []types.Tag{}})
	if err != nil || len(updated.ReportGroup.Tags) != 0 {
		t.Fatal(updated, err)
	}

	groups, err := c.ListReportGroups(
		ctx,
		&sdk.ListReportGroupsInput{
			MaxResults: new(int32(1)),
			SortBy:     types.ReportGroupSortByTypeName,
			SortOrder:  types.SortOrderTypeDescending,
		},
	)
	if err != nil || len(groups.ReportGroups) != 1 {
		t.Fatal(groups, err)
	}

	batch, err := c.BatchGetReportGroups(
		ctx,
		&sdk.BatchGetReportGroupsInput{ReportGroupArns: []string{*arn, *arn, *arn + "missing"}},
	)
	if err != nil || len(batch.ReportGroups) != 1 || len(batch.ReportGroupsNotFound) != 1 {
		t.Fatal(batch, err)
	}

	other, err := codebuildClient(
		s,
		"team-b",
		"us-east-1",
	).BatchGetReportGroups(ctx, &sdk.BatchGetReportGroupsInput{ReportGroupArns: []string{*arn}})
	if err != nil || len(other.ReportGroupsNotFound) != 1 {
		t.Fatal(other, err)
	}

	spec := "version: 0.2\nphases:\n  build:\n    commands: [echo test]\nreports:\n  '" +
		*arn + "':\n    files: [junit.xml]\n"
	_, err = c.CreateProject(ctx, dockerBuildInput("report-project", spec))
	if err != nil {
		t.Fatal(err)
	}

	start := &sdk.StartBuildInput{
		ProjectName:      new("report-project"),
		ImageOverride:    new("alpine:3.20"),
		IdempotencyToken: new("start-token"),
	}

	build, err := c.StartBuild(ctx, start)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := c.StartBuild(ctx, start)
	if err != nil || *replay.Build.Id != *build.Build.Id {
		t.Fatal(replay, err)
	}

	start.ImageOverride = new("alpine:3.21")
	_, err = c.StartBuild(ctx, start)
	code(t, err, "InvalidInputException")
	_, err = c.RetryBuild(ctx, &sdk.RetryBuildInput{Id: build.Build.Id})
	code(t, err, "InvalidInputException")
	done := waitCodeBuild(t, c, *build.Build.Id, types.StatusTypeSucceeded)
	if len(done.ReportArns) != 1 || !strings.Contains(done.ReportArns[0], ":report/") {
		t.Fatal(done)
	}

	reports, err := c.ListReportsForReportGroup(
		ctx,
		&sdk.ListReportsForReportGroupInput{
			ReportGroupArn: arn,
			Filter:         &types.ReportFilter{Status: types.ReportStatusTypeFailed},
		},
	)
	if err != nil || len(reports.Reports) != 1 {
		t.Fatal(reports, err)
	}

	all, err := c.ListReports(ctx, &sdk.ListReportsInput{})
	if err != nil || len(all.Reports) != 1 {
		t.Fatal(all, err)
	}

	report, err := c.BatchGetReports(ctx, &sdk.BatchGetReportsInput{ReportArns: append(all.Reports, *arn+":missing")})
	if err != nil || len(report.Reports) != 1 || len(report.ReportsNotFound) != 1 ||
		*report.Reports[0].TestSummary.Total != 3 {
		t.Fatal(report, err)
	}

	cases, err := c.DescribeTestCases(
		ctx,
		&sdk.DescribeTestCasesInput{ReportArn: new(all.Reports[0]), MaxResults: new(int32(1))},
	)
	if err != nil || len(cases.TestCases) != 1 || cases.NextToken == nil {
		t.Fatal(cases, err)
	}

	page, err := c.DescribeTestCases(
		ctx,
		&sdk.DescribeTestCasesInput{ReportArn: new(all.Reports[0]), NextToken: cases.NextToken},
	)
	if err != nil || len(page.TestCases) != 2 {
		t.Fatal(page, err)
	}

	filtered, err := c.DescribeTestCases(
		ctx,
		&sdk.DescribeTestCasesInput{
			ReportArn: new(all.Reports[0]),
			Filter:    &types.TestCaseFilter{Status: new("FAILED"), Keyword: new("fails")},
		},
	)
	if err != nil || len(filtered.TestCases) != 1 || *filtered.TestCases[0].Message != "assertion" {
		t.Fatal(filtered, err)
	}

	_, err = c.UpdateProject(
		ctx,
		&sdk.UpdateProjectInput{
			Name: new("report-project"),
			Environment: &types.ProjectEnvironment{
				Type:        types.EnvironmentTypeLinuxContainer,
				ComputeType: types.ComputeTypeBuildGeneral1Small,
				Image:       new("changed:latest"),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	retryIn := &sdk.RetryBuildInput{Id: build.Build.Id, IdempotencyToken: new("retry-token")}
	retry, err := c.RetryBuild(ctx, retryIn)
	if err != nil || *retry.Build.Id == *build.Build.Id || *retry.Build.Environment.Image != "alpine:3.20" {
		t.Fatal(retry, err)
	}

	again, err := c.RetryBuild(ctx, retryIn)
	if err != nil || *again.Build.Id != *retry.Build.Id {
		t.Fatal(again, err)
	}

	waitCodeBuild(t, c, *retry.Build.Id, types.StatusTypeSucceeded)
	_, err = c.DeleteReportGroup(ctx, &sdk.DeleteReportGroupInput{Arn: arn})
	code(t, err, "InvalidInputException")
	_, err = c.DeleteReportGroup(ctx, &sdk.DeleteReportGroupInput{Arn: arn, DeleteReports: true})
	if err != nil {
		t.Fatal(err)
	}

	remaining, err := c.BatchGetReports(ctx, &sdk.BatchGetReportsInput{ReportArns: all.Reports})
	if err != nil || len(remaining.ReportsNotFound) != 1 {
		t.Fatal(remaining, err)
	}
}

func TestCodeBuildCucumberReportsSDK(t *testing.T) {
	ctx := context.Background()
	service := cbservice.New(&memory.Store{})
	h, err := gateway.New(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), cbapi.Descriptor(service))
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	if err = service.UseRuntime(ctx, &reportFileRuntime{}, server.URL, nil); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(service.Stop)
	c := codebuildClient(server, "test", "us-east-1")
	_, err = c.CreateProject(
		ctx,
		dockerBuildInput(
			"cucumber-project",
			"version: 0.2\nreports:\n  scenarios:\n    files: [cucumber.json]\n    file-format: CUCUMBERJSON\n",
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	build, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: new("cucumber-project")})
	if err != nil {
		t.Fatal(err)
	}

	done := waitCodeBuild(t, c, *build.Build.Id, types.StatusTypeSucceeded)
	if len(done.ReportArns) != 1 || !strings.Contains(done.ReportArns[0], ":report/") {
		t.Fatal(done)
	}

	report, err := c.BatchGetReports(ctx, &sdk.BatchGetReportsInput{ReportArns: done.ReportArns})
	if err != nil || *report.Reports[0].TestSummary.Total != 1 ||
		*report.Reports[0].TestSummary.DurationInNanoSeconds != 10 {
		t.Fatal(report, err)
	}

	cases, err := c.DescribeTestCases(ctx, &sdk.DescribeTestCasesInput{ReportArn: new(done.ReportArns[0])})
	if err != nil || len(cases.TestCases) != 1 || *cases.TestCases[0].Name != "scenario" {
		t.Fatal(cases, err)
	}
}

func TestCodeBuildReportGroupsRecordsValidationSDK(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := codebuildClient(s, "test", "us-east-1")
	for _, typ := range []types.ReportType{types.ReportTypeCodeCoverage, "INVALID"} {
		_, err := c.CreateReportGroup(
			ctx,
			&sdk.CreateReportGroupInput{
				Name:         new("unsupported"),
				Type:         typ,
				ExportConfig: &types.ReportExportConfig{ExportConfigType: types.ReportExportConfigTypeNoExport},
			},
		)
		code(t, err, "InvalidInputException")
	}

	_, err := c.CreateReportGroup(
		ctx,
		&sdk.CreateReportGroupInput{
			Name:         new("unsupported"),
			Type:         types.ReportTypeTest,
			ExportConfig: &types.ReportExportConfig{ExportConfigType: types.ReportExportConfigTypeS3},
		},
	)
	code(t, err, "InvalidInputException")
	for _, name := range []string{"aa-first", "zz-last"} {
		_, err = c.CreateReportGroup(
			ctx,
			&sdk.CreateReportGroupInput{
				Name:         new(name),
				Type:         types.ReportTypeTest,
				ExportConfig: &types.ReportExportConfig{ExportConfigType: types.ReportExportConfigTypeNoExport},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	groups, err := c.ListReportGroups(
		ctx,
		&sdk.ListReportGroupsInput{
			MaxResults: new(int32(1)),
			SortBy:     types.ReportGroupSortByTypeName,
			SortOrder:  types.SortOrderTypeDescending,
		},
	)
	if err != nil || len(groups.ReportGroups) != 1 || !strings.HasSuffix(groups.ReportGroups[0], "zz-last") ||
		groups.NextToken == nil {
		t.Fatal(groups, err)
	}

	page, err := c.ListReportGroups(
		ctx,
		&sdk.ListReportGroupsInput{NextToken: groups.NextToken, SortOrder: types.SortOrderTypeDescending},
	)
	if err != nil || len(page.ReportGroups) != 1 || !strings.HasSuffix(page.ReportGroups[0], "aa-first") {
		t.Fatal(page, err)
	}

	reports, err := c.ListReports(ctx, &sdk.ListReportsInput{})
	if err != nil || len(reports.Reports) != 0 {
		t.Fatal(reports, err)
	}

	_, err = c.DescribeTestCases(ctx, &sdk.DescribeTestCasesInput{ReportArn: new(groups.ReportGroups[0] + ":missing")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.RetryBuild(ctx, &sdk.RetryBuildInput{Id: new("missing:id")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.UpdateReportGroup(
		ctx,
		&sdk.UpdateReportGroupInput{
			Arn:  new(groups.ReportGroups[0]),
			Tags: []types.Tag{{Key: new("dup"), Value: new("1")}, {Key: new("dup"), Value: new("2")}},
		},
	)
	code(t, err, "InvalidInputException")
	_, err = c.DeleteReportGroup(ctx, &sdk.DeleteReportGroupInput{Arn: new(groups.ReportGroups[0])})
	if err != nil {
		t.Fatal(err)
	}

	other, err := codebuildClient(s, "test", "us-west-2").ListReportGroups(ctx, &sdk.ListReportGroupsInput{})
	if err != nil || len(other.ReportGroups) != 0 {
		t.Fatal(other, err)
	}
}

func TestCodeBuildReportTruncationSDK(t *testing.T) {
	ctx := context.Background()
	service := cbservice.New(&memory.Store{})
	h, err := gateway.New(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), cbapi.Descriptor(service))
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	if err = service.UseRuntime(ctx, &reportFileRuntime{}, server.URL, nil); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(service.Stop)
	c := codebuildClient(server, "test", "us-east-1")
	_, err = c.CreateProject(
		ctx,
		dockerBuildInput(
			"large-project",
			"version: 0.2\nphases:\n  build:\n    commands: [echo many-cases]\nreports:\n  tests:\n    files: ['**/*.xml']\n",
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	build, err := c.StartBuild(ctx, &sdk.StartBuildInput{ProjectName: new("large-project")})
	if err != nil {
		t.Fatal(err)
	}

	done := waitCodeBuild(t, c, *build.Build.Id, types.StatusTypeSucceeded)
	report, err := c.BatchGetReports(ctx, &sdk.BatchGetReportsInput{ReportArns: done.ReportArns})
	if err != nil || !*report.Reports[0].Truncated || *report.Reports[0].TestSummary.Total != 501 {
		t.Fatal(report, err)
	}

	cases, err := c.DescribeTestCases(
		ctx,
		&sdk.DescribeTestCasesInput{ReportArn: new(done.ReportArns[0]), MaxResults: new(int32(1))},
	)
	if err != nil || *cases.TestCases[0].Name != "last-failure" {
		t.Fatal(cases, err)
	}
}

func TestCodeBuildRetryRecordsSDK(t *testing.T) {
	ctx := context.Background()
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	c := codebuildClient(s, "test", "us-east-1")
	_, err := c.CreateProject(
		ctx,
		dockerBuildInput("records-retry", "version: 0.2\nphases:\n  build:\n    commands: [echo test]\n"),
	)
	if err != nil {
		t.Fatal(err)
	}

	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	// Persisted completed builds may be loaded from fixtures or a previous Docker-mode run.
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		p, _, e := storage.Read[cbapi.Project](tx, scope.Key("codebuild", "project", "records-retry"))
		if e != nil {
			return e
		}

		p.Environment.Image = new("saved:latest")
		return storage.Write(tx, scope.Key("codebuild", "build", "records-retry:previous"), struct {
			Project cbapi.Project
			Scope   request.Scope
			Build   cbapi.Build
		}{Project: *p, Scope: scope, Build: cbapi.Build{
			Id: new(
				"records-retry:previous",
			),
			ProjectName:   p.Name,
			BuildComplete: new(true),
			BuildStatus:   new("FAILED"),
		}})
	})
	if err != nil {
		t.Fatal(err)
	}

	in := &sdk.RetryBuildInput{Id: new("records-retry:previous"), IdempotencyToken: new("records-token")}
	build, err := c.RetryBuild(ctx, in)
	if err != nil || build.Build.BuildComplete || build.Build.BuildStatus != types.StatusTypeInProgress ||
		*build.Build.Environment.Image != "saved:latest" {
		t.Fatal(build, err)
	}

	replay, err := c.RetryBuild(ctx, in)
	if err != nil || *replay.Build.Id != *build.Build.Id {
		t.Fatal(replay, err)
	}

	builds, listErr := c.ListBuildsForProject(ctx, &sdk.ListBuildsForProjectInput{ProjectName: new("records-retry")})
	if listErr != nil || len(builds.Ids) != 2 {
		t.Fatalf("duplicate build: %+v %v", builds, listErr)
	}

	_, err = c.StopBuild(ctx, &sdk.StopBuildInput{Id: build.Build.Id})
	if err != nil {
		t.Fatal(err)
	}

	in.Id = build.Build.Id
	_, err = c.RetryBuild(ctx, in)
	code(t, err, "InvalidInputException")
}

package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/amplify"
	"github.com/aws/aws-sdk-go-v2/service/amplify/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func amplifyClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func amplifyError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestAmplifyLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := amplifyClient(s, "test", "us-east-1")
	created, err := c.CreateApp(ctx, &sdk.CreateAppInput{
		Name: new("site"), BuildSpec: new("version: 1"),
		EnvironmentVariables: map[string]string{"TEAM": "core"}, EnableBasicAuth: new(true),
		BasicAuthCredentials: new("dXNlcjpwYXNz"), AccessToken: new("private-token"),
		CustomRules:              []types.CustomRule{{Source: new("/old"), Target: new("/new"), Status: new("301")}},
		AutoBranchCreationConfig: &types.AutoBranchCreationConfig{BasicAuthCredentials: new("nested-secret")},
		Tags:                     map[string]string{"team": "core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.App.AppId
	if created.App.BasicAuthCredentials != nil || created.App.AutoBranchCreationConfig.BasicAuthCredentials != nil ||
		aws.ToString(created.App.DefaultDomain) != *id+".amplifyapp.com" {
		t.Fatal(created.App)
	}

	got, err := c.GetApp(ctx, &sdk.GetAppInput{AppId: id})
	if err != nil || aws.ToString(got.App.BuildSpec) != "version: 1" || len(got.App.CustomRules) != 1 {
		t.Fatal(got, err)
	}

	_, err = c.UpdateApp(
		ctx,
		&sdk.UpdateAppInput{AppId: id, Name: new("renamed"), EnvironmentVariables: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateApp(ctx, &sdk.CreateAppInput{Name: new("second")})
	if err != nil {
		t.Fatal(err)
	}

	apps, err := c.ListApps(ctx, &sdk.ListAppsInput{MaxResults: 1})
	if err != nil || len(apps.Apps) != 1 || apps.NextToken == nil {
		t.Fatal(apps, err)
	}

	apps2, err := c.ListApps(ctx, &sdk.ListAppsInput{MaxResults: 1, NextToken: apps.NextToken})
	if err != nil || len(apps2.Apps) != 1 || *apps2.Apps[0].AppId == *apps.Apps[0].AppId {
		t.Fatal(apps2, err)
	}

	branch, err := c.CreateBranch(ctx, &sdk.CreateBranchInput{
		AppId: id, BranchName: new("feature/main"),
		Stage: types.StageDevelopment, BuildSpec: new("version: 1"), BasicAuthCredentials: new("branch-secret"),
		EnableBasicAuth: new(true), Tags: map[string]string{"team": "core"},
	})
	if err != nil || branch.Branch.BasicAuthCredentials != nil {
		t.Fatal(branch, err)
	}

	b := branch.Branch.BranchName
	_, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{AppId: id, BranchName: b})
	amplifyError(t, err, "BadRequestException")
	_, err = c.UpdateBranch(
		ctx,
		&sdk.UpdateBranchInput{AppId: id, BranchName: b, Description: new("updated"), Ttl: new("42")},
	)
	if err != nil {
		t.Fatal(err)
	}

	bg, err := c.GetBranch(ctx, &sdk.GetBranchInput{AppId: id, BranchName: b})
	if err != nil || aws.ToString(bg.Branch.Ttl) != "42" {
		t.Fatal(bg, err)
	}

	branches, err := c.ListBranches(ctx, &sdk.ListBranchesInput{AppId: id})
	if err != nil || len(branches.Branches) != 1 {
		t.Fatal(branches, err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: branch.Branch.BranchArn, Tags: map[string]string{"env": "dev"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: branch.Branch.BranchArn})
	if err != nil || tags.Tags["env"] != "dev" {
		t.Fatal(tags, err)
	}

	_, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: branch.Branch.BranchArn, TagKeys: []string{"env"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	rg := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := rg.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*created.App.AppArn}, Tags: map[string]string{"env": "dev"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) > 0 {
		t.Fatal(tagged, err)
	}

	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: created.App.AppArn})
	if err != nil || tags.Tags["env"] != "dev" {
		t.Fatal(tags, err)
	}

	for _, region := range []string{"us-west-2"} {
		_, err = amplifyClient(s, "test", region).GetApp(ctx, &sdk.GetAppInput{AppId: id})
		amplifyError(t, err, "NotFoundException")
	}

	_, err = amplifyClient(s, "111111111111", "us-east-1").GetApp(ctx, &sdk.GetAppInput{AppId: id})
	amplifyError(t, err, "NotFoundException")
	_, err = c.DeleteBranch(ctx, &sdk.DeleteBranchInput{AppId: id, BranchName: b})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApp(ctx, &sdk.DeleteAppInput{AppId: id})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetApp(ctx, &sdk.GetAppInput{AppId: id})
	amplifyError(t, err, "NotFoundException")
}

func TestAmplifyJobsDomainsAndMetadata(t *testing.T) {
	s := server(t)
	c := amplifyClient(s, "test", "us-east-1")
	ctx := context.Background()
	a, err := c.CreateApp(ctx, &sdk.CreateAppInput{Name: new("site")})
	if err != nil {
		t.Fatal(err)
	}

	id := a.App.AppId
	_, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{AppId: id, BranchName: new("main")})
	if err != nil {
		t.Fatal(err)
	}

	b := new("main")
	job, err := c.StartJob(
		ctx,
		&sdk.StartJobInput{AppId: id, BranchName: b, JobType: types.JobTypeRelease, CommitId: new("abc")},
	)
	if err != nil || job.JobSummary.Status != types.JobStatusPending {
		t.Fatal(job, err)
	}

	retry, err := c.StartJob(
		ctx,
		&sdk.StartJobInput{AppId: id, BranchName: b, JobType: types.JobTypeRetry, JobId: job.JobSummary.JobId},
	)
	if err != nil || *retry.JobSummary.JobId == *job.JobSummary.JobId ||
		aws.ToString(retry.JobSummary.CommitId) != "abc" {
		t.Fatal(retry, err)
	}

	jobs, err := c.ListJobs(ctx, &sdk.ListJobsInput{AppId: id, BranchName: b, MaxResults: 1})
	if err != nil || len(jobs.JobSummaries) != 1 || jobs.NextToken == nil {
		t.Fatal(jobs, err)
	}

	gj, err := c.GetJob(ctx, &sdk.GetJobInput{AppId: id, BranchName: b, JobId: job.JobSummary.JobId})
	if err != nil || len(gj.Job.Steps) != 0 || gj.Job.Summary.Status != types.JobStatusPending {
		t.Fatal(gj, err)
	}

	stopped, err := c.StopJob(ctx, &sdk.StopJobInput{AppId: id, BranchName: b, JobId: job.JobSummary.JobId})
	if err != nil || stopped.JobSummary.Status != types.JobStatusCancelled {
		t.Fatal(stopped, err)
	}

	_, err = c.DeleteJob(ctx, &sdk.DeleteJobInput{AppId: id, BranchName: b, JobId: job.JobSummary.JobId})
	if err != nil {
		t.Fatal(err)
	}

	deploy, err := c.CreateDeployment(ctx, &sdk.CreateDeploymentInput{AppId: id, BranchName: b})
	if err != nil || !strings.HasPrefix(aws.ToString(deploy.ZipUploadUrl), "https://amplify.invalid/") {
		t.Fatal(deploy, err)
	}

	started, err := c.StartDeployment(ctx, &sdk.StartDeploymentInput{AppId: id, BranchName: b, JobId: deploy.JobId})
	if err != nil || started.JobSummary.Status != types.JobStatusPending {
		t.Fatal(started, err)
	}

	_, err = c.StartDeployment(ctx, &sdk.StartDeploymentInput{AppId: id, BranchName: b, JobId: deploy.JobId})
	amplifyError(t, err, "BadRequestException")
	_, err = c.StartDeployment(
		ctx,
		&sdk.StartDeploymentInput{AppId: id, BranchName: b, SourceUrl: new("http://127.0.0.1:1/never-fetch.zip")},
	)
	if err != nil {
		t.Fatal(err)
	}

	d, err := c.CreateDomainAssociation(ctx, &sdk.CreateDomainAssociationInput{
		AppId: id, DomainName: new("example.com"),
		SubDomainSettings: []types.SubDomainSetting{{Prefix: new("www"), BranchName: b}},
	})
	if err != nil || d.DomainAssociation.DomainStatus != types.DomainStatusPendingVerification ||
		aws.ToBool(d.DomainAssociation.SubDomains[0].Verified) {
		t.Fatal(d, err)
	}

	_, err = c.UpdateDomainAssociation(
		ctx,
		&sdk.UpdateDomainAssociationInput{AppId: id, DomainName: new("example.com"), EnableAutoSubDomain: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	dg, err := c.GetDomainAssociation(ctx, &sdk.GetDomainAssociationInput{AppId: id, DomainName: new("example.com")})
	if err != nil || !aws.ToBool(dg.DomainAssociation.EnableAutoSubDomain) {
		t.Fatal(dg, err)
	}

	dl, err := c.ListDomainAssociations(ctx, &sdk.ListDomainAssociationsInput{AppId: id})
	if err != nil || len(dl.DomainAssociations) != 1 {
		t.Fatal(dl, err)
	}

	_, err = c.DeleteDomainAssociation(
		ctx,
		&sdk.DeleteDomainAssociationInput{AppId: id, DomainName: new("example.com")},
	)
	if err != nil {
		t.Fatal(err)
	}

	wh, err := c.CreateWebhook(ctx, &sdk.CreateWebhookInput{AppId: id, BranchName: b, Description: new("hook")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateWebhook(ctx, &sdk.UpdateWebhookInput{WebhookId: wh.Webhook.WebhookId, Description: new("changed")})
	if err != nil {
		t.Fatal(err)
	}

	wg, err := c.GetWebhook(ctx, &sdk.GetWebhookInput{WebhookId: wh.Webhook.WebhookId})
	if err != nil || aws.ToString(wg.Webhook.Description) != "changed" {
		t.Fatal(wg, err)
	}

	wl, err := c.ListWebhooks(ctx, &sdk.ListWebhooksInput{AppId: id})
	if err != nil || len(wl.Webhooks) != 1 {
		t.Fatal(wl, err)
	}

	_, err = c.DeleteWebhook(ctx, &sdk.DeleteWebhookInput{WebhookId: wh.Webhook.WebhookId})
	if err != nil {
		t.Fatal(err)
	}

	be, err := c.CreateBackendEnvironment(
		ctx,
		&sdk.CreateBackendEnvironmentInput{AppId: id, EnvironmentName: new("dev"), StackName: new("metadata-only")},
	)
	if err != nil {
		t.Fatal(err)
	}

	eg, err := c.GetBackendEnvironment(
		ctx,
		&sdk.GetBackendEnvironmentInput{AppId: id, EnvironmentName: be.BackendEnvironment.EnvironmentName},
	)
	if err != nil || aws.ToString(eg.BackendEnvironment.StackName) != "metadata-only" {
		t.Fatal(eg, err)
	}

	el, err := c.ListBackendEnvironments(ctx, &sdk.ListBackendEnvironmentsInput{AppId: id, EnvironmentName: new("dev")})
	if err != nil || len(el.BackendEnvironments) != 1 {
		t.Fatal(el, err)
	}

	_, err = c.DeleteBackendEnvironment(ctx, &sdk.DeleteBackendEnvironmentInput{AppId: id, EnvironmentName: new("dev")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApp(ctx, &sdk.DeleteAppInput{AppId: id})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetJob(ctx, &sdk.GetJobInput{AppId: id, BranchName: b, JobId: retry.JobSummary.JobId})
	amplifyError(t, err, "NotFoundException")
}

func TestAmplifyValidation(t *testing.T) {
	c := amplifyClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	for _, in := range []*sdk.CreateAppInput{
		{Name: new(strings.Repeat("a", 256))},
		{Name: new("site"), Platform: types.Platform("INVALID")},
		{Name: new("site"), Tags: map[string]string{"aws:reserved": "bad"}},
		{Name: new("site"), EnvironmentVariables: map[string]string{"A": strings.Repeat("x", 5501)}},
		{Name: new("site"), CustomRules: []types.CustomRule{{Source: new(""), Target: new("/")}}},
		{Name: new("site"), AutoBranchCreationConfig: &types.AutoBranchCreationConfig{Stage: types.Stage("INVALID")}},
	} {
		_, err := c.CreateApp(ctx, in)
		amplifyError(t, err, "BadRequestException")
	}

	_, err := c.ListApps(ctx, &sdk.ListAppsInput{MaxResults: 101})
	amplifyError(t, err, "BadRequestException")
	_, err = c.ListApps(ctx, &sdk.ListAppsInput{NextToken: new("!")})
	amplifyError(t, err, "BadRequestException")
}

func TestAmplifyPersistentRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := amplifyClient(s, "test", "us-east-1")
	a, err := c.CreateApp(ctx, &sdk.CreateAppInput{Name: new("persist"), Tags: map[string]string{"team": "core"}})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	_, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{AppId: a.App.AppId, BranchName: new("main")})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	job, err := c.StartJob(
		ctx,
		&sdk.StartJobInput{AppId: a.App.AppId, BranchName: new("main"), JobType: types.JobTypeRelease},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = amplifyClient(s, "test", "us-east-1")
	got, err := c.GetApp(ctx, &sdk.GetAppInput{AppId: a.App.AppId})
	if err != nil || got.App.Tags["team"] != "core" {
		t.Fatal(got, err)
	}

	j, err := c.GetJob(ctx, &sdk.GetJobInput{AppId: a.App.AppId, BranchName: new("main"), JobId: job.JobSummary.JobId})
	if err != nil || j.Job.Summary.Status != types.JobStatusPending {
		t.Fatal(j, err)
	}
}

func TestAmplifyRejectsUnsupportedAndInvalidBranchInputs(t *testing.T) {
	c := amplifyClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	app, err := c.CreateApp(ctx, &sdk.CreateAppInput{Name: new("validation")})
	if err != nil {
		t.Fatal(err)
	}

	id := app.App.AppId
	for _, in := range []*sdk.CreateBranchInput{
		{AppId: id, BranchName: new("main"), Stage: types.Stage("INVALID")},
		{AppId: id, BranchName: new("main"), Ttl: new("-1")},
		{AppId: id, BranchName: new("main"), Backend: &types.Backend{StackArn: new("invalid")}},
	} {
		_, err = c.CreateBranch(ctx, in)
		amplifyError(t, err, "BadRequestException")
	}

	_, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{AppId: id, BranchName: new("main")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListBranches(ctx, &sdk.ListBranchesInput{AppId: id, MaxResults: 51})
	amplifyError(t, err, "BadRequestException")
	_, err = c.CreateDomainAssociation(
		ctx,
		&sdk.CreateDomainAssociationInput{
			AppId:             id,
			DomainName:        new("-bad.example"),
			SubDomainSettings: []types.SubDomainSetting{},
		},
	)
	amplifyError(t, err, "BadRequestException")
	_, err = c.CreateDomainAssociation(ctx, &sdk.CreateDomainAssociationInput{
		AppId:             id,
		DomainName:        new("example.com"),
		SubDomainSettings: []types.SubDomainSetting{},
		CertificateSettings: &types.CertificateSettings{
			Type:                 types.CertificateTypeCustom,
			CustomCertificateArn: new("invalid"),
		},
	})
	amplifyError(t, err, "BadRequestException")
	_, err = c.StartJob(ctx, &sdk.StartJobInput{AppId: id, BranchName: new("main"), JobType: types.JobTypeManual})
	amplifyError(t, err, "BadRequestException")

	deployment, err := c.CreateDeployment(ctx, &sdk.CreateDeploymentInput{
		AppId: id, BranchName: new("main"), FileMap: map[string]string{"index.html": "hash"},
	})
	if err != nil || !strings.HasPrefix(deployment.FileUploadUrls["index.html"], "https://amplify.invalid/") {
		t.Fatal(deployment, err)
	}
}

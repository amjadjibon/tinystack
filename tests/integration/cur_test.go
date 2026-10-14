package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/costandusagereportservice"
	"github.com/aws/aws-sdk-go-v2/service/costandusagereportservice/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/amjadjibon/tinystack/internal/config"
)

func curClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func curDefinition(name string) *types.ReportDefinition {
	return &types.ReportDefinition{
		ReportName:               new(name),
		TimeUnit:                 types.TimeUnitDaily,
		Format:                   types.ReportFormatCsv,
		Compression:              types.CompressionFormatGzip,
		AdditionalSchemaElements: []types.SchemaElement{types.SchemaElementResources},
		S3Bucket:                 new("cur-destination"),
		S3Prefix:                 new(""),
		S3Region:                 types.AWSRegionUsStandard,
		AdditionalArtifacts:      []types.AdditionalArtifact{types.AdditionalArtifactRedshift},
		RefreshClosedReports:     new(false),
		ReportVersioning:         types.ReportVersioningCreateNewReport,
	}
}

func wantCURError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestCURSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := curClient(s, "test", "us-east-1")
	d := curDefinition("local-report")
	tags := []types.Tag{{Key: new("team"), Value: new("core")}}
	_, err := c.PutReportDefinition(ctx, &sdk.PutReportDefinitionInput{ReportDefinition: d, Tags: tags})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutReportDefinition(ctx, &sdk.PutReportDefinitionInput{ReportDefinition: d})
	wantCURError(t, err, "DuplicateReportNameException")
	otherRegion := curClient(s, "alias", "eu-west-1")
	listed, err := otherRegion.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{})
	if err != nil || len(listed.ReportDefinitions) != 1 || !reflect.DeepEqual(listed.ReportDefinitions[0], *d) {
		t.Fatal(listed, err)
	}

	isolated, err := curClient(
		s,
		"team-b",
		"us-east-1",
	).DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{})
	if err != nil || len(isolated.ReportDefinitions) != 0 {
		t.Fatal(isolated, err)
	}

	d.S3Prefix = new("updated/prefix")
	d.RefreshClosedReports = new(true)
	d.ReportVersioning = types.ReportVersioningOverwriteReport
	d.TimeUnit = types.TimeUnitMonthly
	_, err = otherRegion.ModifyReportDefinition(
		ctx,
		&sdk.ModifyReportDefinitionInput{ReportName: d.ReportName, ReportDefinition: d},
	)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{})
	if err != nil || !reflect.DeepEqual(updated.ReportDefinitions[0], *d) {
		t.Fatal(updated, err)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ReportName: d.ReportName, Tags: []types.Tag{
		{Key: new("team"), Value: new("platform")}, {Key: new("empty"), Value: new("")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	gotTags, err := otherRegion.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ReportName: d.ReportName})
	if err != nil || len(gotTags.Tags) != 2 || aws.ToString(gotTags.Tags[1].Value) != "platform" {
		t.Fatal(gotTags, err)
	}

	_, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ReportName: d.ReportName, TagKeys: []string{"empty", "absent"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	arn := "arn:aws:cur:us-east-1:000000000000:definition/local-report"
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{arn}, Tags: map[string]string{"inventory": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	untagged, err := tagging.UntagResources(ctx, &rgta.UntagResourcesInput{
		ResourceARNList: []string{arn}, TagKeys: []string{"inventory"},
	})
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal(untagged, err)
	}

	_, err = curClient(
		s,
		"team-b",
		"us-east-1",
	).TagResource(ctx, &sdk.TagResourceInput{ReportName: d.ReportName, Tags: tags})
	wantCURError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteReportDefinition(ctx, &sdk.DeleteReportDefinitionInput{ReportName: d.ReportName})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ReportName: d.ReportName})
	wantCURError(t, err, "ResourceNotFoundException")
	_, err = c.ModifyReportDefinition(
		ctx,
		&sdk.ModifyReportDefinitionInput{ReportName: d.ReportName, ReportDefinition: d},
	)
	wantCURError(t, err, "ValidationException")
}

func TestCURValidationAndPagination(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := curClient(s, "test", "us-east-1")
	for _, tc := range []struct {
		mutate func(*types.ReportDefinition)
		name   string
	}{
		{name: "name", mutate: func(d *types.ReportDefinition) { d.ReportName = new("bad/name") }},
		{name: "bucket", mutate: func(d *types.ReportDefinition) { d.S3Bucket = new("bad bucket") }},
		{name: "prefix", mutate: func(d *types.ReportDefinition) { d.S3Prefix = new("bad prefix") }},
		{name: "time", mutate: func(d *types.ReportDefinition) { d.TimeUnit = "WEEKLY" }},
		{name: "format", mutate: func(d *types.ReportDefinition) { d.Format = "CSV" }},
		{name: "compression", mutate: func(d *types.ReportDefinition) { d.Compression = "NONE" }},
		{name: "region", mutate: func(d *types.ReportDefinition) { d.S3Region = "invalid" }},
		{name: "schema", mutate: func(d *types.ReportDefinition) {
			d.AdditionalSchemaElements = []types.SchemaElement{"invalid"}
		}},
		{name: "artifact", mutate: func(d *types.ReportDefinition) {
			d.AdditionalArtifacts = []types.AdditionalArtifact{"invalid"}
		}},
		{name: "versioning", mutate: func(d *types.ReportDefinition) { d.ReportVersioning = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := curDefinition("invalid")
			tc.mutate(d)
			_, err := c.PutReportDefinition(ctx, &sdk.PutReportDefinitionInput{ReportDefinition: d})
			wantCURError(t, err, "ValidationException")
		})
	}

	for _, tc := range []struct {
		mutate func(*types.ReportDefinition)
		name   string
	}{
		{name: "billing-view", mutate: func(d *types.ReportDefinition) { d.BillingViewArn = new("view") }},
		{name: "delivery-status", mutate: func(d *types.ReportDefinition) {
			d.ReportStatus = &types.ReportStatus{LastStatus: types.LastStatusSuccess}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := curDefinition("unsupported")
			tc.mutate(d)
			_, err := c.PutReportDefinition(ctx, &sdk.PutReportDefinitionInput{ReportDefinition: d})
			wantCURError(t, err, "ValidationException")
		})
	}

	for i := range 7 {
		_, err := c.PutReportDefinition(
			ctx,
			&sdk.PutReportDefinitionInput{ReportDefinition: curDefinition(fmt.Sprintf("report-%d", i))},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{MaxResults: new(int32(5))})
	if err != nil || len(first.ReportDefinitions) != 5 || first.NextToken == nil {
		t.Fatal(first, err)
	}

	second, err := c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{NextToken: first.NextToken})
	if err != nil || len(second.ReportDefinitions) != 2 || second.NextToken != nil {
		t.Fatal(second, err)
	}

	_, err = c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{MaxResults: new(int32(4))})
	wantCURError(t, err, "ValidationException")
	_, err = c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{NextToken: new("!")})
	wantCURError(t, err, "ValidationException")
	_, err = c.DescribeReportDefinitions(
		ctx,
		&sdk.DescribeReportDefinitionsInput{NextToken: new(strings.Repeat("a", 300))},
	)
	wantCURError(t, err, "ValidationException")
	d := curDefinition("renamed")
	_, err = c.ModifyReportDefinition(
		ctx,
		&sdk.ModifyReportDefinitionInput{ReportName: new("report-0"), ReportDefinition: d},
	)
	wantCURError(t, err, "ValidationException")
	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ReportName: new("report-0"), TagKeys: make([]string, 201)})
	wantCURError(t, err, "ValidationException")
	tags := make([]types.Tag, 51)
	for i := range tags {
		tags[i] = types.Tag{Key: new(fmt.Sprintf("tag-%d", i)), Value: new("value")}
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ReportName: new("report-0"), Tags: tags})
	wantCURError(t, err, "ValidationException")
	got, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ReportName: new("report-0")})
	if err != nil || len(got.Tags) != 0 {
		t.Fatal(got, err)
	}
}

func TestCURPersistentRecordsOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := curClient(s, "test", "us-east-1")
	objects := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("cur-destination")})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	_, err = c.PutReportDefinition(ctx, &sdk.PutReportDefinitionInput{
		ReportDefinition: curDefinition("persistent"), Tags: []types.Tag{{Key: new("saved"), Value: new("yes")}},
	})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	list, err := objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("cur-destination")})
	if err != nil || len(list.Contents) != 0 {
		stop()
		t.Fatal(list, err)
	}

	response, err := s.Client().Get(s.URL + "/_tinystack/resources")
	if err != nil {
		stop()
		t.Fatal(err)
	}

	var inventory struct {
		Resources []struct {
			Details map[string]any
			Service string
		}
	}

	err = json.NewDecoder(response.Body).Decode(&inventory)
	response.Body.Close()
	if err != nil {
		stop()
		t.Fatal(err)
	}

	found := false
	for _, r := range inventory.Resources {
		if r.Service == "costandusagereportservice" {
			found = true
			if r.Details["RecordsOnly"] != true || r.Details["ExecutionReady"] != false {
				stop()
				t.Fatal(r)
			}
		}
	}

	if !found {
		stop()
		t.Fatal("CUR inventory missing")
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = curClient(s, "test", "ap-southeast-1")
	reports, err := c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{})
	if err != nil || len(reports.ReportDefinitions) != 1 {
		t.Fatal(reports, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ReportName: new("persistent")})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatal(tags, err)
	}

	response, err = s.Client().Post(s.URL+"/_tinystack/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	reports, err = c.DescribeReportDefinitions(ctx, &sdk.DescribeReportDefinitionsInput{})
	if err != nil || len(reports.ReportDefinitions) != 0 {
		t.Fatal(reports, err)
	}
}

// curDefaultEndpoint forwards a signed default-endpoint request to the local server,
// retaining the Host used in the signature. No request leaves the test server.
type curDefaultEndpoint struct {
	endpoint *url.URL
	host     string
	target   string
}

func (c *curDefaultEndpoint) Do(r *http.Request) (*http.Response, error) {
	c.host = r.URL.Host
	c.target = r.Header.Get("X-Amz-Target")
	forwarded := r.Clone(r.Context())
	forwarded.URL.Scheme = c.endpoint.Scheme
	forwarded.URL.Host = c.endpoint.Host
	forwarded.Host = c.host
	return http.DefaultClient.Do(forwarded)
}

func TestCURDefaultEndpointSigning(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	endpoint, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := &curDefaultEndpoint{endpoint: endpoint}
	c := sdk.New(
		sdk.Options{Region: "us-east-1", Credentials: creds("test"), HTTPClient: transport, Retryer: aws.NopRetryer{}},
	)
	_, err = c.PutReportDefinition(
		context.Background(),
		&sdk.PutReportDefinitionInput{ReportDefinition: curDefinition("default-endpoint")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if transport.host != "cur.us-east-1.amazonaws.com" ||
		transport.target != "AWSOrigamiServiceGatewayService.PutReportDefinition" {
		t.Fatal(transport.host, transport.target)
	}

	other := curClient(s, "test", "eu-west-1")
	reports, err := other.DescribeReportDefinitions(context.Background(), &sdk.DescribeReportDefinitionsInput{})
	if err != nil || len(reports.ReportDefinitions) != 1 {
		t.Fatal(reports, err)
	}
}

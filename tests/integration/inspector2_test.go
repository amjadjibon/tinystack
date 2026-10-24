package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/inspector2"
	"github.com/aws/aws-sdk-go-v2/service/inspector2/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func inspector2Client(s *httptest.Server) *sdk.Client {
	return sdk.New(sdk.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"),
		Retryer: aws.NopRetryer{},
	})
}

func inspector2Error(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func inspector2Criteria() *types.FilterCriteria {
	return &types.FilterCriteria{Severity: []types.StringFilter{{Comparison: "EQUALS", Value: new("LOW")}}}
}

func TestInspector2Lifecycle(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := inspector2Client(s)
	before, err := c.BatchGetAccountStatus(ctx, &sdk.BatchGetAccountStatusInput{})
	if err != nil || len(before.Accounts) != 1 || before.Accounts[0].State.Status != "DISABLED" {
		t.Fatalf("initial: %+v %v", before, err)
	}

	kinds := []types.ResourceScanType{"EC2", "ECR", "LAMBDA", "LAMBDA_CODE", "CODE_REPOSITORY"}
	enabled, err := c.Enable(ctx, &sdk.EnableInput{ResourceTypes: kinds})
	if err != nil || len(enabled.Accounts) != 1 || enabled.Accounts[0].Status != "ENABLED" {
		t.Fatalf("enable: %+v %v", enabled, err)
	}

	account := enabled.Accounts[0].AccountId
	status, err := c.BatchGetAccountStatus(ctx, &sdk.BatchGetAccountStatusInput{AccountIds: []string{*account}})
	if err != nil || status.Accounts[0].ResourceState.CodeRepository.Status != "ENABLED" {
		t.Fatalf("status: %+v %v", status, err)
	}

	_, err = c.UpdateConfiguration(ctx, &sdk.UpdateConfigurationInput{
		EcrConfiguration: &types.EcrConfiguration{RescanDuration: "DAYS_30"},
		Ec2Configuration: &types.Ec2Configuration{ScanMode: "EC2_SSM_AGENT_BASED"},
	})
	if err != nil {
		t.Fatal(err)
	}

	conf, err := c.GetConfiguration(ctx, &sdk.GetConfigurationInput{})
	if err != nil || conf.EcrConfiguration.RescanDurationState.RescanDuration != "DAYS_30" ||
		conf.Ec2Configuration.ScanModeState.ScanMode != "EC2_SSM_AGENT_BASED" {
		t.Fatalf("configuration: %+v %v", conf, err)
	}

	first, err := c.CreateFilter(ctx, &sdk.CreateFilterInput{
		Name: new("first"), Action: "SUPPRESS",
		FilterCriteria: inspector2Criteria(), Tags: map[string]string{"team": "core"}, Reason: new("test"),
	})
	if err != nil {
		t.Fatal(err)
	}

	foreign := sdk.New(sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("111122223333")})
	isolated, e := foreign.ListFilters(ctx, &sdk.ListFiltersInput{})
	if e != nil || len(isolated.Filters) != 0 {
		t.Fatalf("account isolation: %+v %v", isolated, e)
	}

	_, e = foreign.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: first.Arn, Tags: map[string]string{"foreign": "no"}},
	)
	inspector2Error(t, e, "ResourceNotFoundException")

	second, err := c.CreateFilter(
		ctx,
		&sdk.CreateFilterInput{Name: new("second"), Action: "NONE", FilterCriteria: inspector2Criteria()},
	)
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListFilters(ctx, &sdk.ListFiltersInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Filters) != 1 || page.NextToken == nil {
		t.Fatalf("page: %+v %v", page, err)
	}

	next, err := c.ListFilters(ctx, &sdk.ListFiltersInput{MaxResults: new(int32(1)), NextToken: page.NextToken})
	if err != nil || len(next.Filters) != 1 || next.NextToken != nil || *next.Filters[0].Arn == *page.Filters[0].Arn {
		t.Fatalf("next: %+v %v", next, err)
	}

	_, err = c.UpdateFilter(ctx, &sdk.UpdateFilterInput{
		FilterArn: first.Arn, Name: new("updated"), Action: "NONE",
		Description: new("updated description"), Reason: new("updated reason"), FilterCriteria: &types.FilterCriteria{
			ResourceId: []types.StringFilter{{Comparison: "PREFIX", Value: new("i-")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListFilters(ctx, &sdk.ListFiltersInput{Arns: []string{*first.Arn}, Action: "NONE"})
	if err != nil || len(listed.Filters) != 1 || *listed.Filters[0].Name != "updated" ||
		*listed.Filters[0].Criteria.ResourceId[0].Value != "i-" {
		t.Fatalf("updated: %+v %v", listed, err)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: first.Arn, Tags: map[string]string{"env": "test"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: first.Arn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: first.Arn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["env"] != "test" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	tc := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tc.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{*first.Arn},
		Tags:            map[string]string{"inventory": "yes"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("inventory tags: %+v %v", tagged, err)
	}

	for _, arn := range []*string{first.Arn, second.Arn} {
		if _, deleteErr := c.DeleteFilter(ctx, &sdk.DeleteFilterInput{Arn: arn}); deleteErr != nil {
			t.Fatal(deleteErr)
		}
	}

	_, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: first.Arn})
	inspector2Error(t, err, "ResourceNotFoundException")
	disabled, err := c.Disable(ctx, &sdk.DisableInput{ResourceTypes: []types.ResourceScanType{"EC2"}})
	if err != nil || disabled.Accounts[0].ResourceStatus.Ec2 != "DISABLED" ||
		disabled.Accounts[0].ResourceStatus.Ecr != "ENABLED" {
		t.Fatalf("partial disable: %+v %v", disabled, err)
	}

	disabled, err = c.Disable(ctx, &sdk.DisableInput{})
	if err != nil || disabled.Accounts[0].Status != "DISABLED" {
		t.Fatalf("disable: %+v %v", disabled, err)
	}
}

func TestInspector2EmptyReadsAndErrors(t *testing.T) {
	ctx := context.Background()
	c := inspector2Client(server(t))
	findings, err := c.ListFindings(ctx, &sdk.ListFindingsInput{FilterCriteria: inspector2Criteria()})
	if err != nil || len(findings.Findings) != 0 {
		t.Fatalf("findings: %+v %v", findings, err)
	}

	coverage, err := c.ListCoverage(ctx, &sdk.ListCoverageInput{})
	if err != nil || len(coverage.CoveredResources) != 0 {
		t.Fatalf("coverage: %+v %v", coverage, err)
	}

	statistics, err := c.ListCoverageStatistics(ctx, &sdk.ListCoverageStatisticsInput{GroupBy: "RESOURCE_TYPE"})
	if err != nil || statistics.TotalCounts == nil || *statistics.TotalCounts != 0 ||
		len(statistics.CountsByGroup) != 0 {
		t.Fatalf("statistics: %+v %v", statistics, err)
	}

	aggregations, err := c.ListFindingAggregations(ctx, &sdk.ListFindingAggregationsInput{
		AggregationType:    "ACCOUNT",
		AggregationRequest: &types.AggregationRequestMemberAccountAggregation{Value: types.AccountAggregation{}},
	})
	if err != nil || len(aggregations.Responses) != 0 {
		t.Fatalf("aggregations: %+v %v", aggregations, err)
	}

	details, err := c.BatchGetFindingDetails(ctx, &sdk.BatchGetFindingDetailsInput{
		FindingArns: []string{"arn:aws:inspector2:us-east-1:000000000000:finding/12345678901234567890123456789012"},
	})
	if err != nil || len(details.FindingDetails) != 0 {
		t.Fatalf("details: %+v %v", details, err)
	}

	usage, err := c.ListUsageTotals(ctx, &sdk.ListUsageTotalsInput{})
	if err != nil || len(usage.Totals) != 1 || len(usage.Totals[0].Usage) != 0 {
		t.Fatalf("usage: %+v %v", usage, err)
	}

	tests := []struct {
		name string
		call func() error
		code string
	}{
		{"foreign enable", func() error {
			_, e := c.Enable(
				ctx,
				&sdk.EnableInput{AccountIds: []string{"123456789012"}, ResourceTypes: []types.ResourceScanType{"EC2"}},
			)
			return e
		}, "ValidationException"},
		{"foreign disable", func() error {
			_, e := c.Disable(ctx, &sdk.DisableInput{AccountIds: []string{"123456789012"}})
			return e
		}, "ValidationException"},
		{"foreign status", func() error {
			_, e := c.BatchGetAccountStatus(ctx, &sdk.BatchGetAccountStatusInput{AccountIds: []string{"123456789012"}})
			return e
		}, "ValidationException"},
		{"foreign config", func() error {
			_, e := c.GetConfiguration(ctx, &sdk.GetConfigurationInput{AccountId: new("123456789012")})
			return e
		}, "ValidationException"},
		{"foreign update", func() error {
			_, e := c.UpdateConfiguration(
				ctx,
				&sdk.UpdateConfigurationInput{
					AccountId:        new("123456789012"),
					Ec2Configuration: &types.Ec2Configuration{ScanMode: "EC2_SSM_AGENT_BASED"},
				},
			)
			return e
		}, "ValidationException"},
		{"foreign usage", func() error {
			_, e := c.ListUsageTotals(ctx, &sdk.ListUsageTotalsInput{AccountIds: []string{"123456789012"}})
			return e
		}, "ValidationException"},
		{"action enum", func() error {
			_, e := c.CreateFilter(
				ctx,
				&sdk.CreateFilterInput{Name: new("invalid"), Action: "INVALID", FilterCriteria: inspector2Criteria()},
			)
			return e
		}, "ValidationException"},
		{"comparison enum", func() error {
			_, e := c.CreateFilter(
				ctx,
				&sdk.CreateFilterInput{
					Name:   new("invalid"),
					Action: "NONE",
					FilterCriteria: &types.FilterCriteria{
						Severity: []types.StringFilter{{Comparison: "INVALID", Value: new("LOW")}},
					},
				},
			)
			return e
		}, "ValidationException"},
		{"empty name", func() error {
			_, e := c.CreateFilter(
				ctx,
				&sdk.CreateFilterInput{Name: new(""), Action: "NONE", FilterCriteria: inspector2Criteria()},
			)
			return e
		}, "ValidationException"},
		{
			"bad token",
			func() error { _, e := c.ListFilters(ctx, &sdk.ListFiltersInput{NextToken: new("!")}); return e },
			"ValidationException",
		},
		{
			"bad findings token",
			func() error { _, e := c.ListFindings(ctx, &sdk.ListFindingsInput{NextToken: new("!")}); return e },
			"ValidationException",
		},
		{"bad bounds", func() error {
			_, e := c.ListFindings(
				ctx,
				&sdk.ListFindingsInput{
					FilterCriteria: &types.FilterCriteria{
						InspectorScore: []types.NumberFilter{
							{LowerInclusive: new(float64(8)), UpperInclusive: new(float64(2))},
						},
					},
				},
			)
			return e
		}, "ValidationException"},
	}

	tests = append(tests, struct {
		name string
		call func() error
		code string
	}{
		name: "unsupported filter member", call: func() error {
			_, e := c.CreateFilter(
				ctx,
				&sdk.CreateFilterInput{Name: new("unsupported"), Action: "NONE", FilterCriteria: &types.FilterCriteria{
					CodeVulnerabilityDetectorName: []types.StringFilter{{Comparison: "EQUALS", Value: new("detector")}},
				}},
			)
			return e
		}, code: "NotImplemented",
	}, struct {
		name string
		call func() error
		code string
	}{
		name: "unsupported coverage member", call: func() error {
			_, e := c.ListCoverage(ctx, &sdk.ListCoverageInput{FilterCriteria: &types.CoverageFilterCriteria{
				CodeRepositoryProjectName: []types.CoverageStringFilter{{Comparison: "EQUALS", Value: new("project")}},
			}})
			return e
		}, code: "NotImplemented",
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { inspector2Error(t, tt.call(), tt.code) })
	}
}

func TestInspector2SQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := inspector2Client(s)
	created, err := c.CreateFilter(ctx, &sdk.CreateFilterInput{
		Name: new("persistent"), Action: "SUPPRESS",
		FilterCriteria: inspector2Criteria(), Tags: map[string]string{"persisted": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, enableErr := c.Enable(
		ctx,
		&sdk.EnableInput{ResourceTypes: []types.ResourceScanType{"EC2"}},
	); enableErr != nil {
		t.Fatal(enableErr)
	}

	if _, updateErr := c.UpdateConfiguration(
		ctx,
		&sdk.UpdateConfigurationInput{EcrConfiguration: &types.EcrConfiguration{
			RescanDuration: "DAYS_30",
		}},
	); updateErr != nil {
		t.Fatal(updateErr)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = inspector2Client(s)
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: created.Arn})
	if err != nil || tags.Tags["persisted"] != "yes" {
		t.Fatalf("persisted tags: %+v %v", tags, err)
	}

	filters, err := c.ListFilters(ctx, &sdk.ListFiltersInput{})
	if err != nil || len(filters.Filters) != 1 || *filters.Filters[0].Criteria.Severity[0].Value != "LOW" {
		t.Fatalf("persisted filters: %+v %v", filters, err)
	}

	status, err := c.BatchGetAccountStatus(ctx, &sdk.BatchGetAccountStatusInput{})
	if err != nil || status.Accounts[0].ResourceState.Ec2.Status != "ENABLED" {
		t.Fatalf("persisted state: %+v %v", status, err)
	}

	conf, err := c.GetConfiguration(ctx, &sdk.GetConfigurationInput{})
	if err != nil || conf.EcrConfiguration.RescanDurationState.RescanDuration != "DAYS_30" {
		t.Fatalf("persisted configuration: %+v %v", conf, err)
	}
}

func TestInspector2CloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := `{"Resources":{"Filter":{"Type":"AWS::InspectorV2::Filter","Properties":{
 "Name":"stack-filter","Description":"first","FilterAction":"SUPPRESS",
 "FilterCriteria":{"Severity":[{"Comparison":"EQUALS","Value":"LOW"}]},
 "Tags":{"team":"core"}} }},"Outputs":{"Arn":{"Value":{"Fn::GetAtt":["Filter","Arn"]}}}}`
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("inspector-filter"), TemplateBody: new(template)})
	if err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, c, "inspector-filter", cfntypes.StackStatusCreateComplete)
	ic := inspector2Client(s)
	filters, err := ic.ListFilters(ctx, &sdk.ListFiltersInput{})
	if err != nil || len(filters.Filters) != 1 || *filters.Filters[0].Name != "stack-filter" {
		t.Fatalf("stack filter: %+v %v", filters, err)
	}

	_, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName: new("inspector-filter"),
			TemplateBody: new(strings.NewReplacer("core", "platform", "stack-filter", "updated-stack-filter",
				"SUPPRESS", "NONE", "LOW", "HIGH", "first", "second").Replace(template)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "inspector-filter", cfntypes.StackStatusUpdateComplete)
	updated, err := ic.ListFilters(ctx, &sdk.ListFiltersInput{})
	if err != nil || len(updated.Filters) != 1 || *updated.Filters[0].Arn != *filters.Filters[0].Arn ||
		*updated.Filters[0].Name != "updated-stack-filter" || updated.Filters[0].Action != "NONE" ||
		*updated.Filters[0].Criteria.Severity[0].Value != "HIGH" || *updated.Filters[0].Description != "second" {
		t.Fatalf("stack update: %+v %v", updated, err)
	}

	// UpdateFilter cannot clear a description, so removing it replaces the filter.
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    new("inspector-filter"),
		TemplateBody: new(strings.Replace(template, `"Description":"first",`, "", 1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "inspector-filter", cfntypes.StackStatusUpdateComplete)
	replaced, err := ic.ListFilters(ctx, &sdk.ListFiltersInput{})
	if err != nil || len(replaced.Filters) != 1 || *replaced.Filters[0].Arn == *filters.Filters[0].Arn ||
		replaced.Filters[0].Description != nil {
		t.Fatalf("stack replacement: %+v %v", replaced, err)
	}

	tags, err := ic.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: replaced.Filters[0].Arn})
	if err != nil || tags.Tags["team"] != "core" {
		t.Fatalf("stack tags: %+v %v", tags, err)
	}

	if _, deleteErr := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("inspector-filter")}); deleteErr != nil {
		t.Fatal(deleteErr)
	}

	wantStatus(ctx, t, c, *stack.StackId, cfntypes.StackStatusDeleteComplete)
	filters, err = ic.ListFilters(ctx, &sdk.ListFiltersInput{})
	if err != nil || len(filters.Filters) != 0 {
		t.Fatalf("stack deletion: %+v %v", filters, err)
	}
}

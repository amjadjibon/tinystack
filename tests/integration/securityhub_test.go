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
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/securityhub"
	"github.com/aws/aws-sdk-go-v2/service/securityhub/types"
	"github.com/aws/smithy-go"
)

func securityHubClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func securityHubError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func shFinding(id, title, updated string) types.AwsSecurityFinding {
	return types.AwsSecurityFinding{
		SchemaVersion: new(
			"2018-10-08",
		),
		Id:         new(id),
		ProductArn: new("arn:aws:securityhub:us-east-1:000000000000:product/000000000000/default"),
		GeneratorId: new(
			"local",
		),
		AwsAccountId: new("000000000000"),
		Types:        []string{"Software and Configuration Checks"},
		CreatedAt: new(
			"2026-01-01T00:00:00Z",
		),
		UpdatedAt: new(updated),
		Severity:  &types.Severity{Label: types.SeverityLabelHigh},
		Title: new(
			title,
		),
		Description: new("Stored local finding"),
		Resources:   []types.Resource{{Type: new("AwsS3Bucket"), Id: new("arn:aws:s3:::local")}},
		Compliance:  &types.Compliance{Status: types.ComplianceStatusFailed},
	}
}

func TestSecurityHubLifecycle(t *testing.T) {
	s := server(t)
	c := securityHubClient(s)
	ctx := context.Background()
	_, err := c.DescribeHub(ctx, &sdk.DescribeHubInput{})
	securityHubError(t, err, "InvalidAccessException")
	_, err = c.GetFindings(ctx, &sdk.GetFindingsInput{})
	securityHubError(t, err, "InvalidAccessException")
	_, err = c.DescribeStandards(ctx, &sdk.DescribeStandardsInput{})
	securityHubError(t, err, "InvalidAccessException")
	_, err = c.EnableSecurityHub(ctx, &sdk.EnableSecurityHubInput{Tags: map[string]string{"team": "security"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.EnableSecurityHub(ctx, &sdk.EnableSecurityHubInput{})
	securityHubError(t, err, "ResourceConflictException")
	hub, err := c.DescribeHub(ctx, &sdk.DescribeHubInput{})
	if err != nil || aws.ToString(hub.HubArn) != "arn:aws:securityhub:us-east-1:000000000000:hub/default" ||
		hub.ControlFindingGenerator != types.ControlFindingGeneratorSecurityControl {
		t.Fatalf("hub: %+v %v", hub, err)
	}

	other := sdk.New(
		sdk.Options{
			Region:       "eu-west-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	_, err = other.DescribeHub(ctx, &sdk.DescribeHubInput{})
	securityHubError(t, err, "InvalidAccessException")
	otherAccount := sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("111111111111"),
			Retryer:      aws.NopRetryer{},
		},
	)
	_, err = otherAccount.DescribeHub(ctx, &sdk.DescribeHubInput{})
	securityHubError(t, err, "InvalidAccessException")
	_, err = c.UpdateSecurityHubConfiguration(
		ctx,
		&sdk.UpdateSecurityHubConfigurationInput{
			ControlFindingGenerator: types.ControlFindingGeneratorStandardControl,
			AutoEnableControls:      new(false),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	hub, err = c.DescribeHub(ctx, &sdk.DescribeHubInput{})
	if err != nil || hub.ControlFindingGenerator != types.ControlFindingGeneratorStandardControl ||
		aws.ToBool(hub.AutoEnableControls) {
		t.Fatalf("update: %+v %v", hub, err)
	}

	catalogue, err := c.DescribeStandards(ctx, &sdk.DescribeStandardsInput{MaxResults: new(int32(2))})
	if err != nil || len(catalogue.Standards) != 2 || catalogue.NextToken == nil {
		t.Fatalf("catalogue: %+v %v", catalogue, err)
	}

	next, err := c.DescribeStandards(ctx, &sdk.DescribeStandardsInput{NextToken: catalogue.NextToken})
	if err != nil || len(next.Standards) != 4 {
		t.Fatalf("catalogue page: %+v %v", next, err)
	}

	enabled, err := c.GetEnabledStandards(ctx, &sdk.GetEnabledStandardsInput{})
	if err != nil || len(enabled.StandardsSubscriptions) != 2 {
		t.Fatalf("defaults: %+v %v", enabled, err)
	}

	for _, sub := range enabled.StandardsSubscriptions {
		if sub.StandardsStatus != types.StandardsStatusReady {
			t.Fatalf("status: %+v", sub)
		}
	}

	const cis = "arn:aws:securityhub:us-east-1::standards/cis-aws-foundations-benchmark/v/3.0.0"
	added, err := c.BatchEnableStandards(
		ctx,
		&sdk.BatchEnableStandardsInput{
			StandardsSubscriptionRequests: []types.StandardsSubscriptionRequest{{StandardsArn: new(cis)}},
		},
	)
	if err != nil || len(added.StandardsSubscriptions) != 1 ||
		aws.ToString(
			added.StandardsSubscriptions[0].StandardsSubscriptionArn,
		) != "arn:aws:securityhub:us-east-1:000000000000:subscription/cis-aws-foundations-benchmark/v/3.0.0" {
		t.Fatalf("enable: %+v %v", added, err)
	}

	_, err = c.BatchEnableStandards(
		ctx,
		&sdk.BatchEnableStandardsInput{
			StandardsSubscriptionRequests: []types.StandardsSubscriptionRequest{{StandardsArn: new(cis)}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.BatchEnableStandards(
		ctx,
		&sdk.BatchEnableStandardsInput{
			StandardsSubscriptionRequests: []types.StandardsSubscriptionRequest{
				{StandardsArn: new("arn:aws:securityhub:us-east-1::standards/invented/v/1")},
			},
		},
	)
	securityHubError(t, err, "InvalidInputException")
	_, err = c.BatchDisableStandards(
		ctx,
		&sdk.BatchDisableStandardsInput{
			StandardsSubscriptionArns: []string{aws.ToString(added.StandardsSubscriptions[0].StandardsSubscriptionArn)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: hub.HubArn, Tags: map[string]string{"environment": "local"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: hub.HubArn})
	if err != nil || tags.Tags["environment"] != "local" || tags.Tags["team"] != "security" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	taggingClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := taggingClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(hub.HubArn)},
			Tags:            map[string]string{"source": "rgta"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tag resources: %+v %v", tagged, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: hub.HubArn, TagKeys: []string{"environment"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: hub.HubArn})
	if err != nil || tags.Tags["environment"] != "" || tags.Tags["source"] != "rgta" {
		t.Fatalf("untag: %+v %v", tags, err)
	}

	action, err := c.CreateActionTarget(
		ctx,
		&sdk.CreateActionTargetInput{Id: new("review"), Name: new("Review"), Description: new("Review finding")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateActionTarget(
		ctx,
		&sdk.UpdateActionTargetInput{ActionTargetArn: action.ActionTargetArn, Name: new("Updated")},
	)
	if err != nil {
		t.Fatal(err)
	}

	actions, err := c.DescribeActionTargets(
		ctx,
		&sdk.DescribeActionTargetsInput{ActionTargetArns: []string{aws.ToString(action.ActionTargetArn)}},
	)
	if err != nil || len(actions.ActionTargets) != 1 || aws.ToString(actions.ActionTargets[0].Name) != "Updated" {
		t.Fatalf("actions: %+v %v", actions, err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: action.ActionTargetArn, Tags: map[string]string{"team": "security"}},
	)
	securityHubError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteActionTarget(ctx, &sdk.DeleteActionTargetInput{ActionTargetArn: action.ActionTargetArn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteActionTarget(ctx, &sdk.DeleteActionTargetInput{ActionTargetArn: action.ActionTargetArn})
	securityHubError(t, err, "ResourceNotFoundException")
	_, err = c.DisableSecurityHub(ctx, &sdk.DisableSecurityHubInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: hub.HubArn})
	securityHubError(t, err, "InvalidAccessException")
	_, err = c.EnableSecurityHub(ctx, &sdk.EnableSecurityHubInput{EnableDefaultStandards: new(false)})
	if err != nil {
		t.Fatal(err)
	}

	enabled, err = c.GetEnabledStandards(ctx, &sdk.GetEnabledStandardsInput{})
	if err != nil || len(enabled.StandardsSubscriptions) != 0 {
		t.Fatalf("disabled defaults: %+v %v", enabled, err)
	}
}

func TestSecurityHubFindingsAndInsights(t *testing.T) {
	s := server(t)
	c := securityHubClient(s)
	ctx := context.Background()
	if _, err := c.EnableSecurityHub(ctx, &sdk.EnableSecurityHubInput{EnableDefaultStandards: new(false)}); err != nil {
		t.Fatal(err)
	}

	a := shFinding("a", "Zulu", "2026-01-02T00:00:00Z")
	b := shFinding("b", "Alpha", "2026-01-03T00:00:00Z")
	bad := shFinding("bad", "Bad", "invalid")
	imported, err := c.BatchImportFindings(
		ctx,
		&sdk.BatchImportFindingsInput{Findings: []types.AwsSecurityFinding{a, b, bad}},
	)
	if err != nil || aws.ToInt32(imported.SuccessCount) != 2 || aws.ToInt32(imported.FailedCount) != 1 ||
		len(imported.FailedFindings) != 1 {
		t.Fatalf("import: %+v %v", imported, err)
	}

	a.Title = new("Replaced")
	if _, err = c.BatchImportFindings(
		ctx,
		&sdk.BatchImportFindingsInput{Findings: []types.AwsSecurityFinding{a}},
	); err != nil {
		t.Fatal(err)
	}

	filters := &types.AwsSecurityFindingFilters{
		GeneratorId: []types.StringFilter{{Comparison: types.StringFilterComparisonPrefix, Value: new("loc")}},
		ResourceType: []types.StringFilter{
			{Comparison: types.StringFilterComparisonEquals, Value: new("AwsS3Bucket")},
		},
		WorkflowStatus: []types.StringFilter{
			{Comparison: types.StringFilterComparisonNotEquals, Value: new("RESOLVED")},
		},
		CreatedAt: []types.DateFilter{{Start: new("2025-12-31T23:00:00-01:00"), End: new("2026-01-02T00:00:00Z")}},
	}

	input := &sdk.GetFindingsInput{
		Filters:      filters,
		SortCriteria: []types.SortCriterion{{Field: new("Title"), SortOrder: types.SortOrderAscending}},
		MaxResults:   new(int32(1)),
	}

	first, err := c.GetFindings(ctx, input)
	if err != nil || len(first.Findings) != 1 || aws.ToString(first.Findings[0].Id) != "b" || first.NextToken == nil {
		t.Fatalf("sorted: %+v %v", first, err)
	}

	input.NextToken = first.NextToken
	second, err := c.GetFindings(ctx, input)
	if err != nil || len(second.Findings) != 1 || aws.ToString(second.Findings[0].Id) != "a" ||
		second.NextToken != nil ||
		aws.ToString(second.Findings[0].Title) != "Replaced" {
		t.Fatalf("page: %+v %v", second, err)
	}

	descending, err := c.GetFindings(
		ctx,
		&sdk.GetFindingsInput{
			SortCriteria: []types.SortCriterion{{Field: new("UpdatedAt"), SortOrder: types.SortOrderDescending}},
		},
	)
	if err != nil || len(descending.Findings) != 2 || aws.ToString(descending.Findings[0].Id) != "b" {
		t.Fatalf("descending: %+v %v", descending, err)
	}

	updated, err := c.BatchUpdateFindings(ctx, &sdk.BatchUpdateFindingsInput{
		FindingIdentifiers: []types.AwsSecurityFindingIdentifier{
			{Id: a.Id, ProductArn: a.ProductArn},
			{Id: new("missing"), ProductArn: a.ProductArn},
		},
		Workflow: &types.WorkflowUpdate{
			Status: types.WorkflowStatusResolved,
		},
		Severity: &types.SeverityUpdate{Label: types.SeverityLabelLow},
		Note: &types.NoteUpdate{
			Text:      new("Reviewed"),
			UpdatedBy: new("tester"),
		},
		Confidence:        new(int32(90)),
		Criticality:       new(int32(50)),
		VerificationState: types.VerificationStateTruePositive,
		Types:             []string{"Effects/Data Exposure"},
		UserDefinedFields: map[string]string{"owner": "local"},
		RelatedFindings:   []types.RelatedFinding{{Id: b.Id, ProductArn: b.ProductArn}},
	})

	if err != nil || len(updated.ProcessedFindings) != 1 || len(updated.UnprocessedFindings) != 1 {
		t.Fatalf("update: %+v %v", updated, err)
	}

	result, err := c.GetFindings(
		ctx,
		&sdk.GetFindingsInput{
			Filters: &types.AwsSecurityFindingFilters{
				Id: []types.StringFilter{{Comparison: types.StringFilterComparisonEquals, Value: a.Id}},
			},
		},
	)
	if err != nil || len(result.Findings) != 1 {
		t.Fatalf("get: %+v %v", result, err)
	}

	f := result.Findings[0]
	if f.Workflow.Status != types.WorkflowStatusResolved || f.Severity.Label != types.SeverityLabelLow ||
		aws.ToInt32(f.Confidence) != 90 ||
		aws.ToInt32(f.Criticality) != 50 ||
		f.Note == nil ||
		f.Note.UpdatedAt == nil ||
		aws.ToString(f.UpdatedAt) == aws.ToString(a.UpdatedAt) ||
		f.UserDefinedFields["owner"] != "local" ||
		len(f.RelatedFindings) != 1 ||
		f.VerificationState != types.VerificationStateTruePositive {
		t.Fatalf("updated finding: %+v", f)
	}

	insight, err := c.CreateInsight(
		ctx,
		&sdk.CreateInsightInput{
			Name: new("Active findings"),
			Filters: &types.AwsSecurityFindingFilters{
				RecordState: []types.StringFilter{
					{Comparison: types.StringFilterComparisonEquals, Value: new("ACTIVE")},
				},
			},
			GroupByAttribute: new("SeverityLabel"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	groups, err := c.GetInsightResults(ctx, &sdk.GetInsightResultsInput{InsightArn: insight.InsightArn})
	if err != nil || len(groups.InsightResults.ResultValues) != 2 {
		t.Fatalf("groups: %+v %v", groups, err)
	}

	_, err = c.UpdateInsight(
		ctx,
		&sdk.UpdateInsightInput{
			InsightArn:       insight.InsightArn,
			Name:             new("Updated"),
			GroupByAttribute: new("ResourceType"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	insights, err := c.GetInsights(ctx, &sdk.GetInsightsInput{InsightArns: []string{aws.ToString(insight.InsightArn)}})
	if err != nil || len(insights.Insights) != 1 || aws.ToString(insights.Insights[0].Name) != "Updated" {
		t.Fatalf("insights: %+v %v", insights, err)
	}

	groups, err = c.GetInsightResults(ctx, &sdk.GetInsightResultsInput{InsightArn: insight.InsightArn})
	if err != nil || len(groups.InsightResults.ResultValues) != 1 ||
		aws.ToInt32(groups.InsightResults.ResultValues[0].Count) != 2 {
		t.Fatalf("resource groups: %+v %v", groups, err)
	}

	_, err = c.DeleteInsight(ctx, &sdk.DeleteInsightInput{InsightArn: insight.InsightArn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetInsightResults(ctx, &sdk.GetInsightResultsInput{InsightArn: insight.InsightArn})
	securityHubError(t, err, "ResourceNotFoundException")
	_, err = c.GetFindings(
		ctx,
		&sdk.GetFindingsInput{
			Filters: &types.AwsSecurityFindingFilters{
				Title: []types.StringFilter{{Comparison: types.StringFilterComparisonEquals, Value: new("x")}},
			},
		},
	)
	securityHubError(t, err, "NotImplemented")
	_, err = c.GetFindings(
		ctx,
		&sdk.GetFindingsInput{
			Filters: &types.AwsSecurityFindingFilters{
				Id: []types.StringFilter{{Comparison: types.StringFilterComparisonContains, Value: new("a")}},
			},
		},
	)
	securityHubError(t, err, "InvalidInputException")
	_, err = c.GetFindings(ctx, &sdk.GetFindingsInput{NextToken: new("!")})
	securityHubError(t, err, "InvalidInputException")
	_, err = c.GetFindings(
		ctx,
		&sdk.GetFindingsInput{
			SortCriteria: []types.SortCriterion{{Field: new("Unimplemented"), SortOrder: types.SortOrderAscending}},
		},
	)
	securityHubError(t, err, "InvalidInputException")
	_, err = c.CreateInsight(
		ctx,
		&sdk.CreateInsightInput{
			Name:             new("Bad group"),
			Filters:          &types.AwsSecurityFindingFilters{},
			GroupByAttribute: new("Unimplemented"),
		},
	)
	securityHubError(t, err, "InvalidInputException")
	_, err = c.BatchUpdateFindings(
		ctx,
		&sdk.BatchUpdateFindingsInput{
			FindingIdentifiers: []types.AwsSecurityFindingIdentifier{{Id: a.Id, ProductArn: a.ProductArn}},
			Confidence:         new(int32(101)),
		},
	)
	securityHubError(t, err, "InvalidInputException")
}

func TestSecurityHubPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := securityHubClient(s)
	if _, err := c.EnableSecurityHub(
		ctx,
		&sdk.EnableSecurityHubInput{Tags: map[string]string{"persisted": "yes"}},
	); err != nil {
		t.Fatal(err)
	}

	finding := shFinding("persisted", "Persistent", "2026-01-02T00:00:00Z")
	if _, err := c.BatchImportFindings(
		ctx,
		&sdk.BatchImportFindingsInput{Findings: []types.AwsSecurityFinding{finding}},
	); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = securityHubClient(s)
	hub, err := c.DescribeHub(ctx, &sdk.DescribeHubInput{})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: hub.HubArn})
	if err != nil || tags.Tags["persisted"] != "yes" {
		t.Fatalf("persisted tags: %+v %v", tags, err)
	}

	findings, err := c.GetFindings(ctx, &sdk.GetFindingsInput{})
	if err != nil || len(findings.Findings) != 1 {
		t.Fatalf("persisted findings: %+v %v", findings, err)
	}

	standards, err := c.GetEnabledStandards(ctx, &sdk.GetEnabledStandardsInput{})
	if err != nil || len(standards.StandardsSubscriptions) != 2 {
		t.Fatalf("persisted standards: %+v %v", standards, err)
	}
}

func TestSecurityHubCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := `{
  "Resources": {
    "Hub": {
      "Type": "AWS::SecurityHub::Hub",
      "Properties": {
        "EnableDefaultStandards": false,
        "AutoEnableControls": true,
        "Tags": [
          {
            "Key": "team",
            "Value": "security"
          }
        ]
      }
    },
    "Standard": {
      "Type": "AWS::SecurityHub::Standard",
      "DependsOn": "Hub",
      "Properties": {
        "StandardsArn": "arn:aws:securityhub:us-east-1::standards/nist-800-53/v/5.0.0"
      }
    },
    "Insight": {
      "Type": "AWS::SecurityHub::Insight",
      "DependsOn": "Hub",
      "Properties": {
        "Name": "Review",
        "Filters": {
          "RecordState": [
            {
              "Comparison": "EQUALS",
              "Value": "ACTIVE"
            }
          ]
        },
        "GroupByAttribute": "SeverityLabel"
      }
    }
  },
  "Outputs": {
    "HubAttribute": { "Value": { "Fn::GetAtt": ["Hub", "ARN"] } },
    "SubscribedAt": { "Value": { "Fn::GetAtt": ["Hub", "SubscribedAt"] } },
    "HubArn": {
      "Value": {
        "Ref": "Hub"
      }
    },
    "Subscription": {
      "Value": {
        "Fn::GetAtt": [
          "Standard",
          "StandardsSubscriptionArn"
        ]
      }
    },
    "InsightArn": {
      "Value": {
        "Ref": "Insight"
      }
    }
  }
}`
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("securityhub"), TemplateBody: new(template)})
	if err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, c, "securityhub", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	if outputs["Subscription"] == "" || outputs["HubArn"] == "" || outputs["InsightArn"] == "" ||
		outputs["SubscribedAt"] == "" || outputs["HubAttribute"] != outputs["HubArn"] {
		t.Fatalf("missing outputs: %+v", outputs)
	}

	sh := securityHubClient(s)
	enabled, err := sh.GetEnabledStandards(ctx, &sdk.GetEnabledStandardsInput{})
	if err != nil || len(enabled.StandardsSubscriptions) != 1 {
		t.Fatalf("stack standards: %+v %v", enabled, err)
	}

	updatedTemplate := strings.Replace(template, `"EnableDefaultStandards": false`, `"EnableDefaultStandards": true`, 1)
	updatedTemplate = strings.Replace(updatedTemplate, `"AutoEnableControls": true`, `"AutoEnableControls": false`, 1)
	updatedTemplate = strings.Replace(updatedTemplate, `"Value": "security"`, `"Value": "updated"`, 1)
	if _, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("securityhub"), TemplateBody: new(updatedTemplate)},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "securityhub", cfntypes.StackStatusUpdateComplete)
	enabled, err = sh.GetEnabledStandards(ctx, &sdk.GetEnabledStandardsInput{})
	if err != nil || len(enabled.StandardsSubscriptions) != 3 {
		t.Fatalf("updated defaults: %+v %v", enabled, err)
	}

	hub, err := sh.DescribeHub(ctx, &sdk.DescribeHubInput{})
	if err != nil || aws.ToBool(hub.AutoEnableControls) {
		t.Fatalf("updated hub: %+v %v", hub, err)
	}

	tags, err := sh.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: hub.HubArn})
	if err != nil || tags.Tags["team"] != "updated" {
		t.Fatalf("updated tags: %+v %v", tags, err)
	}

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("securityhub")}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	_, err = sh.DescribeHub(ctx, &sdk.DescribeHubInput{})
	securityHubError(t, err, "InvalidAccessException")
}

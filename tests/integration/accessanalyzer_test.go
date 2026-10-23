package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/accessanalyzer"
	"github.com/aws/aws-sdk-go-v2/service/accessanalyzer/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func accessAnalyzerClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func wantAccessAnalyzerError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func TestAccessAnalyzerLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := accessAnalyzerClient(s)

	// 1. CreateAnalyzer with inline archive rule and tags
	createOut, err := c.CreateAnalyzer(ctx, &sdk.CreateAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
		Type:         types.TypeAccount,
		Tags:         map[string]string{"env": "staging", "team": "security"},
		ArchiveRules: []types.InlineArchiveRule{
			{
				RuleName: aws.String("rule-inline"),
				Filter: map[string]types.Criterion{
					"isPublic": {
						Eq: []string{"false"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateAnalyzer: %v", err)
	}

	arn := *createOut.Arn
	if arn == "" {
		t.Fatal("expected non-empty ARN")
	}

	// Conflict on duplicate analyzer name
	_, err = c.CreateAnalyzer(ctx, &sdk.CreateAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
		Type:         types.TypeAccount,
	})
	wantAccessAnalyzerError(t, err, "ConflictException")

	// 2. GetAnalyzer
	getOut, err := c.GetAnalyzer(ctx, &sdk.GetAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
	})
	if err != nil {
		t.Fatalf("GetAnalyzer: %v", err)
	}

	if *getOut.Analyzer.Name != "primary-analyzer" {
		t.Fatalf("want name primary-analyzer, got %s", *getOut.Analyzer.Name)
	}

	if getOut.Analyzer.Status != types.AnalyzerStatusActive {
		t.Fatalf("want status ACTIVE, got %v", getOut.Analyzer.Status)
	}

	if getOut.Analyzer.Tags["env"] != "staging" {
		t.Fatalf("want env=staging tag, got %v", getOut.Analyzer.Tags)
	}

	// 3. ListAnalyzers
	listOut, err := c.ListAnalyzers(ctx, &sdk.ListAnalyzersInput{
		MaxResults: aws.Int32(10),
	})
	if err != nil {
		t.Fatalf("ListAnalyzers: %v", err)
	}

	if len(listOut.Analyzers) != 1 {
		t.Fatalf("want 1 analyzer, got %d", len(listOut.Analyzers))
	}

	// Filter by type: ORGANIZATION should return 0
	listOrg, err := c.ListAnalyzers(ctx, &sdk.ListAnalyzersInput{
		Type: types.TypeOrganization,
	})
	if err != nil {
		t.Fatalf("ListAnalyzers (org): %v", err)
	}

	if len(listOrg.Analyzers) != 0 {
		t.Fatalf("want 0 organization analyzers, got %d", len(listOrg.Analyzers))
	}

	// 4. UpdateAnalyzer
	_, err = c.UpdateAnalyzer(ctx, &sdk.UpdateAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
		Configuration: &types.AnalyzerConfigurationMemberUnusedAccess{
			Value: types.UnusedAccessConfiguration{
				UnusedAccessAge: aws.Int32(60),
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateAnalyzer: %v", err)
	}

	getAfterUpdate, err := c.GetAnalyzer(ctx, &sdk.GetAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
	})
	if err != nil {
		t.Fatalf("GetAnalyzer after update: %v", err)
	}

	member, ok := getAfterUpdate.Analyzer.Configuration.(*types.AnalyzerConfigurationMemberUnusedAccess)
	if !ok || member.Value.UnusedAccessAge == nil || *member.Value.UnusedAccessAge != 60 {
		t.Fatalf("expected unusedAccessAge to be 60, got %v", getAfterUpdate.Analyzer.Configuration)
	}

	// 5. ArchiveRules: Get inline rule
	ruleGet, err := c.GetArchiveRule(ctx, &sdk.GetArchiveRuleInput{
		AnalyzerName: aws.String("primary-analyzer"),
		RuleName:     aws.String("rule-inline"),
	})
	if err != nil {
		t.Fatalf("GetArchiveRule: %v", err)
	}

	if *ruleGet.ArchiveRule.RuleName != "rule-inline" {
		t.Fatalf("want rule-inline, got %s", *ruleGet.ArchiveRule.RuleName)
	}

	// CreateArchiveRule
	_, err = c.CreateArchiveRule(ctx, &sdk.CreateArchiveRuleInput{
		AnalyzerName: aws.String("primary-analyzer"),
		RuleName:     aws.String("rule-explicit"),
		Filter: map[string]types.Criterion{
			"resource": {
				Contains: []string{"arn:aws:s3:::example-bucket"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateArchiveRule: %v", err)
	}

	// Conflict on duplicate rule name
	_, err = c.CreateArchiveRule(ctx, &sdk.CreateArchiveRuleInput{
		AnalyzerName: aws.String("primary-analyzer"),
		RuleName:     aws.String("rule-explicit"),
		Filter: map[string]types.Criterion{
			"resource": {
				Contains: []string{"arn:aws:s3:::example-bucket"},
			},
		},
	})
	wantAccessAnalyzerError(t, err, "ConflictException")

	// ListArchiveRules
	rulesList, err := c.ListArchiveRules(ctx, &sdk.ListArchiveRulesInput{
		AnalyzerName: aws.String("primary-analyzer"),
	})
	if err != nil {
		t.Fatalf("ListArchiveRules: %v", err)
	}

	if len(rulesList.ArchiveRules) != 2 {
		t.Fatalf("want 2 archive rules, got %d", len(rulesList.ArchiveRules))
	}

	// UpdateArchiveRule
	_, err = c.UpdateArchiveRule(ctx, &sdk.UpdateArchiveRuleInput{
		AnalyzerName: aws.String("primary-analyzer"),
		RuleName:     aws.String("rule-explicit"),
		Filter: map[string]types.Criterion{
			"resource": {
				Contains: []string{"arn:aws:s3:::updated-bucket"},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateArchiveRule: %v", err)
	}

	// ApplyArchiveRule
	_, err = c.ApplyArchiveRule(ctx, &sdk.ApplyArchiveRuleInput{
		AnalyzerArn: aws.String(arn),
		RuleName:    aws.String("rule-explicit"),
	})
	if err != nil {
		t.Fatalf("ApplyArchiveRule: %v", err)
	}

	// DeleteArchiveRule
	_, err = c.DeleteArchiveRule(ctx, &sdk.DeleteArchiveRuleInput{
		AnalyzerName: aws.String("primary-analyzer"),
		RuleName:     aws.String("rule-explicit"),
	})
	if err != nil {
		t.Fatalf("DeleteArchiveRule: %v", err)
	}

	_, err = c.GetArchiveRule(ctx, &sdk.GetArchiveRuleInput{
		AnalyzerName: aws.String("primary-analyzer"),
		RuleName:     aws.String("rule-explicit"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	// 6. Findings (empty / not found)
	findingsList, err := c.ListFindings(ctx, &sdk.ListFindingsInput{
		AnalyzerArn: aws.String(arn),
	})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}

	if len(findingsList.Findings) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(findingsList.Findings))
	}

	findingsV2List, err := c.ListFindingsV2(ctx, &sdk.ListFindingsV2Input{
		AnalyzerArn: aws.String(arn),
	})
	if err != nil {
		t.Fatalf("ListFindingsV2: %v", err)
	}

	if len(findingsV2List.Findings) != 0 {
		t.Fatalf("expected 0 findings v2, got %d", len(findingsV2List.Findings))
	}

	_, err = c.GetFinding(ctx, &sdk.GetFindingInput{
		AnalyzerArn: aws.String(arn),
		Id:          aws.String("non-existent-finding"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	_, err = c.GetFindingV2(ctx, &sdk.GetFindingV2Input{
		AnalyzerArn: aws.String(arn),
		Id:          aws.String("non-existent-finding-v2"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	_, err = c.UpdateFindings(ctx, &sdk.UpdateFindingsInput{
		AnalyzerArn: aws.String(arn),
		Ids:         []string{"unknown-finding"},
		Status:      types.FindingStatusUpdateArchived,
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")

	// 7. Tags: TagResource, ListTagsForResource, UntagResource
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        map[string]string{"cost-center": "1234"},
	})
	if err != nil {
		t.Fatalf("TagResource: %v", err)
	}

	tagsList, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: aws.String(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource: %v", err)
	}

	if tagsList.Tags["cost-center"] != "1234" || tagsList.Tags["env"] != "staging" {
		t.Fatalf("unexpected tags: %v", tagsList.Tags)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     []string{"cost-center"},
	})
	if err != nil {
		t.Fatalf("UntagResource: %v", err)
	}

	tagsAfterUntag, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: aws.String(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource after untag: %v", err)
	}

	if _, ok := tagsAfterUntag.Tags["cost-center"]; ok {
		t.Fatal("expected cost-center tag to be removed")
	}

	// 8. Resource Groups Tagging API
	tagClient := taggingClient(s, "us-east-1")
	resources, err := tagClient.GetResources(ctx, &rgta.GetResourcesInput{
		ResourceTypeFilters: []string{"access-analyzer:analyzer"},
	})
	if err != nil {
		t.Fatalf("GetResources: %v", err)
	}

	if len(resources.ResourceTagMappingList) != 1 {
		t.Fatalf("want 1 resource tag mapping, got %d", len(resources.ResourceTagMappingList))
	}

	if *resources.ResourceTagMappingList[0].ResourceARN != arn {
		t.Fatalf("want ARN %s, got %s", arn, *resources.ResourceTagMappingList[0].ResourceARN)
	}

	// 9. DeleteAnalyzer
	_, err = c.DeleteAnalyzer(ctx, &sdk.DeleteAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
	})
	if err != nil {
		t.Fatalf("DeleteAnalyzer: %v", err)
	}

	_, err = c.GetAnalyzer(ctx, &sdk.GetAnalyzerInput{
		AnalyzerName: aws.String("primary-analyzer"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestCloudFormationAccessAnalyzer(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	aa := accessAnalyzerClient(s)

	template := `{
		"Resources": {
			"MyAnalyzer": {
				"Type": "AWS::AccessAnalyzer::Analyzer",
				"Properties": {
					"AnalyzerName": "cfn-test-analyzer",
					"Type": "ACCOUNT",
					"AnalyzerConfiguration": {
						"UnusedAccess": {
							"UnusedAccessAge": 180
						}
					},
					"ArchiveRules": [
						{
							"RuleName": "cfn-rule",
							"Filter": [
								{
									"Property": "isPublic",
									"Eq": ["false"]
								}
							]
						}
					],
					"Tags": [
						{"Key": "ManagedBy", "Value": "CloudFormation"}
					]
				}
			}
		},
		"Outputs": {
			"AnalyzerRef": {"Value": {"Ref": "MyAnalyzer"}},
			"AnalyzerArn": {"Value": {"Fn::GetAtt": ["MyAnalyzer", "Arn"]}}
		}
	}`

	stackName := "accessanalyzer-stack"
	_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(template),
	})
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}

	stack := settled(ctx, t, cf, stackName)
	if stack.StackStatus != cfntypes.StackStatusCreateComplete {
		events, _ := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: aws.String(stackName)})
		for _, ev := range events.StackEvents {
			reason := ""
			if ev.ResourceStatusReason != nil {
				reason = *ev.ResourceStatusReason
			}
			t.Logf("Event: %s %s %s: %s", *ev.LogicalResourceId, *ev.ResourceType, ev.ResourceStatus, reason)
		}
		t.Fatalf("want CREATE_COMPLETE, got %s", stack.StackStatus)
	}

	var refVal, arnVal string
	for _, out := range stack.Outputs {
		switch *out.OutputKey {
		case "AnalyzerRef":
			refVal = *out.OutputValue
		case "AnalyzerArn":
			arnVal = *out.OutputValue
		}
	}

	if refVal != "cfn-test-analyzer" {
		t.Fatalf("want Ref to be analyzer name cfn-test-analyzer, got %s", refVal)
	}

	if arnVal == "" {
		t.Fatal("expected non-empty Arn output")
	}

	// Verify analyzer exists via accessanalyzer client
	getOut, err := aa.GetAnalyzer(ctx, &sdk.GetAnalyzerInput{
		AnalyzerName: aws.String("cfn-test-analyzer"),
	})
	if err != nil {
		t.Fatalf("GetAnalyzer via SDK: %v", err)
	}

	if *getOut.Analyzer.Arn != arnVal {
		t.Fatalf("want arn %s, got %s", arnVal, *getOut.Analyzer.Arn)
	}

	if getOut.Analyzer.Tags["ManagedBy"] != "CloudFormation" {
		t.Fatalf("want ManagedBy=CloudFormation tag, got %v", getOut.Analyzer.Tags)
	}

	// Verify archive rule exists
	ruleOut, err := aa.GetArchiveRule(ctx, &sdk.GetArchiveRuleInput{
		AnalyzerName: aws.String("cfn-test-analyzer"),
		RuleName:     aws.String("cfn-rule"),
	})
	if err != nil {
		t.Fatalf("GetArchiveRule via SDK: %v", err)
	}

	if *ruleOut.ArchiveRule.RuleName != "cfn-rule" {
		t.Fatalf("want cfn-rule, got %s", *ruleOut.ArchiveRule.RuleName)
	}

	// DeleteStack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		t.Fatalf("DeleteStack: %v", err)
	}

	wantStatus(ctx, t, cf, *stack.StackId, cfntypes.StackStatusDeleteComplete)

	_, err = aa.GetAnalyzer(ctx, &sdk.GetAnalyzerInput{
		AnalyzerName: aws.String("cfn-test-analyzer"),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

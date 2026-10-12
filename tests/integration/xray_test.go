package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/xray"
	"github.com/aws/aws-sdk-go-v2/service/xray/types"
)

func xrayClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func TestXRaySDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := xrayClient(s, "test", "us-east-1")

	// 1. Group lifecycle
	groupCreated, err := c.CreateGroup(ctx, &sdk.CreateGroupInput{
		GroupName:        aws.String("production-group"),
		FilterExpression: aws.String("service(\"api\")"),
		InsightsConfiguration: &types.InsightsConfiguration{
			InsightsEnabled:      aws.Bool(true),
			NotificationsEnabled: aws.Bool(false),
		},
		Tags: []types.Tag{
			{Key: aws.String("Environment"), Value: aws.String("Production")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	groupARN := groupCreated.Group.GroupARN
	if !strings.HasPrefix(aws.ToString(groupARN), "arn:aws:xray:us-east-1:") {
		t.Fatalf("unexpected group ARN: %s", aws.ToString(groupARN))
	}

	// Duplicate create should fail
	_, err = c.CreateGroup(ctx, &sdk.CreateGroupInput{
		GroupName: aws.String("production-group"),
	})
	if err == nil {
		t.Fatal("expected duplicate CreateGroup to fail")
	}

	// GetGroup by name
	getGroup, err := c.GetGroup(ctx, &sdk.GetGroupInput{
		GroupName: aws.String("production-group"),
	})
	if err != nil || aws.ToString(getGroup.Group.GroupARN) != aws.ToString(groupARN) {
		t.Fatal(getGroup, err)
	}

	// UpdateGroup
	updatedGroup, err := c.UpdateGroup(ctx, &sdk.UpdateGroupInput{
		GroupName:        aws.String("production-group"),
		FilterExpression: aws.String("service(\"api-v2\")"),
	})
	if err != nil || aws.ToString(updatedGroup.Group.FilterExpression) != "service(\"api-v2\")" {
		t.Fatal(updatedGroup, err)
	}

	// GetGroups
	groups, err := c.GetGroups(ctx, &sdk.GetGroupsInput{})
	if err != nil || len(groups.Groups) != 1 {
		t.Fatal(groups, err)
	}

	// 2. Sampling Rule lifecycle
	// Verify built-in Default rule exists
	rules, err := c.GetSamplingRules(ctx, &sdk.GetSamplingRulesInput{})
	if err != nil || len(rules.SamplingRuleRecords) != 1 {
		t.Fatal(rules, err)
	}
	if aws.ToString(rules.SamplingRuleRecords[0].SamplingRule.RuleName) != "Default" ||
		aws.ToInt32(rules.SamplingRuleRecords[0].SamplingRule.Priority) != 10000 {
		t.Fatalf("unexpected default rule: %+v", rules.SamplingRuleRecords[0].SamplingRule)
	}

	// Cannot delete Default rule
	_, err = c.DeleteSamplingRule(ctx, &sdk.DeleteSamplingRuleInput{
		RuleName: aws.String("Default"),
	})
	if err == nil {
		t.Fatal("expected DeleteSamplingRule for Default to fail")
	}

	// Create custom rule
	customRule, err := c.CreateSamplingRule(ctx, &sdk.CreateSamplingRuleInput{
		SamplingRule: &types.SamplingRule{
			RuleName:      aws.String("api-rule"),
			Priority:      aws.Int32(50),
			FixedRate:     0.2,
			ReservoirSize: 10,
			ServiceName:   aws.String("api-service"),
			ServiceType:   aws.String("*"),
			Host:          aws.String("*"),
			HTTPMethod:    aws.String("GET"),
			URLPath:       aws.String("/items"),
			ResourceARN:   aws.String("*"),
			Version:       aws.Int32(1),
		},
		Tags: []types.Tag{
			{Key: aws.String("Service"), Value: aws.String("API")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ruleARN := customRule.SamplingRuleRecord.SamplingRule.RuleARN
	if !strings.HasPrefix(aws.ToString(ruleARN), "arn:aws:xray:us-east-1:") {
		t.Fatalf("unexpected rule ARN: %s", aws.ToString(ruleARN))
	}

	// List rules returns custom rule first (priority 50 < 10000)
	rules, err = c.GetSamplingRules(ctx, &sdk.GetSamplingRulesInput{})
	if err != nil || len(rules.SamplingRuleRecords) != 2 {
		t.Fatal(rules, err)
	}
	if aws.ToString(rules.SamplingRuleRecords[0].SamplingRule.RuleName) != "api-rule" {
		t.Fatalf("expected api-rule first, got %s", aws.ToString(rules.SamplingRuleRecords[0].SamplingRule.RuleName))
	}

	// UpdateSamplingRule
	updatedRule, err := c.UpdateSamplingRule(ctx, &sdk.UpdateSamplingRuleInput{
		SamplingRuleUpdate: &types.SamplingRuleUpdate{
			RuleName:  aws.String("api-rule"),
			FixedRate: aws.Float64(0.5),
		},
	})
	if err != nil || updatedRule.SamplingRuleRecord.SamplingRule.FixedRate != 0.5 {
		t.Fatal(updatedRule, err)
	}

	// 3. Tagging
	// TagResource on Group
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceARN: groupARN,
		Tags: []types.Tag{
			{Key: aws.String("Owner"), Value: aws.String("Observability")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceARN: groupARN,
	})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	// UntagResource
	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceARN: groupARN,
		TagKeys:     []string{"Environment"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceARN: groupARN,
	})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "Owner" {
		t.Fatal(tags, err)
	}

	// Tagging via Resource Groups Tagging API
	taggingClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := taggingClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(groupARN), aws.ToString(ruleARN)},
			Tags:            map[string]string{"rgta": "works"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	ruleTags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceARN: ruleARN,
	})
	if err != nil || len(ruleTags.Tags) != 2 {
		t.Fatal(ruleTags, err)
	}

	// 4. Clean up / Delete
	_, err = c.DeleteGroup(ctx, &sdk.DeleteGroupInput{
		GroupARN: groupARN,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteSamplingRule(ctx, &sdk.DeleteSamplingRuleInput{
		RuleARN: ruleARN,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Post delete checks
	_, err = c.GetGroup(ctx, &sdk.GetGroupInput{
		GroupARN: groupARN,
	})
	if err == nil {
		t.Fatal("expected GetGroup to fail after delete")
	}

	rules, err = c.GetSamplingRules(ctx, &sdk.GetSamplingRulesInput{})
	if err != nil || len(rules.SamplingRuleRecords) != 1 {
		t.Fatal(rules, err)
	}
}

package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func costExplorerClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantCostExplorerError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

//nolint:staticcheck // Exercise the supported deprecated Threshold input and migration.
func TestCostExplorerLifecycle(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := costExplorerClient(srv, "test", "us-east-1")
	otherRegion := costExplorerClient(srv, "test", "eu-west-1")
	otherAccount := costExplorerClient(srv, "111111111111", "us-east-1")
	monitor, err := c.CreateAnomalyMonitor(ctx, &sdk.CreateAnomalyMonitorInput{
		AnomalyMonitor: &types.AnomalyMonitor{
			MonitorName:      new("services"),
			MonitorType:      types.MonitorTypeDimensional,
			MonitorDimension: types.MonitorDimensionService,
		},
		ResourceTags: []types.ResourceTag{{Key: new("team"), Value: new("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(*monitor.MonitorArn, "arn:aws:ce::000000000000:anomalymonitor/") {
		t.Fatal(monitor)
	}

	custom, err := c.CreateAnomalyMonitor(ctx, &sdk.CreateAnomalyMonitorInput{
		AnomalyMonitor: &types.AnomalyMonitor{
			MonitorName: new("tags"), MonitorType: types.MonitorTypeCustom,
			MonitorSpecification: &types.Expression{Tags: &types.TagValues{Key: new("team"), Values: []string{"core"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := otherRegion.GetAnomalyMonitors(
		ctx,
		&sdk.GetAnomalyMonitorsInput{MonitorArnList: []string{*monitor.MonitorArn}},
	)
	if err != nil || len(got.AnomalyMonitors) != 1 || got.AnomalyMonitors[0].LastEvaluatedDate != nil {
		t.Fatal(got, err)
	}

	isolated, err := otherAccount.GetAnomalyMonitors(ctx, &sdk.GetAnomalyMonitorsInput{})
	if err != nil || len(isolated.AnomalyMonitors) != 0 {
		t.Fatal(isolated, err)
	}

	page, err := c.GetAnomalyMonitors(ctx, &sdk.GetAnomalyMonitorsInput{MaxResults: new(int32(1))})
	if err != nil || len(page.AnomalyMonitors) != 1 || page.NextPageToken == nil {
		t.Fatal(page, err)
	}

	second, err := c.GetAnomalyMonitors(
		ctx,
		&sdk.GetAnomalyMonitorsInput{MaxResults: new(int32(1)), NextPageToken: page.NextPageToken},
	)
	if err != nil || len(second.AnomalyMonitors) != 1 ||
		*second.AnomalyMonitors[0].MonitorArn == *page.AnomalyMonitors[0].MonitorArn {
		t.Fatal(second, err)
	}

	_, err = c.UpdateAnomalyMonitor(
		ctx,
		&sdk.UpdateAnomalyMonitorInput{MonitorArn: monitor.MonitorArn, MonitorName: new("renamed")},
	)
	if err != nil {
		t.Fatal(err)
	}

	sub, err := c.CreateAnomalySubscription(ctx, &sdk.CreateAnomalySubscriptionInput{
		AnomalySubscription: &types.AnomalySubscription{
			SubscriptionName: new("daily"),
			Frequency:        types.AnomalySubscriptionFrequencyDaily,
			MonitorArnList: []string{
				*monitor.MonitorArn,
			},
			Threshold:   new(float64(10)),
			Subscribers: []types.Subscriber{{Address: new("test@example.com"), Type: types.SubscriberTypeEmail}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	expression := &types.Expression{
		Dimensions: &types.DimensionValues{
			Key:          types.DimensionAnomalyTotalImpactAbsolute,
			Values:       []string{"20"},
			MatchOptions: []types.MatchOption{types.MatchOptionGreaterThanOrEqual},
		},
	}

	_, err = c.UpdateAnomalySubscription(
		ctx,
		&sdk.UpdateAnomalySubscriptionInput{
			SubscriptionArn:     sub.SubscriptionArn,
			ThresholdExpression: expression,
			SubscriptionName:    new("updated"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	subs, err := otherRegion.GetAnomalySubscriptions(
		ctx,
		&sdk.GetAnomalySubscriptionsInput{MonitorArn: monitor.MonitorArn},
	)
	if err != nil || len(subs.AnomalySubscriptions) != 1 || subs.AnomalySubscriptions[0].Threshold != nil ||
		subs.AnomalySubscriptions[0].ThresholdExpression == nil {
		t.Fatal(subs, err)
	}

	category, err := c.CreateCostCategoryDefinition(ctx, &sdk.CreateCostCategoryDefinitionInput{
		Name:        new("Team"),
		RuleVersion: types.CostCategoryRuleVersionCostCategoryExpressionV1,
		Rules: []types.CostCategoryRule{
			{
				Value: new("Core"),
				Rule:  &types.Expression{Tags: &types.TagValues{Key: new("team"), Values: []string{"core"}}},
			},
		},
		DefaultValue: new("Other"),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := otherRegion.DescribeCostCategoryDefinition(
		ctx,
		&sdk.DescribeCostCategoryDefinitionInput{CostCategoryArn: category.CostCategoryArn},
	)
	if err != nil || len(described.CostCategory.Rules) != 1 || *described.CostCategory.DefaultValue != "Other" ||
		len(described.CostCategory.ProcessingStatus) != 0 {
		t.Fatal(described, err)
	}

	_, err = c.UpdateCostCategoryDefinition(ctx, &sdk.UpdateCostCategoryDefinitionInput{
		CostCategoryArn: category.CostCategoryArn,
		RuleVersion:     types.CostCategoryRuleVersionCostCategoryExpressionV1,
		Rules: []types.CostCategoryRule{
			{
				Value: new("Platform"),
				Rule: &types.Expression{
					Dimensions: &types.DimensionValues{Key: types.DimensionRegion, Values: []string{"us-east-1"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	categories, err := c.ListCostCategoryDefinitions(ctx, &sdk.ListCostCategoryDefinitionsInput{})
	if err != nil || len(categories.CostCategoryReferences) != 1 ||
		categories.CostCategoryReferences[0].NumberOfRules != 1 ||
		categories.CostCategoryReferences[0].DefaultValue == nil ||
		*categories.CostCategoryReferences[0].DefaultValue != "Other" {
		t.Fatal(categories, err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{*monitor.MonitorArn, *sub.SubscriptionArn, *category.CostCategoryArn},
			Tags:            map[string]string{"inventory": "yes"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	resources, err := tagging.GetResources(ctx, &rgta.GetResourcesInput{ResourceTypeFilters: []string{"ce"}})
	if err != nil || len(resources.ResourceTagMappingList) != 3 {
		t.Fatal(resources, err)
	}

	untagged, err := tagging.UntagResources(
		ctx,
		&rgta.UntagResourcesInput{ResourceARNList: []string{*monitor.MonitorArn}, TagKeys: []string{"inventory"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatal(untagged, err)
	}

	for _, arn := range []*string{monitor.MonitorArn, sub.SubscriptionArn, category.CostCategoryArn} {
		_, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{
				ResourceArn:  arn,
				ResourceTags: []types.ResourceTag{{Key: new("owner"), Value: new("local")}},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := otherRegion.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		if e != nil || len(tags.ResourceTags) == 0 {
			t.Fatal(tags, e)
		}

		_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, ResourceTagKeys: []string{"owner"}})
		if e != nil {
			t.Fatal(e)
		}

		_, e = otherAccount.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		wantCostExplorerError(t, e, "ResourceNotFoundException")
	}

	_, err = c.DeleteAnomalySubscription(ctx, &sdk.DeleteAnomalySubscriptionInput{SubscriptionArn: sub.SubscriptionArn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteAnomalySubscription(ctx, &sdk.DeleteAnomalySubscriptionInput{SubscriptionArn: sub.SubscriptionArn})
	wantCostExplorerError(t, err, "UnknownSubscriptionException")
	_, err = c.DeleteCostCategoryDefinition(
		ctx,
		&sdk.DeleteCostCategoryDefinitionInput{CostCategoryArn: category.CostCategoryArn},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, arn := range []*string{monitor.MonitorArn, custom.MonitorArn} {
		_, err = c.DeleteAnomalyMonitor(ctx, &sdk.DeleteAnomalyMonitorInput{MonitorArn: arn})
		if err != nil {
			t.Fatal(err)
		}
	}
}

//nolint:staticcheck // Verify that deprecated Threshold cannot coexist with ThresholdExpression.
func TestCostExplorerValidationAndUnsupported(t *testing.T) {
	c := costExplorerClient(server(t), "test", "us-west-2")
	ctx := context.Background()
	_, err := c.CreateAnomalyMonitor(
		ctx,
		&sdk.CreateAnomalyMonitorInput{
			AnomalyMonitor: &types.AnomalyMonitor{
				MonitorName:      new("bad"),
				MonitorType:      types.MonitorTypeCustom,
				MonitorDimension: types.MonitorDimensionService,
			},
		},
	)
	wantCostExplorerError(t, err, "ValidationException")
	_, err = c.CreateAnomalyMonitor(
		ctx,
		&sdk.CreateAnomalyMonitorInput{
			AnomalyMonitor: &types.AnomalyMonitor{
				MonitorName:       new("bad"),
				MonitorType:       types.MonitorTypeDimensional,
				MonitorDimension:  types.MonitorDimensionService,
				LastEvaluatedDate: new("2026-10-01"),
			},
		},
	)
	wantCostExplorerError(t, err, "ValidationException")
	_, err = c.CreateAnomalySubscription(
		ctx,
		&sdk.CreateAnomalySubscriptionInput{AnomalySubscription: &types.AnomalySubscription{
			SubscriptionName: new("bad"),
			Frequency:        types.AnomalySubscriptionFrequencyDaily,
			MonitorArnList: []string{
				"missing",
			},
			Subscribers: []types.Subscriber{
				{Type: types.SubscriberTypeEmail, Address: new("test@example.com")},
			},
			Threshold:           new(float64(1)),
			ThresholdExpression: &types.Expression{},
		}},
	)
	wantCostExplorerError(t, err, "ValidationException")
	_, err = c.GetAnomalyMonitors(ctx, &sdk.GetAnomalyMonitorsInput{NextPageToken: new("%invalid")})
	wantCostExplorerError(t, err, "InvalidNextTokenException")
	_, err = c.GetCostForecast(
		ctx,
		&sdk.GetCostForecastInput{
			TimePeriod:  &types.DateInterval{Start: new("2026-10-01"), End: new("2026-10-02")},
			Granularity: types.GranularityDaily,
			Metric:      "UNBLENDED_COST",
		},
	)
	wantCostExplorerError(t, err, "UnknownOperationException")
}

func TestCostExplorerSQLiteRestartAndReset(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	srv, stop := persistent(t, path, &fakeRuntime{})
	c := costExplorerClient(srv, "test", "us-west-2")
	m, err := c.CreateAnomalyMonitor(
		ctx,
		&sdk.CreateAnomalyMonitorInput{
			AnomalyMonitor: &types.AnomalyMonitor{
				MonitorName:      new("persistent"),
				MonitorType:      types.MonitorTypeDimensional,
				MonitorDimension: types.MonitorDimensionService,
			},
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	sub, err := c.CreateAnomalySubscription(
		ctx,
		&sdk.CreateAnomalySubscriptionInput{AnomalySubscription: &types.AnomalySubscription{
			SubscriptionName: new(
				"persistent",
			),
			Frequency:      types.AnomalySubscriptionFrequencyImmediate,
			MonitorArnList: []string{*m.MonitorArn},
			Subscribers: []types.Subscriber{
				{Type: types.SubscriberTypeSns, Address: new("arn:aws:sns:us-east-1:000000000000:alerts")},
			},
			ThresholdExpression: &types.Expression{
				Dimensions: &types.DimensionValues{
					Key:          types.DimensionAnomalyTotalImpactAbsolute,
					Values:       []string{"10"},
					MatchOptions: []types.MatchOption{types.MatchOptionGreaterThanOrEqual},
				},
			},
		}},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	category, err := c.CreateCostCategoryDefinition(ctx, &sdk.CreateCostCategoryDefinitionInput{
		Name:        new("Persistent"),
		RuleVersion: types.CostCategoryRuleVersionCostCategoryExpressionV1,
		Rules: []types.CostCategoryRule{
			{
				Type: types.CostCategoryRuleTypeInheritedValue,
				InheritedValue: &types.CostCategoryInheritedValueDimension{
					DimensionName: types.CostCategoryInheritedValueDimensionNameTag,
					DimensionKey:  new("team"),
				},
			},
		},
	})
	stop()
	if err != nil {
		t.Fatal(err)
	}

	srv, stop = persistent(t, path, &fakeRuntime{})
	t.Cleanup(stop)
	c = costExplorerClient(srv, "test", "eu-west-1")
	monitors, err := c.GetAnomalyMonitors(ctx, &sdk.GetAnomalyMonitorsInput{MonitorArnList: []string{*m.MonitorArn}})
	if err != nil || len(monitors.AnomalyMonitors) != 1 {
		t.Fatal(monitors, err)
	}

	subs, err := c.GetAnomalySubscriptions(
		ctx,
		&sdk.GetAnomalySubscriptionsInput{SubscriptionArnList: []string{*sub.SubscriptionArn}},
	)
	if err != nil || len(subs.AnomalySubscriptions) != 1 {
		t.Fatal(subs, err)
	}

	got, err := c.DescribeCostCategoryDefinition(
		ctx,
		&sdk.DescribeCostCategoryDefinitionInput{CostCategoryArn: category.CostCategoryArn},
	)
	if err != nil || got.CostCategory.Rules[0].InheritedValue == nil {
		t.Fatal(got, err)
	}

	response, err := srv.Client().Post(srv.URL+"/_tinystack/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}

	monitors, err = c.GetAnomalyMonitors(ctx, &sdk.GetAnomalyMonitorsInput{})
	if err != nil || len(monitors.AnomalyMonitors) != 0 {
		t.Fatal(monitors, err)
	}

	subs, err = c.GetAnomalySubscriptions(ctx, &sdk.GetAnomalySubscriptionsInput{})
	if err != nil || len(subs.AnomalySubscriptions) != 0 {
		t.Fatal(subs, err)
	}

	_, err = c.DescribeCostCategoryDefinition(
		ctx,
		&sdk.DescribeCostCategoryDefinitionInput{CostCategoryArn: category.CostCategoryArn},
	)
	wantCostExplorerError(t, err, "ResourceNotFoundException")
}

package integration_test

import (
	"context"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBControlPlane(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()

	limits, err := c.DescribeLimits(ctx, &sdk.DescribeLimitsInput{})
	if err != nil || *limits.TableMaxReadCapacityUnits != 40000 || *limits.AccountMaxWriteCapacityUnits != 80000 {
		t.Fatalf("provisioned limits: %+v %v", limits, err)
	}

	endpoints, err := c.DescribeEndpoints(ctx, &sdk.DescribeEndpointsInput{})
	if err != nil || len(endpoints.Endpoints) != 1 || *endpoints.Endpoints[0].Address != s.URL {
		t.Fatalf("endpoints: %+v %v", endpoints, err)
	}

	const name = "insights-table"
	if _, err = create(ctx, c, name); err != nil {
		t.Fatal(err)
	}

	updated, err := c.UpdateContributorInsights(ctx, &sdk.UpdateContributorInsightsInput{
		TableName: new(name), ContributorInsightsAction: types.ContributorInsightsActionEnable,
		ContributorInsightsMode: types.ContributorInsightsModeThrottledKeys,
	})
	if err != nil || updated.ContributorInsightsStatus != types.ContributorInsightsStatusEnabled {
		t.Fatalf("enable contributor insights: %+v %v", updated, err)
	}

	described, err := c.DescribeContributorInsights(ctx, &sdk.DescribeContributorInsightsInput{TableName: new(name)})
	if err != nil || described.ContributorInsightsMode != types.ContributorInsightsModeThrottledKeys ||
		described.LastUpdateDateTime == nil {
		t.Fatalf("describe contributor insights: %+v %v", described, err)
	}

	listed, err := c.ListContributorInsights(ctx, &sdk.ListContributorInsightsInput{})
	if err != nil || len(listed.ContributorInsightsSummaries) != 1 ||
		listed.ContributorInsightsSummaries[0].ContributorInsightsStatus != types.ContributorInsightsStatusEnabled {
		t.Fatalf("list contributor insights: %+v %v", listed, err)
	}
}

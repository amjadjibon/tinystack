package dynamodb

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	accountCapacityLimit = int64(80000)
	tableCapacityLimit   = int64(40000)
	insightsAccessed     = "ACCESSED_AND_THROTTLED_KEYS"
	insightsThrottled    = "THROTTLED_KEYS"
	maxInsightsPage      = 100
)

// DescribeLimits returns the default provisioned capacity quotas.
func (s *Service) DescribeLimits(context.Context, *api.DescribeLimitsInput) (*api.DescribeLimitsOutput, error) {
	return &api.DescribeLimitsOutput{
		AccountMaxReadCapacityUnits: new(accountCapacityLimit), AccountMaxWriteCapacityUnits: new(accountCapacityLimit),
		TableMaxReadCapacityUnits: new(tableCapacityLimit), TableMaxWriteCapacityUnits: new(tableCapacityLimit),
	}, nil
}

// DescribeEndpoints advertises the local endpoint so discovery stays on TinyStack.
func (s *Service) DescribeEndpoints(
	ctx context.Context,
	_ *api.DescribeEndpointsRequest,
) (*api.DescribeEndpointsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if m.Endpoint == "" {
		return nil, awserror.New("InvalidEndpointException", "No local DynamoDB endpoint is available")
	}

	return &api.DescribeEndpointsResponse{
		Endpoints: []api.Endpoint{{Address: new(m.Endpoint), CachePeriodInMinutes: new(int64(60))}},
	}, nil
}

type contributorInsights struct {
	Status string
	Mode   string
	Update float64
}

func (t *table) insightsStatus(index string) contributorInsights {
	insight, found := t.Insights[index]
	if !found {
		return contributorInsights{Status: statusDisabled, Mode: insightsAccessed}
	}

	return insight
}

func (t *table) globalIndexNames() []string {
	names := make([]string, 0, len(t.Description.GlobalSecondaryIndexes))
	for _, index := range t.Description.GlobalSecondaryIndexes {
		names = append(names, common.Value(index.IndexName))
	}

	return names
}

func checkInsightsIndex(t *table, index *string) error {
	if index != nil && !slices.Contains(t.globalIndexNames(), *index) {
		return awserror.New("ResourceNotFoundException", "Requested global secondary index not found")
	}

	return nil
}

func (s *Service) DescribeContributorInsights(
	ctx context.Context,
	in *api.DescribeContributorInsightsInput,
) (*api.DescribeContributorInsightsOutput, error) {
	key, err := tableNamed(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeContributorInsightsOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if indexErr := checkInsightsIndex(t, in.IndexName); indexErr != nil {
			return indexErr
		}

		insight := t.insightsStatus(common.Value(in.IndexName))
		out.TableName, out.IndexName = t.Description.TableName, in.IndexName
		out.ContributorInsightsStatus, out.ContributorInsightsMode = new(insight.Status), new(insight.Mode)
		if insight.Update != 0 {
			out.LastUpdateDateTime = new(insight.Update)
		}

		return nil
	})
	return out, err
}

func (s *Service) UpdateContributorInsights(
	ctx context.Context,
	in *api.UpdateContributorInsightsInput,
) (*api.UpdateContributorInsightsOutput, error) {
	key, err := tableNamed(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	status := statusEnabled
	switch common.Value(in.ContributorInsightsAction) {
	case "ENABLE":
	case "DISABLE":
		status = statusDisabled
	default:
		return nil, awserror.Validation("ContributorInsightsAction must be ENABLE or DISABLE")
	}

	if in.ContributorInsightsMode != nil &&
		(*in.ContributorInsightsMode != insightsAccessed && *in.ContributorInsightsMode != insightsThrottled) {
		return nil, awserror.Validation("ContributorInsightsMode must be ACCESSED_AND_THROTTLED_KEYS or THROTTLED_KEYS")
	}

	out := &api.UpdateContributorInsightsOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if indexErr := checkInsightsIndex(t, in.IndexName); indexErr != nil {
			return indexErr
		}

		index := common.Value(in.IndexName)
		insight := t.insightsStatus(index)
		if in.ContributorInsightsMode != nil {
			insight.Mode = *in.ContributorInsightsMode
		}

		insight.Status, insight.Update = status, common.Unix(s.now())
		if t.Insights == nil {
			t.Insights = map[string]contributorInsights{}
		}

		t.Insights[index] = insight
		out.TableName, out.IndexName = t.Description.TableName, in.IndexName
		out.ContributorInsightsStatus, out.ContributorInsightsMode = new(insight.Status), new(insight.Mode)
		return save(tx, key, t)
	})
	return out, err
}

func (s *Service) ListContributorInsights(
	ctx context.Context,
	in *api.ListContributorInsightsInput,
) (*api.ListContributorInsightsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := scope.AccountID + "/" + scope.Region + "/dynamodb/"
	var key string
	if in.TableName != nil {
		key, err = tableNamed(ctx, in.TableName)
		if err != nil {
			return nil, err
		}
	}

	limit := common.Value(in.MaxResults)
	if limit < 0 || limit > maxInsightsPage {
		return nil, awserror.Validation("MaxResults must be from 0 to %d", maxInsightsPage)
	}

	if limit == 0 {
		limit = maxInsightsPage
	}

	summaries := map[string]api.ContributorInsightsSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if key != "" {
			return listInsightsFor(tx, key, summaries)
		}

		for _, record := range tx.Keys(prefix) {
			if !strings.Contains(record[len(prefix):], "/") {
				if listErr := listInsightsFor(tx, record, summaries); listErr != nil {
					return listErr
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	keys := slices.Sorted(maps.Keys(summaries))
	page, next, err := common.Page(keys, func(key string) string { return key }, in.NextToken, int(limit),
		awserror.Validation("Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	out := &api.ListContributorInsightsOutput{
		NextToken: next, ContributorInsightsSummaries: make([]api.ContributorInsightsSummary, 0, len(page)),
	}
	for _, key := range page {
		out.ContributorInsightsSummaries = append(out.ContributorInsightsSummaries, summaries[key])
	}

	return out, nil
}

func listInsightsFor(tx storage.ReadTx, key string, summaries map[string]api.ContributorInsightsSummary) error {
	t, err := load(tx, key)
	if err != nil {
		return err
	}

	for _, index := range append(t.globalIndexNames(), "") {
		insight := t.insightsStatus(index)
		summary := api.ContributorInsightsSummary{
			TableName: t.Description.TableName, ContributorInsightsStatus: new(insight.Status),
			ContributorInsightsMode: new(insight.Mode),
		}
		if index != "" {
			summary.IndexName = new(index)
		}

		summaries[key+"/"+index] = summary
	}

	return nil
}

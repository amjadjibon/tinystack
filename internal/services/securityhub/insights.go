package securityhub

import (
	"context"
	"slices"
	"sort"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkGroup(group *string) error {
	if group != nil && !slices.Contains(stringFields, *group) {
		return invalid("Unsupported GroupByAttribute.")
	}

	return nil
}

func (s *Service) CreateInsight(ctx context.Context, in *api.CreateInsightRequest) (*api.CreateInsightResponse, error) {
	out := &api.CreateInsightResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		if err := checkFilters(in.Filters); err != nil {
			return err
		}

		if err := checkGroup(in.GroupByAttribute); err != nil {
			return err
		}

		out.InsightArn = new(arn(scope, "insight/"+scope.AccountID+"/custom/"+common.RandomID(32)))
		return storage.Write(
			tx,
			scope.Key(serviceName, "insight", *out.InsightArn),
			api.Insight{
				InsightArn:       out.InsightArn,
				Name:             in.Name,
				Filters:          in.Filters,
				GroupByAttribute: in.GroupByAttribute,
			},
		)
	})

	return out, err
}

func (s *Service) GetInsights(ctx context.Context, in *api.GetInsightsRequest) (*api.GetInsightsResponse, error) {
	out := &api.GetInsightsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		records, err := storage.Records[api.Insight](tx, scope.Prefix(serviceName, "insight"))
		if err != nil {
			return err
		}

		items := []api.Insight{}
		for _, r := range records {
			if len(in.InsightArns) == 0 || slices.Contains(in.InsightArns, common.Value(r.InsightArn)) {
				items = append(items, *r)
			}
		}

		out.Insights, out.NextToken, err = page(
			items,
			func(v api.Insight) string { return common.Value(v.InsightArn) },
			in.NextToken,
			in.MaxResults,
		)
		return err
	})

	return out, err
}

func (s *Service) UpdateInsight(ctx context.Context, in *api.UpdateInsightRequest) (*api.UpdateInsightResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		if err := checkFilters(in.Filters); err != nil {
			return err
		}

		if err := checkGroup(in.GroupByAttribute); err != nil {
			return err
		}

		k := scope.Key(serviceName, "insight", common.Value(in.InsightArn))
		r, found, err := storage.Read[api.Insight](tx, k)
		if err != nil {
			return err
		}

		if !found {
			return missing()
		}

		if in.Name != nil {
			r.Name = in.Name
		}

		if in.Filters != nil {
			r.Filters = in.Filters
		}

		if in.GroupByAttribute != nil {
			r.GroupByAttribute = in.GroupByAttribute
		}

		return storage.Write(tx, k, r)
	})

	return &api.UpdateInsightResponse{}, err
}

func (s *Service) DeleteInsight(ctx context.Context, in *api.DeleteInsightRequest) (*api.DeleteInsightResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		k := scope.Key(serviceName, "insight", common.Value(in.InsightArn))
		if _, found := tx.Get(k); !found {
			return missing()
		}

		tx.Delete(k)
		return nil
	})

	return &api.DeleteInsightResponse{InsightArn: in.InsightArn}, err
}

func (s *Service) GetInsightResults(
	ctx context.Context,
	in *api.GetInsightResultsRequest,
) (*api.GetInsightResultsResponse, error) {
	out := &api.GetInsightResultsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		insight, found, err := storage.Read[api.Insight](
			tx,
			scope.Key(serviceName, "insight", common.Value(in.InsightArn)),
		)
		if err != nil {
			return err
		}

		if !found {
			return missing()
		}

		records, err := storage.Records[api.AwsSecurityFinding](tx, scope.Prefix(serviceName, "finding"))
		if err != nil {
			return err
		}

		counts := map[string]int64{}
		for _, f := range records {
			if !matches(f, insight.Filters) {
				continue
			}

			seen := map[string]bool{}
			for _, v := range findingValues(f, common.Value(insight.GroupByAttribute)) {
				if !seen[v] {
					counts[v]++
					seen[v] = true
				}
			}
		}

		values := []api.InsightResultValue{}
		for v, n := range counts {
			values = append(values, api.InsightResultValue{GroupByAttributeValue: new(v), Count: new(n)})
		}

		// AWS insight results rank groups by count; ties use the attribute value.
		sort.Slice(values, func(i, j int) bool {
			if *values[i].Count != *values[j].Count {
				return *values[i].Count > *values[j].Count
			}

			return *values[i].GroupByAttributeValue < *values[j].GroupByAttributeValue
		})

		out.InsightResults = &api.InsightResults{
			InsightArn:       in.InsightArn,
			GroupByAttribute: insight.GroupByAttribute,
			ResultValues:     values,
		}

		return nil
	})

	return out, err
}

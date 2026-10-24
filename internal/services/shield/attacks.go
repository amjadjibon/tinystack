package shield

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) DescribeAttack(
	ctx context.Context,
	_ *api.DescribeAttackRequest,
) (*api.DescribeAttackResponse, error) {
	err := s.transaction(
		ctx,
		func(_ storage.WriteTx, _ request.Scope, _ *subscriptionRecord) error { return missing() },
	)
	return nil, err
}

func (s *Service) DescribeAttackStatistics(
	ctx context.Context,
	_ *api.DescribeAttackStatisticsRequest,
) (*api.DescribeAttackStatisticsResponse, error) {
	now := float64(time.Now().Unix())
	out := &api.DescribeAttackStatisticsResponse{
		TimeRange: &api.TimeRange{FromInclusive: new(now - 365*86400), ToExclusive: new(now)},
		DataItems: []api.AttackStatisticsDataItem{},
	}

	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, _ *subscriptionRecord) error { return nil })
	return out, err
}

func (s *Service) ListAttacks(ctx context.Context, in *api.ListAttacksRequest) (*api.ListAttacksResponse, error) {
	for _, r := range []*api.TimeRange{in.StartTime, in.EndTime} {
		if r != nil && r.FromInclusive != nil && r.ToExclusive != nil && *r.FromInclusive >= *r.ToExclusive {
			return nil, invalid("Time range must have FromInclusive before ToExclusive")
		}
	}

	out := &api.ListAttacksResponse{}
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, _ *subscriptionRecord) error {
		var err error
		out.AttackSummaries, out.NextToken, err = common.Page(
			[]api.AttackSummary{},
			func(a api.AttackSummary) string { return common.Value(a.AttackId) },
			in.NextToken,
			pageSize(in.MaxResults),
			invalid("Invalid NextToken"),
		)
		return err
	})
	return out, err
}

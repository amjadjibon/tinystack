package xray

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutTelemetryRecords(
	ctx context.Context,
	in *api.PutTelemetryRecordsRequest,
) (*api.PutTelemetryRecordsResult, error) {
	if _, err := request.RegionalScope(ctx); err != nil {
		return nil, err
	}

	if in == nil || len(in.TelemetryRecords) == 0 || len(in.TelemetryRecords) > 100 {
		return nil, invalid("TelemetryRecords must contain 1-100 records")
	}

	for _, r := range in.TelemetryRecords {
		if r.Timestamp == nil || *r.Timestamp <= 0 {
			return nil, invalid("Timestamp required")
		}

		counts := []*int64{
			r.SegmentsReceivedCount,
			r.SegmentsSentCount,
			r.SegmentsRejectedCount,
			r.SegmentsSpilloverCount,
		}

		if b := r.BackendConnectionErrors; b != nil {
			counts = append(
				counts,
				b.TimeoutCount,
				b.ConnectionRefusedCount,
				b.HTTPCode4XXCount,
				b.HTTPCode5XXCount,
				b.UnknownHostCount,
				b.OtherCount,
			)
		}

		for _, n := range counts {
			if n != nil && *n < 0 {
				return nil, invalid("telemetry counts must be nonnegative")
			}
		}
	}

	return &api.PutTelemetryRecordsResult{}, nil
}

func (s *Service) GetSamplingTargets(
	ctx context.Context,
	in *api.GetSamplingTargetsRequest,
) (*api.GetSamplingTargetsResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || len(in.SamplingStatisticsDocuments) > 25 {
		return nil, invalid("SamplingStatisticsDocuments must contain 0-25 records")
	}

	out := &api.GetSamplingTargetsResult{
		SamplingTargetDocuments: []api.SamplingTargetDocument{},
		UnprocessedStatistics:   []api.UnprocessedStatistics{},
		LastRuleModification:    new(float64(0)),
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rules, e := storage.Records[samplingRuleRecord](tx, scope.Prefix(serviceName, samplingRuleKind))
		if e != nil {
			return e
		}

		for _, r := range rules {
			if m := common.Value(r.Record.ModifiedAt); m > *out.LastRuleModification {
				*out.LastRuleModification = m
			}
		}

		for _, d := range in.SamplingStatisticsDocuments {
			if common.Value(d.RuleName) == "" || !clientIDPattern.MatchString(common.Value(d.ClientID)) ||
				d.Timestamp == nil ||
				*d.Timestamp <= 0 ||
				d.RequestCount == nil ||
				d.SampledCount == nil ||
				*d.RequestCount < 0 ||
				*d.SampledCount < 0 ||
				common.Value(d.BorrowCount) < 0 {
				return invalid("invalid sampling statistics document")
			}

			rec, _, _, e := findSamplingRule(tx, scope, d.RuleName, nil)
			if e != nil {
				out.UnprocessedStatistics = append(
					out.UnprocessedStatistics,
					api.UnprocessedStatistics{
						RuleName:  d.RuleName,
						ErrorCode: new("RuleNotFound"),
						Message:   new("Sampling rule not found"),
					},
				)
				continue
			}

			rule := rec.Record.SamplingRule
			out.SamplingTargetDocuments = append(
				out.SamplingTargetDocuments,
				api.SamplingTargetDocument{
					RuleName:          rule.RuleName,
					FixedRate:         rule.FixedRate,
					ReservoirQuota:    rule.ReservoirSize,
					ReservoirQuotaTTL: new(common.Unix(time.Now().Add(10 * time.Second))),
					Interval:          new(int64(10)),
				},
			)
		}

		return nil
	})
	return out, err
}

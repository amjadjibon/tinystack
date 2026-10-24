package swf

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type executionFilter struct {
	Domain    *string
	Start     *api.ExecutionTimeFilter
	Close     *api.ExecutionTimeFilter
	Type      *api.WorkflowTypeFilter
	Tag       *api.TagFilter
	Execution *api.WorkflowExecutionFilter
	Status    *api.CloseStatusFilter
}

func (s *Service) executions(ctx context.Context, f executionFilter, closed bool) ([]api.WorkflowExecutionInfo, error) {
	filters := 0
	for _, set := range []bool{f.Type != nil, f.Tag != nil, f.Execution != nil, f.Status != nil} {
		if set {
			filters++
		}
	}

	if filters > 1 {
		return nil, invalid("Only one of type, tag, execution or close status filter is allowed")
	}

	if (f.Start == nil && f.Close == nil) || (f.Start != nil && f.Close != nil) {
		return nil, invalid("Specify exactly one startTimeFilter or closeTimeFilter")
	}

	for _, t := range []*api.ExecutionTimeFilter{f.Start, f.Close} {
		if t != nil && t.LatestDate != nil && common.Value(t.OldestDate) > *t.LatestDate {
			return nil, invalid("oldestDate must not exceed latestDate")
		}
	}

	out := []api.WorkflowExecutionInfo{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(f.Domain), false); err != nil {
			return err
		}

		var rs []*executionRecord
		if f.Close != nil {
			keys, err := storage.Records[string](tx, scope.Prefix(serviceName, "closed")+common.Value(f.Domain)+"/")
			if err != nil {
				return err
			}

			for _, key := range keys {
				r, ok, err := storage.Read[executionRecord](tx, *key)
				if err != nil {
					return err
				}

				if ok {
					rs = append(rs, r)
				}
			}
		} else {
			var err error
			rs, err = storage.Records[executionRecord](tx, executionPrefix(scope, common.Value(f.Domain)))
			if err != nil {
				return err
			}
		}

		for _, r := range rs {
			info := r.Detail.ExecutionInfo
			if isOpen(r) == closed {
				continue
			}

			t, filter := common.Value(info.StartTimestamp), f.Start
			if f.Close != nil {
				t = common.Value(info.CloseTimestamp)
				filter = f.Close
			}

			if t < common.Value(filter.OldestDate) || (filter.LatestDate != nil && t > *filter.LatestDate) {
				continue
			}

			if f.Type != nil &&
				(common.Value(f.Type.Name) != common.Value(info.WorkflowType.Name) ||
					(f.Type.Version != nil && *f.Type.Version != common.Value(info.WorkflowType.Version))) {
				continue
			}

			if f.Tag != nil && !slices.Contains(info.TagList, common.Value(f.Tag.Tag)) {
				continue
			}

			if f.Execution != nil && common.Value(f.Execution.WorkflowId) != common.Value(info.Execution.WorkflowId) {
				continue
			}

			if f.Status != nil && common.Value(f.Status.Status) != common.Value(info.CloseStatus) {
				continue
			}

			out = append(out, *info)
		}

		return nil
	})
	return out, err
}

func (s *Service) ListOpenWorkflowExecutions(
	ctx context.Context,
	in *api.ListOpenWorkflowExecutionsInput,
) (*api.WorkflowExecutionInfos, error) {
	rs, err := s.executions(
		ctx,
		executionFilter{
			Domain:    in.Domain,
			Start:     in.StartTimeFilter,
			Type:      in.TypeFilter,
			Tag:       in.TagFilter,
			Execution: in.ExecutionFilter,
		},
		false,
	)
	if err != nil {
		return nil, err
	}

	if !common.Value(in.ReverseOrder) {
		slices.Reverse(rs)
	}

	items, next, err := common.Page(
		rs,
		func(i api.WorkflowExecutionInfo) string { return common.Value(i.Execution.RunId) },
		in.NextPageToken,
		limit(in.MaximumPageSize),
		invalid("Invalid nextPageToken"),
	)
	return &api.WorkflowExecutionInfos{ExecutionInfos: items, NextPageToken: next}, err
}

func (s *Service) CountOpenWorkflowExecutions(
	ctx context.Context,
	in *api.CountOpenWorkflowExecutionsInput,
) (*api.WorkflowExecutionCount, error) {
	rs, err := s.executions(
		ctx,
		executionFilter{
			Domain:    in.Domain,
			Start:     in.StartTimeFilter,
			Type:      in.TypeFilter,
			Tag:       in.TagFilter,
			Execution: in.ExecutionFilter,
		},
		false,
	)
	return &api.WorkflowExecutionCount{Count: new(int64(len(rs))), Truncated: new(false)}, err
}

func (s *Service) ListClosedWorkflowExecutions(
	ctx context.Context,
	in *api.ListClosedWorkflowExecutionsInput,
) (*api.WorkflowExecutionInfos, error) {
	rs, err := s.executions(
		ctx,
		executionFilter{
			Domain:    in.Domain,
			Start:     in.StartTimeFilter,
			Type:      in.TypeFilter,
			Tag:       in.TagFilter,
			Execution: in.ExecutionFilter,
			Close:     in.CloseTimeFilter,
			Status:    in.CloseStatusFilter,
		},
		true,
	)
	if err != nil {
		return nil, err
	}

	if !common.Value(in.ReverseOrder) {
		slices.Reverse(rs)
	}

	items, next, err := common.Page(
		rs,
		func(i api.WorkflowExecutionInfo) string { return common.Value(i.Execution.RunId) },
		in.NextPageToken,
		limit(in.MaximumPageSize),
		invalid("Invalid nextPageToken"),
	)
	return &api.WorkflowExecutionInfos{ExecutionInfos: items, NextPageToken: next}, err
}

func (s *Service) CountClosedWorkflowExecutions(
	ctx context.Context,
	in *api.CountClosedWorkflowExecutionsInput,
) (*api.WorkflowExecutionCount, error) {
	rs, err := s.executions(
		ctx,
		executionFilter{
			Domain:    in.Domain,
			Start:     in.StartTimeFilter,
			Type:      in.TypeFilter,
			Tag:       in.TagFilter,
			Execution: in.ExecutionFilter,
			Close:     in.CloseTimeFilter,
			Status:    in.CloseStatusFilter,
		},
		true,
	)
	return &api.WorkflowExecutionCount{Count: new(int64(len(rs))), Truncated: new(false)}, err
}

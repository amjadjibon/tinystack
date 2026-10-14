package costexplorer

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateAnomalyMonitor(
	ctx context.Context,
	in *api.CreateAnomalyMonitorRequest,
) (*api.CreateAnomalyMonitorResponse, error) {
	m := in.AnomalyMonitor
	if m == nil {
		return nil, awserror.Validation("AnomalyMonitor is required.")
	}

	if m.MonitorArn != nil || m.CreationDate != nil || m.LastUpdatedDate != nil || m.LastEvaluatedDate != nil ||
		m.DimensionalValueCount != nil {
		return nil, awserror.Validation("Monitor output fields cannot be supplied at creation.")
	}

	if err := validateMonitor(m); err != nil {
		return nil, err
	}

	m = common.Clone(m)
	m.CreationDate, m.LastUpdatedDate = new(today()), new(today())
	r := &record{Monitor: m}
	if err := s.create(ctx, monitorKind, r, in.ResourceTags); err != nil {
		return nil, err
	}

	return &api.CreateAnomalyMonitorResponse{MonitorArn: new(r.ARN)}, nil
}

func (s *Service) GetAnomalyMonitors(
	ctx context.Context,
	in *api.GetAnomalyMonitorsRequest,
) (*api.GetAnomalyMonitorsResponse, error) {
	rs, next, err := s.list(ctx, monitorKind, in.NextPageToken, in.MaxResults, func(r *record) bool {
		return len(in.MonitorArnList) == 0 || slices.Contains(in.MonitorArnList, r.ARN)
	}, in.MonitorArnList)
	if err != nil {
		return nil, err
	}

	out := &api.GetAnomalyMonitorsResponse{AnomalyMonitors: []api.AnomalyMonitor{}, NextPageToken: next}
	for _, r := range rs {
		out.AnomalyMonitors = append(out.AnomalyMonitors, *r.Monitor)
	}

	return out, nil
}

func (s *Service) UpdateAnomalyMonitor(
	ctx context.Context,
	in *api.UpdateAnomalyMonitorRequest,
) (*api.UpdateAnomalyMonitorResponse, error) {
	if in.MonitorName != nil {
		if err := genericName(in.MonitorName); err != nil {
			return nil, err
		}
	}

	err := s.edit(ctx, in.MonitorArn, monitorKind, func(r *record, _ storage.WriteTx) error {
		if in.MonitorName != nil {
			r.Monitor.MonitorName, r.Monitor.LastUpdatedDate = in.MonitorName, new(today())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAnomalyMonitorResponse{MonitorArn: in.MonitorArn}, nil
}

func (s *Service) DeleteAnomalyMonitor(
	ctx context.Context,
	in *api.DeleteAnomalyMonitorRequest,
) (*api.DeleteAnomalyMonitorResponse, error) {
	if err := s.remove(ctx, in.MonitorArn, monitorKind); err != nil {
		return nil, err
	}

	return &api.DeleteAnomalyMonitorResponse{}, nil
}

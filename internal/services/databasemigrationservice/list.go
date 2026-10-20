package databasemigrationservice

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const filterEndpointARN = "endpoint-arn"

func (r *record) filters() map[string]string {
	switch r.Kind {
	case kindSubnet:
		return map[string]string{"replication-subnet-group-id": r.Name}
	case kindInstance:
		return map[string]string{
			"replication-instance-id": r.Name, "replication-instance-arn": r.ARN,
			"replication-instance-class": common.Value(r.Instance.ReplicationInstanceClass),
			"engine-version":             common.Value(r.Instance.EngineVersion),
		}
	case kindEndpoint:
		return map[string]string{
			"endpoint-id": r.Name, filterEndpointARN: r.ARN,
			"endpoint-type": common.Value(r.Endpoint.EndpointType), "engine-name": common.Value(r.Endpoint.EngineName),
		}
	case kindTask:
		return map[string]string{
			"replication-task-id": r.Name, "replication-task-arn": r.ARN,
			"replication-instance-arn": common.Value(r.Task.ReplicationInstanceArn),
			"migration-type":           common.Value(r.Task.MigrationType),
			filterEndpointARN:          common.Value(r.Task.SourceEndpointArn),
		}
	default:
		return nil
	}
}

func (r *record) advance() {
	if r.Instance != nil && common.Value(r.Instance.ReplicationInstanceStatus) != stateAvailable {
		r.Instance.ReplicationInstanceStatus = new(stateAvailable)
	}

	if r.Task != nil {
		switch common.Value(r.Task.Status) {
		case stateCreating:
			r.Task.Status = new(stateReady)
		case stateStarting:
			r.Task.Status = new(stateRunning)
		case stateRunning:
			if common.Value(r.Task.MigrationType) == "full-load" {
				r.Task.Status = new(stateStopped)
				r.Task.StopReason = new("FULL_LOAD_ONLY_FINISHED: simulated records only; no data migrated")
			}
		}
	}
}

func (s *Service) describe(ctx context.Context, kind string, filters []api.Filter,
	marker *string, maxRecords *int64,
) ([]record, *string, error) {
	limit := int64(100)
	if maxRecords != nil {
		limit = *maxRecords
	}

	if limit < 20 || limit > 100 {
		return nil, nil, invalid("MaxRecords must be between 20 and 100.")
	}

	// Validate even when there are no matching resources.
	sample := &record{
		Kind: kind, Instance: &api.ReplicationInstance{}, Endpoint: &api.Endpoint{},
		Task: &api.ReplicationTask{},
	}
	for _, f := range filters {
		if _, ok := sample.filters()[common.Value(f.Name)]; !ok || len(f.Values) == 0 {
			return nil, nil, invalid("Unknown filter name or empty filter values.")
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	var out []record
	var next *string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		selected := make([]record, 0, len(rows))
		for _, r := range rows {
			matches := true
			for _, f := range filters {
				value := r.filters()[common.Value(f.Name)]
				match := slices.Contains(f.Values, value)
				if kind == kindTask && common.Value(f.Name) == filterEndpointARN {
					match = match || slices.Contains(f.Values, common.Value(r.Task.TargetEndpointArn))
				}

				matches = matches && match
			}

			if matches {
				selected = append(selected, *r)
			}
		}

		out, next, e = common.Page(selected, func(r record) string { return r.ARN }, marker, int(limit),
			invalid("Invalid Marker."))
		if e != nil {
			return e
		}

		for _, r := range out {
			// Return the observed state and commit the next state for the next observation.
			r = *common.Clone(new(r))
			r.advance()
			if e := storage.Write(tx, recordKey(scope, new(r)), r); e != nil {
				return e
			}
		}

		return nil
	})
	return out, next, err
}

func (s *Service) DescribeReplicationSubnetGroups(ctx context.Context, in *api.DescribeReplicationSubnetGroupsMessage,
) (*api.DescribeReplicationSubnetGroupsResponse, error) {
	rows, marker, err := s.describe(ctx, kindSubnet, in.Filters, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeReplicationSubnetGroupsResponse{
		Marker:                  marker,
		ReplicationSubnetGroups: []api.ReplicationSubnetGroup{},
	}
	for _, r := range rows {
		out.ReplicationSubnetGroups = append(out.ReplicationSubnetGroups, *r.Subnet)
	}

	return out, nil
}

func (s *Service) DescribeReplicationInstances(ctx context.Context, in *api.DescribeReplicationInstancesMessage,
) (*api.DescribeReplicationInstancesResponse, error) {
	rows, marker, err := s.describe(ctx, kindInstance, in.Filters, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeReplicationInstancesResponse{Marker: marker, ReplicationInstances: []api.ReplicationInstance{}}
	for _, r := range rows {
		out.ReplicationInstances = append(out.ReplicationInstances, *r.Instance)
	}

	return out, nil
}

func (s *Service) DescribeEndpoints(ctx context.Context, in *api.DescribeEndpointsMessage,
) (*api.DescribeEndpointsResponse, error) {
	rows, marker, err := s.describe(ctx, kindEndpoint, in.Filters, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeEndpointsResponse{Marker: marker, Endpoints: []api.Endpoint{}}
	for _, r := range rows {
		out.Endpoints = append(out.Endpoints, *r.Endpoint)
	}

	return out, nil
}

func (s *Service) DescribeReplicationTasks(ctx context.Context, in *api.DescribeReplicationTasksMessage,
) (*api.DescribeReplicationTasksResponse, error) {
	rows, marker, err := s.describe(ctx, kindTask, in.Filters, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeReplicationTasksResponse{Marker: marker, ReplicationTasks: []api.ReplicationTask{}}
	for _, r := range rows {
		if common.Value(in.WithoutSettings) {
			r.Task.ReplicationTaskSettings = nil
		}

		out.ReplicationTasks = append(out.ReplicationTasks, *r.Task)
	}

	return out, nil
}

package redshiftserverless

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) GetNamespace(ctx context.Context, in *api.GetNamespaceRequest) (*api.GetNamespaceResponse, error) {
	r, err := s.get(ctx, kindNamespace, in.NamespaceName)
	if err != nil {
		return nil, err
	}

	return &api.GetNamespaceResponse{Namespace: r.Namespace}, nil
}

func (s *Service) ListNamespaces(
	ctx context.Context,
	in *api.ListNamespacesRequest,
) (*api.ListNamespacesResponse, error) {
	rows, token, err := s.list(ctx, kindNamespace, in.NextToken, in.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	out := &api.ListNamespacesResponse{NextToken: token, Namespaces: []api.Namespace{}}

	for _, r := range rows {
		out.Namespaces = append(out.Namespaces, *r.Namespace)
	}

	return out, nil
}

func (s *Service) GetWorkgroup(ctx context.Context, in *api.GetWorkgroupRequest) (*api.GetWorkgroupResponse, error) {
	r, err := s.get(ctx, kindWorkgroup, in.WorkgroupName)
	if err != nil {
		return nil, err
	}

	return &api.GetWorkgroupResponse{Workgroup: r.Workgroup}, nil
}

func (s *Service) ListWorkgroups(
	ctx context.Context,
	in *api.ListWorkgroupsRequest,
) (*api.ListWorkgroupsResponse, error) {
	rows, token, err := s.list(ctx, kindWorkgroup, in.NextToken, in.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	out := &api.ListWorkgroupsResponse{NextToken: token, Workgroups: []api.Workgroup{}}

	for _, r := range rows {
		out.Workgroups = append(out.Workgroups, *r.Workgroup)
	}

	return out, nil
}

func (s *Service) GetSnapshot(ctx context.Context, in *api.GetSnapshotRequest) (*api.GetSnapshotResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = s.snapshot(tx, scope, in.SnapshotName, in.SnapshotArn)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetSnapshotResponse{Snapshot: r.Snapshot}, nil
}

func (s *Service) ListSnapshots(ctx context.Context, in *api.ListSnapshotsRequest) (*api.ListSnapshotsResponse, error) {
	rows, token, err := s.list(ctx, kindSnapshot, in.NextToken, in.MaxResults, func(r *record) bool {
		return (in.NamespaceName == nil || common.Value(r.Snapshot.NamespaceName) == *in.NamespaceName) &&
			(in.NamespaceArn == nil || common.Value(r.Snapshot.NamespaceArn) == *in.NamespaceArn) &&
			(in.StartTime == nil || float64(r.Snapshot.SnapshotCreateTime.Unix()) >= *in.StartTime) &&
			(in.EndTime == nil || float64(r.Snapshot.SnapshotCreateTime.Unix()) <= *in.EndTime)
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListSnapshotsResponse{NextToken: token, Snapshots: []api.Snapshot{}}

	for _, r := range rows {
		out.Snapshots = append(out.Snapshots, *r.Snapshot)
	}

	return out, nil
}

func (s *Service) DeleteSnapshot(
	ctx context.Context,
	in *api.DeleteSnapshotRequest,
) (*api.DeleteSnapshotResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindSnapshot, common.Value(in.SnapshotName))
		if e != nil {
			return e
		}

		r.Snapshot.Status = new("DELETED")
		tx.Delete(scope.Key(serviceName, kindSnapshot, r.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSnapshotResponse{Snapshot: r.Snapshot}, nil
}

func (s *Service) GetUsageLimit(ctx context.Context, in *api.GetUsageLimitRequest) (*api.GetUsageLimitResponse, error) {
	r, err := s.get(ctx, kindUsageLimit, in.UsageLimitId)
	if err != nil {
		return nil, err
	}

	return &api.GetUsageLimitResponse{UsageLimit: r.UsageLimit}, nil
}

func (s *Service) ListUsageLimits(
	ctx context.Context,
	in *api.ListUsageLimitsRequest,
) (*api.ListUsageLimitsResponse, error) {
	rows, token, err := s.list(ctx, kindUsageLimit, in.NextToken, in.MaxResults, func(r *record) bool {
		return (in.ResourceArn == nil || common.Value(r.UsageLimit.ResourceArn) == *in.ResourceArn) &&
			(in.UsageType == nil || common.Value(r.UsageLimit.UsageType) == *in.UsageType)
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListUsageLimitsResponse{NextToken: token, UsageLimits: []api.UsageLimit{}}

	for _, r := range rows {
		out.UsageLimits = append(out.UsageLimits, *r.UsageLimit)
	}

	return out, nil
}

func (s *Service) DeleteUsageLimit(
	ctx context.Context,
	in *api.DeleteUsageLimitRequest,
) (*api.DeleteUsageLimitResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindUsageLimit, common.Value(in.UsageLimitId))
		if e != nil {
			return e
		}

		tx.Delete(scope.Key(serviceName, kindUsageLimit, r.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUsageLimitResponse{UsageLimit: r.UsageLimit}, nil
}

func (s *Service) GetEndpointAccess(
	ctx context.Context,
	in *api.GetEndpointAccessRequest,
) (*api.GetEndpointAccessResponse, error) {
	r, err := s.get(ctx, kindEndpoint, in.EndpointName)
	if err != nil {
		return nil, err
	}

	return &api.GetEndpointAccessResponse{Endpoint: r.Endpoint}, nil
}

func (s *Service) ListEndpointAccess(
	ctx context.Context,
	in *api.ListEndpointAccessRequest,
) (*api.ListEndpointAccessResponse, error) {
	rows, token, err := s.list(ctx, kindEndpoint, in.NextToken, in.MaxResults, func(r *record) bool {
		return (in.WorkgroupName == nil || common.Value(r.Endpoint.WorkgroupName) == *in.WorkgroupName) &&
			(in.VpcId == nil || common.Value(r.Endpoint.VpcEndpoint.VpcId) == *in.VpcId)
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListEndpointAccessResponse{NextToken: token, Endpoints: []api.EndpointAccess{}}

	for _, r := range rows {
		out.Endpoints = append(out.Endpoints, *r.Endpoint)
	}

	return out, nil
}

func (s *Service) DeleteEndpointAccess(
	ctx context.Context,
	in *api.DeleteEndpointAccessRequest,
) (*api.DeleteEndpointAccessResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindEndpoint, common.Value(in.EndpointName))
		if e != nil {
			return e
		}

		r.Endpoint.EndpointStatus = new(statusDeleting)
		tx.Delete(scope.Key(serviceName, kindEndpoint, r.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteEndpointAccessResponse{Endpoint: r.Endpoint}, nil
}

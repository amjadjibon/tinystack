package rds

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func clusterSnapshot(r *record) *api.DBClusterSnapshot {
	out := common.Clone(r.ClusterSnapshot)
	out.TagList = tagList(r.Tags)
	if !time.Now().Before(r.ReadyAt) {
		out.Status, out.PercentProgress = new(available), new(int64(100))
	}

	return out
}

func (s *Service) createClusterSnapshot(
	ctx context.Context,
	scope request.Scope,
	source *record,
	id string,
	tags []api.Tag,
) (*record, error) {
	if !aurora(common.Value(source.Cluster.Engine)) {
		return nil, invalid("Cluster snapshots require Aurora")
	}

	id = strings.ToLower(id)
	if !validName(id, 255) {
		return nil, invalid("Invalid DBClusterSnapshotIdentifier")
	}

	status := common.Value(cluster(source).Status)
	if status != available && status != stopped {
		return nil, awserror.New("InvalidDBClusterStateFault", "Cluster must be available or stopped")
	}

	parsed, err := parseTags(tags)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, e := storage.Read[record](tx, scope.Key(serviceName, kindClusterSnapshot, id))
		if e != nil {
			return e
		}

		if found {
			return awserror.New("DBClusterSnapshotAlreadyExistsFault", "Snapshot already exists")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	r := common.Clone(source)
	r.Name, r.Kind, r.Tags, r.Container, r.Volume = id, kindClusterSnapshot, parsed, "", ""
	c := source.Cluster
	_, port, _ := clusterEngine(common.Value(c.Engine), common.Value(c.EngineVersion))
	now := time.Now().UTC()
	r.ClusterSnapshot = &api.DBClusterSnapshot{
		DBClusterSnapshotIdentifier: new(id),
		DBClusterIdentifier:         c.DBClusterIdentifier,
		SnapshotCreateTime: new(
			now,
		),
		ClusterCreateTime: c.ClusterCreateTime,
		Engine:            c.Engine,
		EngineVersion:     c.EngineVersion,
		MasterUsername:    c.MasterUsername,
		Port:              new(port),
		Status:            new(creating),
		PercentProgress:   new(int64(0)),
		DBClusterSnapshotArn: new(
			arn(scope, kindClusterSnapshot, id),
		),
		SnapshotType:     new("manual"),
		StorageEncrypted: c.StorageEncrypted,
	}

	if s.runtime != nil {
		if err := s.runtime.Copy(ctx, s.clusterSpec(ctx, scope, source), s.clusterSpec(ctx, scope, r)); err != nil {
			return nil, err
		}

		r.Volume = s.clusterSpec(ctx, scope, r).Volume
	}

	r.ReadyAt = time.Now().Add(snapshotDelay)
	if err := s.insert(ctx, scope, r); err != nil {
		if s.runtime != nil {
			_ = s.runtime.Remove(ctx, s.clusterSpec(ctx, scope, r), true)
		}

		return nil, err
	}

	return r, nil
}

func (s *Service) CreateDBClusterSnapshot(
	ctx context.Context,
	in *api.CreateDBClusterSnapshotMessage,
) (*api.CreateDBClusterSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.loadCluster(ctx, scope, common.Value(in.DBClusterIdentifier))
	if err != nil {
		return nil, err
	}

	r, err := s.createClusterSnapshot(ctx, scope, source, common.Value(in.DBClusterSnapshotIdentifier), in.Tags)
	if err != nil {
		return nil, err
	}

	return &api.CreateDBClusterSnapshotResult{DBClusterSnapshot: clusterSnapshot(r)}, nil
}

func (s *Service) DescribeDBClusterSnapshots(
	ctx context.Context,
	in *api.DescribeDBClusterSnapshotsMessage,
) (*api.DBClusterSnapshotMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.SnapshotType != nil && *in.SnapshotType != "manual" {
		return nil, invalid("Only manual cluster snapshots are supported")
	}

	records, err := s.list(ctx, scope, kindClusterSnapshot, common.Value(in.DBClusterSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	var selected []*record
	for _, r := range records {
		if in.DBClusterIdentifier == nil ||
			common.Value(r.ClusterSnapshot.DBClusterIdentifier) == *in.DBClusterIdentifier {
			selected = append(selected, r)
		}
	}

	selected, marker, err := pageRecords(selected, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DBClusterSnapshotMessage{Marker: marker}
	for _, r := range selected {
		out.DBClusterSnapshots = append(out.DBClusterSnapshots, *clusterSnapshot(r))
	}

	return out, nil
}

func (s *Service) DeleteDBClusterSnapshot(
	ctx context.Context,
	in *api.DeleteDBClusterSnapshotMessage,
) (*api.DeleteDBClusterSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord(tx, scope, kindClusterSnapshot, common.Value(in.DBClusterSnapshotIdentifier))
		return e
	})
	if err != nil {
		return nil, err
	}

	if common.Value(clusterSnapshot(r).Status) != available {
		return nil, awserror.New("InvalidDBClusterSnapshotStateFault", "Snapshot is not available")
	}

	if s.runtime != nil && r.Volume != "" {
		if err := s.runtime.Remove(ctx, s.clusterSpec(ctx, scope, r), true); err != nil {
			return nil, err
		}
	}

	if err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, kindClusterSnapshot, r.Name))
		return nil
	}); err != nil {
		return nil, err
	}

	return &api.DeleteDBClusterSnapshotResult{DBClusterSnapshot: clusterSnapshot(r)}, nil
}

package rds

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
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
	c := source.Cluster
	engine := common.Value(c.Engine)
	if s.runtime != nil && engine == EngineNeptune {
		return nil, awserror.New("InvalidDBClusterStateFault", "Docker Neptune has no durable graph data to snapshot")
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

	r := common.Clone(source)
	r.Name, r.Kind, r.Tags, r.Container, r.Volume = id, kindClusterSnapshot, parsed, "", ""
	port := common.Value(c.Port)
	if s.runtime != nil {
		_, port, _ = clusterEngine(engine, common.Value(c.EngineVersion))
	}

	var vpc *string
	if c.DBSubnetGroup != nil {
		err := s.store.View(ctx, func(tx storage.ReadTx) error {
			group, err := readRecord(tx, scope, kindSubnet, *c.DBSubnetGroup)
			if err != nil {
				return err
			}

			vpc = group.Subnet.VpcId
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	r.Since = now
	r.ClusterSnapshot = &api.DBClusterSnapshot{
		DBClusterSnapshotIdentifier: new(id),
		DBClusterIdentifier:         c.DBClusterIdentifier,
		SnapshotCreateTime:          new(now),
		ClusterCreateTime:           c.ClusterCreateTime,
		Engine:                      c.Engine,
		EngineVersion:               c.EngineVersion,
		MasterUsername:              c.MasterUsername,
		Port:                        new(port),
		Status:                      new(creating),
		PercentProgress:             new(int64(0)),
		DBClusterSnapshotArn:        new(arn(scope, kindClusterSnapshot, id)),
		SnapshotType:                new(manualSnapshot),
		StorageEncrypted:            c.StorageEncrypted,
		KmsKeyId:                    c.KmsKeyId,
		VpcId:                       vpc,
	}

	if err := s.saveSnapshot(ctx, scope, source, r); err != nil {
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

	if in.SnapshotType != nil && *in.SnapshotType != manualSnapshot {
		return nil, invalid("Only manual cluster snapshots are supported")
	}

	id, err := resourceID(scope, kindClusterSnapshot, common.Value(in.DBClusterSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	sourceID, err := resourceID(scope, kindCluster, common.Value(in.DBClusterIdentifier))
	if err != nil {
		return nil, err
	}

	records, err := s.list(ctx, scope, kindClusterSnapshot, id)
	if err != nil {
		return nil, err
	}

	var selected []*record
	for _, r := range records {
		if sourceID == "" || common.Value(r.ClusterSnapshot.DBClusterIdentifier) == sourceID {
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
	r, err := s.snapshotRecord(ctx, scope, kindClusterSnapshot, common.Value(in.DBClusterSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	if err := clusterSnapshotReady(r); err != nil {
		return nil, err
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

	out := clusterSnapshot(r)
	out.Status = new("deleted")
	return &api.DeleteDBClusterSnapshotResult{DBClusterSnapshot: out}, nil
}

func clusterSnapshotReady(r *record) error {
	if common.Value(clusterSnapshot(r).Status) != available {
		return snapshotStateError(kindClusterSnapshot, "Snapshot is not available")
	}

	return nil
}

func (s *Service) CopyDBClusterSnapshot(
	ctx context.Context,
	in *api.CopyDBClusterSnapshotMessage,
) (*api.CopyDBClusterSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.snapshotRecord(ctx, scope, kindClusterSnapshot, common.Value(in.SourceDBClusterSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	if readyErr := clusterSnapshotReady(source); readyErr != nil {
		return nil, readyErr
	}

	id := strings.ToLower(common.Value(in.TargetDBClusterSnapshotIdentifier))
	if !validName(id, 255) {
		return nil, invalid("Invalid TargetDBClusterSnapshotIdentifier")
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	if common.Value(in.CopyTags) {
		tags, err = tagging.Merge(source.Tags, tags)
		if err != nil {
			return nil, invalid(err.Error())
		}
	}

	r := common.Clone(source)
	r.Name, r.Tags, r.Volume, r.Since = id, tags, "", time.Now().UTC()
	r.RestoreAccounts = nil
	snap := r.ClusterSnapshot
	snap.DBClusterSnapshotIdentifier, snap.DBClusterSnapshotArn = new(id), new(arn(scope, kindClusterSnapshot, id))
	snap.SourceDBClusterSnapshotArn = source.ClusterSnapshot.DBClusterSnapshotArn
	snap.SnapshotCreateTime = new(r.Since)
	snap.Status, snap.PercentProgress = new(creating), new(int64(0))
	if err := s.saveSnapshot(ctx, scope, source, r); err != nil {
		return nil, err
	}

	return &api.CopyDBClusterSnapshotResult{DBClusterSnapshot: clusterSnapshot(r)}, nil
}

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

func (s *Service) RestoreDBClusterFromSnapshot(
	ctx context.Context,
	in *api.RestoreDBClusterFromSnapshotMessage,
) (*api.RestoreDBClusterFromSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.snapshotRecord(ctx, scope, kindClusterSnapshot, common.Value(in.SnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	return s.restoreClusterSnapshot(ctx, scope, source, in, nil)
}

func (s *Service) restoreClusterSnapshot(
	ctx context.Context, scope request.Scope, source *record, in *api.RestoreDBClusterFromSnapshotMessage,
	preferredBackupWindow *string,
) (*api.RestoreDBClusterFromSnapshotResult, error) {
	id := strings.ToLower(common.Value(in.DBClusterIdentifier))
	if !validName(id, 63) {
		return nil, invalid("Invalid DBClusterIdentifier")
	}

	if readyErr := clusterSnapshotReady(source); readyErr != nil {
		return nil, readyErr
	}

	snap := source.ClusterSnapshot
	engine, version := common.Value(snap.Engine), common.Value(snap.EngineVersion)
	if (in.Engine != nil && *in.Engine != engine) || (in.EngineVersion != nil && *in.EngineVersion != version) {
		return nil, awserror.New("InvalidRestoreFault", "Restore requires the snapshot's engine and engine version")
	}

	backup, err := clusterBackupWindow(engine, preferredBackupWindow)
	if err != nil {
		return nil, err
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	c := &api.DBCluster{
		DBClusterIdentifier: new(id),
		Engine:              snap.Engine,
		EngineVersion:       snap.EngineVersion,
		DatabaseName:        source.Cluster.DatabaseName,
		MasterUsername:      snap.MasterUsername,
		Port:                common.OrElse(in.Port, snap.Port),
		DBClusterParameterGroup: common.OrElse(
			in.DBClusterParameterGroupName,
			new("default."+clusterFamily(engine, version)),
		),
		VpcSecurityGroups:          clusterGroups(in.VpcSecurityGroupIds),
		BackupRetentionPeriod:      common.OrElse(in.BackupRetentionPeriod, source.Cluster.BackupRetentionPeriod),
		PreferredMaintenanceWindow: new(defaultMaintenanceWindow),
		PreferredBackupWindow:      backup,
		StorageEncrypted:           snap.StorageEncrypted,
		KmsKeyId:                   snap.KmsKeyId,
		DeletionProtection:         common.OrElse(in.DeletionProtection, new(false)),
		Status:                     new(creating),
	}
	setClusterIdentity(scope, c, id, engine, now)
	if in.DBSubnetGroupName != nil {
		c.DBSubnetGroup = new(strings.ToLower(*in.DBSubnetGroupName))
	}

	if checkErr := checkCluster(c); checkErr != nil {
		return nil, checkErr
	}

	r := &record{
		Name: id, Kind: kindCluster, Cluster: c, Tags: tags, Since: now, ReadyAt: now.Add(snapshotDelay),
		Password: source.Password, RootPassword: source.RootPassword,
	}
	// Validate before allocating the target volume. The lifecycle lock protects RDS references through insertion.
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[record](tx, scope.Key(serviceName, kindCluster, id))
		if readErr != nil {
			return readErr
		}

		if found {
			return resourceError(kindCluster, "AlreadyExists", id)
		}

		if networkErr := checkClusterNetwork(tx, scope, c); networkErr != nil {
			if apiErr := awserror.From(networkErr); apiErr.Code == "DBParameterGroupNotFoundFault" {
				return awserror.New("DBClusterParameterGroupNotFoundFault", apiErr.Message)
			}

			return networkErr
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		if err := s.restoreClusterVolume(ctx, scope, source, r); err != nil {
			return nil, err
		}
	}

	if err := s.insert(ctx, scope, r); err != nil {
		if s.runtime != nil {
			_ = s.runtime.Remove(ctx, s.clusterSpec(ctx, scope, r), true)
		}

		return nil, err
	}

	return &api.RestoreDBClusterFromSnapshotResult{DBCluster: cluster(r)}, nil
}

func (s *Service) restoreClusterVolume(ctx context.Context, scope request.Scope, source, target *record) error {
	if source.Volume == "" {
		return snapshotStateError(kindClusterSnapshot, "Records-only snapshots have no Docker data volume")
	}

	sourceSpec := s.clusterSpec(ctx, scope, source)
	c := target.Cluster
	if common.Value(c.Port) != sourceSpec.Port {
		return awserror.New("InvalidRestoreFault", "Custom cluster engine ports are unsupported in Docker mode")
	}

	// An unnamed SQL database was initialized using the source cluster identifier.
	if aurora(common.Value(c.Engine)) && common.Value(c.DatabaseName) == "" {
		c.DatabaseName = new(sourceSpec.Database)
	}

	targetSpec := s.clusterSpec(ctx, scope, target)
	if err := s.runtime.Copy(ctx, sourceSpec, targetSpec); err != nil {
		return err
	}

	c.Endpoint, c.ReaderEndpoint, c.Port = nil, nil, new(int64(0))
	target.Volume, target.Initialized, target.ReadyAt = targetSpec.Volume, true, time.Time{}
	return nil
}

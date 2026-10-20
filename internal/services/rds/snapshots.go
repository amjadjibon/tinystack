package rds

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const snapshotDelay = 250 * time.Millisecond

func snapshot(r *record) *api.DBSnapshot {
	out := common.Clone(r.Snapshot)
	out.TagList = tagList(r.Tags)
	if !time.Now().Before(r.ReadyAt) {
		out.Status = new(available)
		out.PercentProgress = new(int64(100))
	}

	return out
}

// resourceID accepts local names and ARNs, without crossing the request scope.
func resourceID(scope request.Scope, kind, id string) (string, error) {
	if strings.HasPrefix(id, "arn:") {
		prefix := arn(scope, kind, "")
		if !strings.HasPrefix(id, prefix) {
			return "", invalid("Resource ARN must belong to the request account and Region")
		}

		id = strings.TrimPrefix(id, prefix)
	}

	return strings.ToLower(id), nil
}

func (s *Service) snapshotRecord(ctx context.Context, scope request.Scope, id string) (*record, error) {
	id, err := resourceID(scope, kindSnapshot, id)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord(tx, scope, kindSnapshot, id)
		return e
	})
	return r, err
}

func snapshotReady(r *record) error {
	if common.Value(snapshot(r).Status) != available {
		return awserror.New("InvalidDBSnapshotStateFault", "Snapshot is not available")
	}

	return nil
}

// createSnapshot is called with the lifecycle lock held, including for final snapshots.
func (s *Service) createSnapshot(
	ctx context.Context, scope request.Scope, source *record, id string, tags []api.Tag, final bool,
) (*record, error) {
	id = strings.ToLower(id)
	if !validName(id, 255) {
		return nil, invalid("Invalid DBSnapshotIdentifier")
	}

	status := common.Value(instance(source).DBInstanceStatus)
	if source.Instance.DBClusterIdentifier != nil ||
		(status != available && status != "storage-optimization" && (!final || status != stopped)) {
		return nil, awserror.New("InvalidDBInstanceStateFault", "A standalone available DB instance is required")
	}

	parsed, err := parseTags(tags)
	if err != nil {
		return nil, err
	}

	if len(tags) == 0 && common.Value(source.Instance.CopyTagsToSnapshot) {
		parsed = common.CloneTags(source.Tags)
	}

	r := common.Clone(source)
	r.SourceTags = common.CloneTags(source.Tags)
	now := time.Now().UTC()
	r.Name, r.Kind, r.Tags, r.Since = id, kindSnapshot, parsed, now
	r.Container, r.Volume, r.ReadyAt = "", "", now.Add(snapshotDelay)
	db := source.Instance
	_, _, port, _ := Engine(common.Value(db.Engine), common.Value(db.EngineVersion))
	r.Snapshot = &api.DBSnapshot{
		DBSnapshotIdentifier: new(id), DBInstanceIdentifier: db.DBInstanceIdentifier,
		DBSnapshotArn: new(arn(scope, kindSnapshot, id)), SnapshotType: new("manual"),
		Status: new(creating), PercentProgress: new(int64(0)), Engine: db.Engine, EngineVersion: db.EngineVersion,
		AllocatedStorage: db.AllocatedStorage, MasterUsername: db.MasterUsername, Port: new(port),
		InstanceCreateTime: db.InstanceCreateTime, SnapshotCreateTime: new(now), OriginalSnapshotCreateTime: new(now),
		DbiResourceId: db.DbiResourceId, Encrypted: new(false),
	}

	if db.DBSubnetGroup != nil {
		r.Snapshot.VpcId = db.DBSubnetGroup.VpcId
	}

	// Preserve the actual database name when it was derived from the source identifier.
	if r.Instance.DBName == nil {
		r.Instance.DBName = new(strings.ReplaceAll(source.Name, "-", "_"))
	}

	if err := s.saveSnapshot(ctx, scope, source, r); err != nil {
		return nil, err
	}

	return r, nil
}

// saveSnapshot checks duplicates before any I/O and removes a failed copy's owned volume.
func (s *Service) saveSnapshot(ctx context.Context, scope request.Scope, source, target *record) error {
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, e := storage.Read[record](tx, scope.Key(serviceName, kindSnapshot, target.Name))
		if e != nil {
			return e
		}

		if found {
			return resourceError(kindSnapshot, "AlreadyExists", target.Name)
		}

		return nil
	})
	if err != nil {
		return err
	}

	if s.runtime != nil {
		if source.Kind == kindSnapshot && source.Volume == "" {
			return awserror.New("InvalidDBSnapshotStateFault", "Records-only snapshots have no Docker data volume")
		}

		if err := s.runtime.Copy(ctx, s.dataSpec(scope, source), s.dataSpec(scope, target)); err != nil {
			return err
		}

		target.Volume = s.dataSpec(scope, target).Volume
	}

	target.ReadyAt = time.Now().UTC().Add(snapshotDelay)
	if err := s.insert(ctx, scope, target); err != nil {
		if s.runtime != nil {
			_ = s.runtime.Remove(ctx, s.dataSpec(scope, target), true)
		}

		return err
	}

	return nil
}

func (s *Service) CreateDBSnapshot(
	ctx context.Context, in *api.CreateDBSnapshotMessage,
) (*api.CreateDBSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := resourceID(scope, kindDB, common.Value(in.DBInstanceIdentifier))
	if err != nil {
		return nil, err
	}

	var source *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		source, e = readRecord(tx, scope, kindDB, id)
		return e
	})
	if err != nil {
		return nil, err
	}

	r, err := s.createSnapshot(ctx, scope, source, common.Value(in.DBSnapshotIdentifier), in.Tags, false)
	if err != nil {
		return nil, err
	}

	return &api.CreateDBSnapshotResult{DBSnapshot: snapshot(r)}, nil
}

func (s *Service) DescribeDBSnapshots(
	ctx context.Context, in *api.DescribeDBSnapshotsMessage,
) (*api.DBSnapshotMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	typ := common.Value(in.SnapshotType)
	if typ != "" && typ != "manual" && typ != "automated" {
		return nil, invalid("Supported SnapshotType values are manual and automated")
	}

	id, err := resourceID(scope, kindSnapshot, common.Value(in.DBSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	db, err := resourceID(scope, kindDB, common.Value(in.DBInstanceIdentifier))
	if err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		if !slices.Contains(
			[]string{instanceFilter, "db-snapshot-id", "dbi-resource-id", "snapshot-type", engineFilter},
			common.Value(f.Name),
		) ||
			len(f.Values) == 0 {
			return nil, invalid("Unsupported filter or empty filter Values")
		}

		for _, v := range f.Values {
			if strings.ContainsAny(v, "*?") {
				return nil, invalid("Filter wildcards are unsupported")
			}
		}
	}

	records, err := s.list(ctx, scope, kindSnapshot, id)
	if err != nil {
		return nil, err
	}

	selected := make([]*record, 0, len(records))
	for _, r := range records {
		snap := r.Snapshot
		if (db != "" && db != common.Value(snap.DBInstanceIdentifier)) ||
			(typ != "" && typ != common.Value(snap.SnapshotType)) ||
			(in.DbiResourceId != nil && *in.DbiResourceId != common.Value(snap.DbiResourceId)) {
			continue
		}

		match := true
		for _, f := range in.Filters {
			value := map[string]string{
				instanceFilter: common.Value(snap.DBInstanceIdentifier), "db-snapshot-id": r.Name,
				"dbi-resource-id": common.Value(snap.DbiResourceId), "snapshot-type": common.Value(snap.SnapshotType),
				engineFilter: common.Value(snap.Engine),
			}[common.Value(f.Name)]
			matches := false
			for _, v := range f.Values {
				if common.Value(f.Name) == instanceFilter {
					v, err = resourceID(scope, kindDB, v)
					if err != nil {
						return nil, err
					}
				}

				matches = matches || v == value
			}

			match = match && matches
		}

		if match {
			selected = append(selected, r)
		}
	}

	selected, marker, err := pageRecords(selected, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DBSnapshotMessage{Marker: marker}
	for _, r := range selected {
		out.DBSnapshots = append(out.DBSnapshots, *snapshot(r))
	}

	return out, nil
}

func (s *Service) DeleteDBSnapshot(
	ctx context.Context, in *api.DeleteDBSnapshotMessage,
) (*api.DeleteDBSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.snapshotRecord(ctx, scope, common.Value(in.DBSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	if readyErr := snapshotReady(r); readyErr != nil {
		return nil, readyErr
	}

	if s.runtime != nil && r.Volume != "" {
		if removeErr := s.runtime.Remove(ctx, s.dataSpec(scope, r), true); removeErr != nil {
			return nil, removeErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, kindSnapshot, r.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := snapshot(r)
	out.Status = new("deleted")
	return &api.DeleteDBSnapshotResult{DBSnapshot: out}, nil
}

func (s *Service) CopyDBSnapshot(
	ctx context.Context, in *api.CopyDBSnapshotMessage,
) (*api.CopyDBSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.snapshotRecord(ctx, scope, common.Value(in.SourceDBSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	if readyErr := snapshotReady(source); readyErr != nil {
		return nil, readyErr
	}

	id := strings.ToLower(common.Value(in.TargetDBSnapshotIdentifier))
	if !validName(id, 255) {
		return nil, invalid("Invalid TargetDBSnapshotIdentifier")
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
	r.Snapshot.DBSnapshotIdentifier, r.Snapshot.DBSnapshotArn = new(id), new(arn(scope, kindSnapshot, id))
	r.Snapshot.SourceDBSnapshotIdentifier = source.Snapshot.DBSnapshotArn
	r.Snapshot.SourceRegion = new(scope.Region)
	r.Snapshot.SnapshotCreateTime = new(r.Since)
	r.Snapshot.Status, r.Snapshot.PercentProgress = new(creating), new(int64(0))
	if err := s.saveSnapshot(ctx, scope, source, r); err != nil {
		return nil, err
	}

	return &api.CopyDBSnapshotResult{DBSnapshot: snapshot(r)}, nil
}

func (s *Service) RestoreDBInstanceFromDBSnapshot(
	ctx context.Context, in *api.RestoreDBInstanceFromDBSnapshotMessage,
) (*api.RestoreDBInstanceFromDBSnapshotResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.snapshotRecord(ctx, scope, common.Value(in.DBSnapshotIdentifier))
	if err != nil {
		return nil, err
	}

	if readyErr := snapshotReady(source); readyErr != nil {
		return nil, readyErr
	}

	id := strings.ToLower(common.Value(in.DBInstanceIdentifier))
	if !validName(id, 63) {
		return nil, invalid("Invalid DBInstanceIdentifier")
	}

	r := common.Clone(source)
	r.Name, r.Kind, r.Snapshot, r.Since = id, kindDB, nil, time.Now().UTC()
	r.Container, r.Volume, r.Initialized = "", "", false
	r.Tags, err = parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(in.Tags) == 0 {
		r.Tags = common.CloneTags(source.SourceTags)
	}

	r.SourceTags = nil

	db := r.Instance
	db.DBInstanceIdentifier, db.DBInstanceArn = new(id), new(arn(scope, kindDB, id))
	db.DbiResourceId, db.InstanceCreateTime = new("db-"+common.RandomID(26)), new(r.Since)
	db.DBInstanceStatus, db.CopyTagsToSnapshot = new(creating), in.CopyTagsToSnapshot
	db.DBParameterGroups = nil
	if in.DBInstanceClass != nil {
		if !strings.HasPrefix(*in.DBInstanceClass, "db.") || len(*in.DBInstanceClass) < 4 {
			return nil, invalid("Invalid DBInstanceClass")
		}

		db.DBInstanceClass = in.DBInstanceClass
	}

	if in.AllocatedStorage != nil {
		if *in.AllocatedStorage < common.Value(db.AllocatedStorage) || *in.AllocatedStorage > 65536 {
			return nil, invalid("AllocatedStorage cannot be smaller than the snapshot or exceed 65536 GiB")
		}

		db.AllocatedStorage = in.AllocatedStorage
	}

	// Validate references and duplicate identity before provisioning a volume.
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, e := storage.Read[record](tx, scope.Key(serviceName, kindDB, id))
		if e != nil {
			return e
		}

		if found {
			return resourceError(kindDB, "AlreadyExists", id)
		}

		if in.DBSubnetGroupName != nil {
			group, e := readRecord(tx, scope, kindSubnet, *in.DBSubnetGroupName)
			if e != nil {
				return e
			}

			db.DBSubnetGroup = group.Subnet
		}

		if in.DBParameterGroupName != nil {
			group, e := readRecord(tx, scope, kindParameter, *in.DBParameterGroupName)
			if e != nil {
				return e
			}

			if common.Value(
				group.Parameter.DBParameterGroupFamily,
			) != common.Value(
				db.Engine,
			)+common.Value(
				db.EngineVersion,
			) {
				return invalid("DBParameterGroupFamily does not match the engine version")
			}

			db.DBParameterGroups = []api.DBParameterGroupStatus{{
				DBParameterGroupName: new(group.Name), ParameterApplyStatus: new("in-sync"),
			}}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	_, _, port, _ := Engine(common.Value(db.Engine), common.Value(db.EngineVersion))
	db.Endpoint = &api.Endpoint{Address: new("127.0.0.1"), Port: new(port)}
	r.ReadyAt = time.Now().UTC().Add(snapshotDelay)
	if s.runtime != nil {
		if source.Volume == "" {
			return nil, awserror.New("InvalidDBSnapshotStateFault", "Records-only snapshots have no Docker data volume")
		}

		r.ReadyAt = time.Time{}
		db.Endpoint = nil
		if err := s.runtime.Copy(ctx, s.dataSpec(scope, source), s.spec(scope, r)); err != nil {
			return nil, err
		}

		r.Volume, r.Initialized = s.spec(scope, r).Volume, true
	}

	if err := s.insert(ctx, scope, r); err != nil {
		if s.runtime != nil {
			_ = s.runtime.Remove(ctx, s.spec(scope, r), true)
		}

		return nil, err
	}

	return &api.RestoreDBInstanceFromDBSnapshotResult{DBInstance: instance(r)}, nil
}

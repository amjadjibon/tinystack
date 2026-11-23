package rds

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	backupNotFound       = "DBInstanceAutomatedBackupNotFoundFault"
	backupIDFilter       = "dbi-resource-id"
	backupStatusFilter   = "status"
	snapshotRestoreLimit = "TinyStack restores at the automated snapshot time without transaction log replay"
)

type backupSeries struct {
	live      *record
	snapshots []*record
}

func restoreWindow(snapshots []*record, live *record, now time.Time) api.RestoreWindow {
	var window api.RestoreWindow
	for _, snap := range snapshots {
		if backupExpired(snap, live, now) {
			continue
		}

		if window.EarliestTime == nil || snap.Since.Before(*window.EarliestTime) {
			window.EarliestTime = new(snap.Since)
		}

		if window.LatestTime == nil || snap.Since.After(*window.LatestTime) {
			window.LatestTime = new(snap.Since)
		}
	}

	return window
}

func (b backupSeries) describe(scope request.Scope, now time.Time) *api.DBInstanceAutomatedBackup {
	r, status := b.live, "active"
	window := restoreWindow(b.snapshots, b.live, now)
	switch {
	case r == nil:
		if window.LatestTime == nil {
			return nil
		}

		status = "retained"
		r = b.snapshots[len(b.snapshots)-1]
	case backupSourceOf(r).retention == 0:
		return nil
	case window.LatestTime == nil:
		status = creating
	}

	db := instance(r)
	out := &api.DBInstanceAutomatedBackup{
		DBInstanceArn: db.DBInstanceArn, DbiResourceId: db.DbiResourceId, Region: new(scope.Region),
		DBInstanceIdentifier: db.DBInstanceIdentifier, Status: new(status), AllocatedStorage: db.AllocatedStorage,
		Engine: db.Engine, EngineVersion: db.EngineVersion, InstanceCreateTime: db.InstanceCreateTime,
		MasterUsername: db.MasterUsername, Encrypted: new(false), BackupRetentionPeriod: db.BackupRetentionPeriod,
		PreferredBackupWindow: db.PreferredBackupWindow, TagList: db.TagList,
		DBInstanceAutomatedBackupsArn: new(arn(scope, backupARNKind, common.Value(db.DbiResourceId))),
	}
	if window.LatestTime != nil {
		out.RestoreWindow = &window
	}

	if db.DBSubnetGroup != nil {
		out.VpcId = db.DBSubnetGroup.VpcId
	}

	if len(db.OptionGroupMemberships) > 0 {
		out.OptionGroupName = db.OptionGroupMemberships[0].OptionGroupName
	}

	_, _, port, _ := Engine(common.Value(db.Engine), common.Value(db.EngineVersion))
	out.Port = new(port)
	return out
}

func instanceBackupSeries(tx storage.ReadTx, scope request.Scope) (map[string]backupSeries, error) {
	series := make(map[string]backupSeries)
	records, err := storage.Records[record](tx, scope.Prefix(serviceName, kindDB))
	if err != nil {
		return nil, err
	}

	for _, r := range records {
		if r.Instance.DBClusterIdentifier == nil && r.Instance.DbiResourceId != nil {
			series[strings.ToLower(*r.Instance.DbiResourceId)] = backupSeries{live: r}
		}
	}

	prefix := backupScopePrefix(scope, kindDB)
	for _, key := range tx.Keys(prefix) {
		id, snapshotID, ok := strings.Cut(strings.TrimPrefix(key, prefix), "/")
		if !ok {
			continue
		}

		snap, found, err := storage.Read[record](tx, scope.Key(serviceName, kindSnapshot, snapshotID))
		if err != nil {
			return nil, err
		}

		if found {
			item := series[id]
			item.snapshots = append(item.snapshots, snap)
			series[id] = item
		}
	}

	return series, nil
}

func instanceBackupFilterValues(scope request.Scope, filters []api.Filter) ([]api.Filter, error) {
	if err := validateFilters(filters, instanceFilter, backupIDFilter, backupStatusFilter); err != nil {
		return nil, err
	}

	out := make([]api.Filter, 0, len(filters))
	for _, filter := range filters {
		normalized := api.Filter{Name: filter.Name}
		for _, value := range filter.Values {
			var err error
			switch *filter.Name {
			case instanceFilter:
				value, err = resourceID(scope, kindDB, value)
			case backupIDFilter:
				value, err = resourceID(scope, backupARNKind, value)
			case backupStatusFilter:
				if !slices.Contains([]string{"active", creating, "retained"}, value) {
					return nil, invalid("Unsupported automated backup status")
				}
			}

			if err != nil {
				return nil, err
			}

			normalized.Values = append(normalized.Values, value)
		}

		out = append(out, normalized)
	}

	return out, nil
}

func (s *Service) DescribeDBInstanceAutomatedBackups(
	ctx context.Context, in *api.DescribeDBInstanceAutomatedBackupsMessage,
) (*api.DBInstanceAutomatedBackupMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	filters, err := instanceBackupFilterValues(scope, in.Filters)
	if err != nil {
		return nil, err
	}

	name, err := resourceID(scope, kindDB, common.Value(in.DBInstanceIdentifier))
	if err != nil {
		return nil, err
	}

	id, err := resourceID(scope, backupARNKind, common.Value(in.DBInstanceAutomatedBackupsArn))
	if err != nil {
		return nil, err
	}

	var items []api.DBInstanceAutomatedBackup
	now := s.now()
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		series, readErr := instanceBackupSeries(tx, scope)
		if readErr != nil {
			return readErr
		}

		for _, uid := range slices.Sorted(maps.Keys(series)) {
			if (id != "" && uid != id) ||
				(in.DbiResourceId != nil && !strings.EqualFold(uid, *in.DbiResourceId)) {
				continue
			}

			backup := series[uid].describe(scope, now)
			if backup == nil || (name != "" && *backup.DBInstanceIdentifier != name) {
				continue
			}

			if matchesFilters(filters, map[string]string{
				instanceFilter: *backup.DBInstanceIdentifier, backupIDFilter: uid, backupStatusFilter: *backup.Status,
			}) {
				items = append(items, *backup)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(items) == 0 && (in.DbiResourceId != nil || in.DBInstanceIdentifier != nil ||
		in.DBInstanceAutomatedBackupsArn != nil) && len(filters) == 0 {
		return nil, awserror.New(backupNotFound, "DB instance automated backup not found")
	}

	page, marker, err := pageDiscovery(items, func(v api.DBInstanceAutomatedBackup) string {
		return common.Value(v.DbiResourceId)
	}, in.Marker, in.MaxRecords)
	return &api.DBInstanceAutomatedBackupMessage{DBInstanceAutomatedBackups: page, Marker: marker}, err
}

func chooseBackupID(scope request.Scope, id, backupARN *string) (string, error) {
	if (id == nil) == (backupARN == nil) {
		return "", invalid("Specify either DbiResourceId or DBInstanceAutomatedBackupsArn")
	}

	if id != nil {
		if !validName(*id, 255) {
			return "", invalid("Invalid DbiResourceId")
		}

		return strings.ToLower(*id), nil
	}

	if !strings.HasPrefix(*backupARN, arn(scope, backupARNKind, "")) {
		return "", invalid("DBInstanceAutomatedBackupsArn must belong to the request account and Region")
	}

	return resourceID(scope, backupARNKind, *backupARN)
}

func (s *Service) DeleteDBInstanceAutomatedBackup(
	ctx context.Context, in *api.DeleteDBInstanceAutomatedBackupMessage,
) (*api.DeleteDBInstanceAutomatedBackupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := chooseBackupID(scope, in.DbiResourceId, in.DBInstanceAutomatedBackupsArn)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var item backupSeries
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		series, readErr := instanceBackupSeries(tx, scope)
		item = series[id]
		return readErr
	})
	if err != nil {
		return nil, err
	}

	backup := item.describe(scope, s.now())
	if backup == nil {
		return nil, awserror.New(backupNotFound, "DB instance automated backup not found")
	}

	if item.live != nil {
		return nil, awserror.New("InvalidDBInstanceAutomatedBackupStateFault", "Only retained backups can be deleted")
	}

	for _, snap := range item.snapshots {
		if err := s.removeBackupSnapshot(ctx, scope, snap); err != nil {
			return nil, err
		}
	}

	backup.Status = new("deleting")
	return &api.DeleteDBInstanceAutomatedBackupResult{DBInstanceAutomatedBackup: backup}, nil
}

func restoreCutoff(requested *time.Time, latest *bool, now time.Time) (time.Time, error) {
	switch {
	case requested != nil && common.Value(latest):
		return time.Time{}, invalid("Restore time cannot be combined with UseLatestRestorableTime")
	case requested != nil && !requested.IsZero() && !requested.After(now):
		return *requested, nil
	case requested == nil && common.Value(latest):
		return now, nil
	default:
		return time.Time{}, invalid("Provide a past restore time or UseLatestRestorableTime=true")
	}
}

func selectBackup(item backupSeries, cutoff, now time.Time) (*record, error) {
	var selected *record
	hasBackups := false
	for _, snap := range item.snapshots {
		if backupExpired(snap, item.live, now) {
			continue
		}

		hasBackups = true
		if !snap.Since.After(cutoff) &&
			(selected == nil || snap.Since.After(selected.Since)) {
			selected = snap
		}
	}

	if !hasBackups && item.live != nil && backupSourceOf(item.live).retention == 0 {
		return nil, awserror.New(
			"PointInTimeRestoreNotEnabledFault",
			snapshotRestoreLimit+". Automated backups are disabled",
		)
	}

	if selected == nil {
		return nil, awserror.New(
			"InvalidRestoreFault",
			snapshotRestoreLimit+". No retained snapshot exists at or before that time",
		)
	}

	return selected, nil
}

func (s *Service) RestoreDBInstanceToPointInTime(
	ctx context.Context, in *api.RestoreDBInstanceToPointInTimeMessage,
) (*api.RestoreDBInstanceToPointInTimeResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	cutoff, err := restoreCutoff(in.RestoreTime, in.UseLatestRestorableTime, now)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var item backupSeries
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var selectErr error
		item, selectErr = instanceRestoreSeries(tx, scope, in)
		return selectErr
	})
	if err != nil {
		return nil, err
	}

	snap, err := selectBackup(item, cutoff, now)
	if err != nil {
		return nil, err
	}

	result, err := s.restoreInstanceSnapshot(ctx, scope, snap, &api.RestoreDBInstanceFromDBSnapshotMessage{
		DBInstanceIdentifier: in.TargetDBInstanceIdentifier, DBInstanceClass: in.DBInstanceClass,
		DBSubnetGroupName: in.DBSubnetGroupName, DBParameterGroupName: in.DBParameterGroupName,
		AllocatedStorage: in.AllocatedStorage, Tags: in.Tags, CopyTagsToSnapshot: in.CopyTagsToSnapshot,
	}, in)
	if err != nil {
		return nil, err
	}

	return &api.RestoreDBInstanceToPointInTimeResult{DBInstance: result.DBInstance}, nil
}

func instanceRestoreSeries(
	tx storage.ReadTx, scope request.Scope, in *api.RestoreDBInstanceToPointInTimeMessage,
) (backupSeries, error) {
	if in.SourceDBInstanceIdentifier == nil {
		id, err := chooseBackupID(scope, in.SourceDbiResourceId, in.SourceDBInstanceAutomatedBackupsArn)
		if err != nil {
			return backupSeries{}, err
		}

		series, err := instanceBackupSeries(tx, scope)
		item, ok := series[id]
		if !ok && err == nil {
			err = awserror.New(backupNotFound, "DB instance automated backup not found")
		}

		return item, err
	}

	if in.SourceDbiResourceId != nil || in.SourceDBInstanceAutomatedBackupsArn != nil {
		return backupSeries{}, invalid("Specify only one backup source")
	}

	name, err := resourceID(scope, kindDB, *in.SourceDBInstanceIdentifier)
	if err != nil {
		return backupSeries{}, err
	}

	r, err := readRecord(tx, scope, kindDB, name)
	if err != nil {
		return backupSeries{}, err
	}

	if r.Instance.DBClusterIdentifier != nil {
		return backupSeries{}, awserror.New("InvalidDBInstanceStateFault", "Restore the parent DB cluster instead")
	}

	snapshots, err := backupSnapshots(tx, scope, backupSourceOf(r))
	return backupSeries{live: r, snapshots: snapshots}, err
}

func (s *Service) RestoreDBClusterToPointInTime(
	ctx context.Context, in *api.RestoreDBClusterToPointInTimeMessage,
) (*api.RestoreDBClusterToPointInTimeResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.RestoreType != nil && *in.RestoreType != "full-copy" {
		return nil, invalid("RestoreType must be full-copy")
	}

	if (in.SourceDBClusterIdentifier == nil) == (in.SourceDbClusterResourceId == nil) {
		return nil, invalid("Specify either SourceDBClusterIdentifier or SourceDbClusterResourceId")
	}

	now := s.now()
	cutoff, err := restoreCutoff(in.RestoreToTime, in.UseLatestRestorableTime, now)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var item backupSeries
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var selectErr error
		item, selectErr = clusterRestoreSeries(tx, scope, in)
		return selectErr
	})
	if err != nil {
		return nil, err
	}

	snap, err := selectBackup(item, cutoff, now)
	if err != nil {
		return nil, err
	}

	result, err := s.restoreClusterSnapshot(ctx, scope, snap, &api.RestoreDBClusterFromSnapshotMessage{
		DBClusterIdentifier: in.DBClusterIdentifier, Port: in.Port, DBSubnetGroupName: in.DBSubnetGroupName,
		VpcSecurityGroupIds: in.VpcSecurityGroupIds, DBClusterParameterGroupName: in.DBClusterParameterGroupName,
		BackupRetentionPeriod: in.BackupRetentionPeriod, Tags: in.Tags, DeletionProtection: in.DeletionProtection,
	}, in.PreferredBackupWindow)
	if err != nil {
		return nil, err
	}

	return &api.RestoreDBClusterToPointInTimeResult{DBCluster: result.DBCluster}, nil
}

func clusterRestoreSeries(
	tx storage.ReadTx, scope request.Scope, in *api.RestoreDBClusterToPointInTimeMessage,
) (backupSeries, error) {
	if in.SourceDbClusterResourceId != nil {
		if !validName(*in.SourceDbClusterResourceId, 255) {
			return backupSeries{}, invalid("Invalid SourceDbClusterResourceId")
		}

		snapshots, err := backupSnapshots(tx, scope, backupSource{kind: kindCluster, id: *in.SourceDbClusterResourceId})
		if err != nil {
			return backupSeries{}, err
		}

		if len(snapshots) == 0 {
			return backupSeries{}, awserror.New(
				"DBClusterAutomatedBackupNotFoundFault",
				"Aurora automated backup not found",
			)
		}

		r, err := backupSourceOf(snapshots[0]).live(tx, scope)
		return backupSeries{live: r, snapshots: snapshots}, err
	}

	name, err := resourceID(scope, kindCluster, *in.SourceDBClusterIdentifier)
	if err != nil {
		return backupSeries{}, err
	}

	r, err := readRecord(tx, scope, kindCluster, name)
	if err != nil {
		return backupSeries{}, err
	}

	if !aurora(common.Value(r.Cluster.Engine)) {
		return backupSeries{}, awserror.New("InvalidDBClusterStateFault", "Point-in-time restore requires Aurora")
	}

	snapshots, err := backupSnapshots(tx, scope, backupSourceOf(r))
	return backupSeries{live: r, snapshots: snapshots}, err
}

package rds

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultBackupWindow = "03:00-03:30" // #nosec G101 -- default UTC backup schedule.
	automatedSnapshot   = "automated"
	backupIndex         = "aws/rds/backup-index/"
	backupARNKind       = "auto-backup"
	backupDay           = 24 * time.Hour
	backupRetry         = time.Minute
)

func backupWindow(value *string) (string, error) {
	window := common.Value(common.OrElse(value, new(defaultBackupWindow)))
	start, length := backupWindowOffsets(window)
	if start < 0 || length < 30*time.Minute {
		return "", invalid("PreferredBackupWindow must use hh:mm-hh:mm UTC and be at least 30 minutes")
	}

	return window, nil
}

func clusterBackupWindow(engine string, value *string) (*string, error) {
	if !aurora(engine) {
		if value != nil {
			return nil, invalid("PreferredBackupWindow is supported for standalone RDS instances and Aurora clusters")
		}

		return nil, nil //nolint:nilnil // Additional cluster engines have no backup window.
	}

	window, err := backupWindow(value)
	return new(window), err
}

func backupWindowOffsets(window string) (startOffset, length time.Duration) {
	if len(window) != len(defaultBackupWindow) || window[5] != '-' {
		return -1, 0
	}

	start, startErr := time.Parse("15:04", window[:5])
	end, endErr := time.Parse("15:04", window[6:])
	if startErr != nil || endErr != nil {
		return -1, 0
	}

	left := time.Duration(start.Hour())*time.Hour + time.Duration(start.Minute())*time.Minute
	right := time.Duration(end.Hour())*time.Hour + time.Duration(end.Minute())*time.Minute
	return left, (right - left + backupDay) % backupDay
}

func currentBackupSlot(window *string, now time.Time) time.Time {
	start, length := backupWindowOffsets(common.Value(common.OrElse(window, new(defaultBackupWindow))))
	slot := now.UTC().Truncate(backupDay).Add(start)
	if slot.After(now) {
		slot = slot.Add(-backupDay)
	}

	if now.Before(slot.Add(length)) {
		return slot
	}

	return time.Time{}
}

func snapshotTime(captured time.Time) (at time.Time, kind string) {
	if captured.IsZero() {
		return time.Now().UTC(), manualSnapshot
	}

	return captured.Truncate(time.Millisecond), automatedSnapshot
}

func isAutomatedSnapshot(r *record) bool {
	return (r.Snapshot != nil && common.Value(r.Snapshot.SnapshotType) == automatedSnapshot) ||
		(r.ClusterSnapshot != nil && common.Value(r.ClusterSnapshot.SnapshotType) == automatedSnapshot)
}

type backupSource struct {
	window         *string
	kind, name, id string
	retention      int64
}

func backupSourceOf(r *record) backupSource {
	if r.Cluster != nil {
		c := r.Cluster
		return backupSource{
			window: c.PreferredBackupWindow, kind: kindCluster, name: common.Value(c.DBClusterIdentifier),
			id: common.Value(c.DbClusterResourceId), retention: common.Value(c.BackupRetentionPeriod),
		}
	}

	db := r.Instance
	return backupSource{
		window: db.PreferredBackupWindow,
		kind:   kindDB,
		name:   common.Value(db.DBInstanceIdentifier),
		id: common.Value(
			db.DbiResourceId,
		),
		retention: common.Value(common.OrElse(db.BackupRetentionPeriod, new(int64(1)))),
	}
}

func backupScopePrefix(scope request.Scope, kind string) string {
	return backupIndex + scope.AccountID + "/" + scope.Region + "/" + kind + "/"
}

func (b backupSource) prefix(scope request.Scope) string {
	return backupScopePrefix(scope, b.kind) + strings.ToLower(b.id) + "/"
}

func (b backupSource) live(tx storage.ReadTx, scope request.Scope) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, b.kind, b.name))
	if err != nil || !found || backupSourceOf(r).id != b.id {
		return nil, err
	}

	return r, nil
}

func registerBackup(tx storage.WriteTx, scope request.Scope, snap *record) error {
	source := backupSourceOf(snap)
	if err := storage.Write(tx, source.prefix(scope)+snap.Name, true); err != nil {
		return err
	}

	r, err := source.live(tx, scope)
	if err != nil || r == nil {
		return err
	}

	r.BackupRetryAt = time.Time{}
	if r.Instance != nil {
		r.Instance.LatestRestorableTime = new(snap.Since)
	} else {
		r.Cluster.LatestRestorableTime = new(snap.Since)
		r.Cluster.EarliestRestorableTime = common.OrElse(r.Cluster.EarliestRestorableTime, new(snap.Since))
	}

	return writeRecord(tx, scope, r)
}

func (s *Service) visitBackups(ctx context.Context) {
	s.visitBackupSources(ctx, indexPrefix, kindDB)
	s.visitBackupSources(ctx, clusterIndex, kindCluster)
	s.pruneBackups(ctx)
}

func (s *Service) visitBackupSources(ctx context.Context, prefix, kind string) {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(prefix)
		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
		if len(parts) == 3 {
			s.takeBackup(ctx, request.Scope{AccountID: parts[0], Region: parts[1]}, kind, parts[2])
		}
	}
}

func backupAvailable(r *record) bool {
	if r.Cluster != nil {
		return aurora(common.Value(r.Cluster.Engine)) && common.Value(cluster(r).Status) == available
	}

	return r.Instance.DBClusterIdentifier == nil && common.Value(instance(r).DBInstanceStatus) == available
}

func (s *Service) takeBackup(ctx context.Context, scope request.Scope, kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	var source *record
	var snapshotID string
	key := scope.Key(serviceName, kind, name)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[record](tx, key)
		if err != nil || !found || !backupAvailable(r) {
			return err
		}

		b := backupSourceOf(r)
		slot := currentBackupSlot(b.window, now)
		if b.retention == 0 || slot.IsZero() || now.Before(r.BackupRetryAt) {
			return nil
		}

		if slot.Equal(r.BackupSlot) && r.BackupRetryAt.IsZero() {
			return nil
		}

		snapshotID = "rds-" + strings.ToLower(b.id) + "-" + slot.Format("2006-01-02-1504")
		r.BackupSlot, r.BackupRetryAt = slot, now.Add(backupRetry)
		source = r
		return writeRecord(tx, scope, r)
	})
	if err != nil || source == nil {
		return
	}

	if kind == kindCluster {
		_, _ = s.createClusterSnapshot(ctx, scope, source, snapshotID, nil, now)
	} else {
		_, _ = s.createSnapshot(ctx, scope, source, snapshotID, nil, false, now)
	}
}

func backupSnapshotKind(kind string) string {
	if kind == kindCluster {
		return kindClusterSnapshot
	}

	return kindSnapshot
}

func backupSnapshots(tx storage.ReadTx, scope request.Scope, source backupSource) ([]*record, error) {
	prefix := source.prefix(scope)
	var out []*record
	for _, key := range tx.Keys(prefix) {
		snap, found, err := storage.Read[record](tx,
			scope.Key(serviceName, backupSnapshotKind(source.kind), strings.TrimPrefix(key, prefix)))
		if err != nil {
			return nil, err
		}

		if found {
			out = append(out, snap)
		}
	}

	return out, nil
}

func backupExpired(snap, live *record, now time.Time) bool {
	source := backupSourceOf(snap)
	if live != nil {
		source = backupSourceOf(live)
	}

	return source.retention == 0 || !now.Before(snap.Since.Add(time.Duration(source.retention)*backupDay))
}

func (s *Service) pruneBackups(ctx context.Context) {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(backupIndex)
		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		parts := strings.Split(strings.TrimPrefix(key, backupIndex), "/")
		if len(parts) == 5 {
			s.pruneBackup(ctx, request.Scope{AccountID: parts[0], Region: parts[1]}, key, parts[2], parts[4])
		}
	}
}

func (s *Service) pruneBackup(ctx context.Context, scope request.Scope, index, sourceKind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snap *record
	now := s.now()
	key := scope.Key(serviceName, backupSnapshotKind(sourceKind), name)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[record](tx, key)
		if err != nil {
			return err
		}

		if !found {
			tx.Delete(index)
			return nil
		}

		live, err := backupSourceOf(r).live(tx, scope)
		if err != nil || !backupExpired(r, live, now) {
			return err
		}

		if now.Before(r.BackupRetryAt) {
			return nil
		}

		r.BackupRetryAt = now.Add(backupRetry)
		snap = r
		return writeRecord(tx, scope, r)
	})
	if err == nil && snap != nil {
		_ = s.removeBackupSnapshot(ctx, scope, snap)
	}
}

func (s *Service) removeBackupSnapshot(ctx context.Context, scope request.Scope, snap *record) error {
	if s.runtime != nil && snap.Volume != "" {
		if err := s.runtime.Remove(ctx, s.dataSpec(ctx, scope, snap), true); err != nil {
			return err
		}
	}

	source := backupSourceOf(snap)
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, snap.Kind, snap.Name))
		tx.Delete(source.prefix(scope) + snap.Name)
		live, err := source.live(tx, scope)
		if err != nil {
			return err
		}

		if live != nil {
			remaining, err := backupSnapshots(tx, scope, source)
			if err != nil {
				return err
			}

			window := restoreWindow(remaining, live, s.now())
			if live.Cluster != nil {
				live.Cluster.EarliestRestorableTime, live.Cluster.LatestRestorableTime = window.EarliestTime, window.LatestTime
			} else {
				live.Instance.LatestRestorableTime = window.LatestTime
			}

			if err := writeRecord(tx, scope, live); err != nil {
				return err
			}
		}

		return recordEvent(tx, scope, snap, eventDeletion, "Automated snapshot expired or deleted")
	})
}

func (s *Service) deleteBackupSnapshots(ctx context.Context, scope request.Scope, r *record) error {
	var snapshots []*record
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		snapshots, err = backupSnapshots(tx, scope, backupSourceOf(r))
		return err
	})
	if err != nil {
		return err
	}

	for _, snap := range snapshots {
		if err := s.removeBackupSnapshot(ctx, scope, snap); err != nil {
			return err
		}
	}

	return nil
}

// Preserve the current retention policy when the live resource is removed.
func retainBackupSnapshots(tx storage.WriteTx, scope request.Scope, r *record) error {
	source := backupSourceOf(r)
	snapshots, err := backupSnapshots(tx, scope, source)
	if err != nil {
		return err
	}

	for _, snap := range snapshots {
		if snap.Cluster != nil {
			snap.Cluster.BackupRetentionPeriod = new(source.retention)
		} else {
			snap.Instance.BackupRetentionPeriod = new(source.retention)
		}

		if err := writeRecord(tx, scope, snap); err != nil {
			return err
		}
	}

	return nil
}

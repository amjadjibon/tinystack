package rds

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maintenanceIndex         = "aws/rds/maintenance-index/"
	defaultMaintenanceWindow = "sun:05:00-sun:05:30"
	maintenanceWeek          = 7 * 24 * time.Hour
)

func maintenanceOffset(value string) (time.Duration, error) {
	if len(value) != len("mon:00:00") || value[3] != ':' {
		return 0, invalid("PreferredMaintenanceWindow must use ddd:hh:mm-ddd:hh:mm")
	}

	days := [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	for day, name := range days {
		if value[:3] != name {
			continue
		}

		clock, err := time.Parse("15:04", value[4:])
		if err == nil {
			return (time.Duration(day)*24+time.Duration(clock.Hour()))*time.Hour +
				time.Duration(clock.Minute())*time.Minute, nil
		}
	}

	return 0, invalid("PreferredMaintenanceWindow must use ddd:hh:mm-ddd:hh:mm")
}

func maintenanceWindow(value *string, now time.Time) (string, time.Time, error) {
	window := strings.ToLower(common.Value(common.OrElse(value, new(defaultMaintenanceWindow))))
	left, right, ok := strings.Cut(window, "-")
	if !ok {
		return "", time.Time{}, invalid("PreferredMaintenanceWindow must use ddd:hh:mm-ddd:hh:mm")
	}

	start, err := maintenanceOffset(left)
	if err != nil {
		return "", time.Time{}, err
	}

	end, err := maintenanceOffset(right)
	if err != nil {
		return "", time.Time{}, err
	}

	length := (end - start + maintenanceWeek) % maintenanceWeek
	if length < 30*time.Minute {
		return "", time.Time{}, invalid("PreferredMaintenanceWindow must be at least 30 minutes")
	}

	now = now.UTC()
	week := now.Truncate(24 * time.Hour).Add(-time.Duration(now.Weekday()) * 24 * time.Hour)
	due := week.Add(start)
	if due.After(now) {
		due = due.Add(-maintenanceWeek)
	}

	if !now.Before(due.Add(length)) {
		due = due.Add(maintenanceWeek)
	}

	return window, due, nil
}

func maintenanceKey(scope request.Scope, kind, name string) string {
	return maintenanceIndex + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + name
}

func writeMaintenance(tx storage.WriteTx, scope request.Scope, r *record) error {
	key := maintenanceKey(scope, r.Kind, r.Name)
	var window *string
	switch {
	case r.Instance != nil && (r.Instance.PendingModifiedValues != nil || pendingOptionGroup(r.Instance) != nil):
		window = r.Instance.PreferredMaintenanceWindow
	case r.Cluster != nil && r.Cluster.PendingModifiedValues != nil:
		window = r.Cluster.PreferredMaintenanceWindow
	default:
		r.MaintenanceAt = time.Time{}
		r.MaintenanceOptIn = nil
		tx.Delete(key)
		return nil
	}

	if r.MaintenanceAt.IsZero() {
		_, due, err := maintenanceWindow(window, time.Now())
		if err != nil {
			return err
		}

		r.MaintenanceAt = due
	}

	return storage.Write(tx, key, r.MaintenanceAt)
}

func applyInstancePending(db *api.DBInstance) {
	if p := db.PendingModifiedValues; p != nil {
		db.DBInstanceClass = common.OrElse(p.DBInstanceClass, db.DBInstanceClass)
		db.AllocatedStorage = common.OrElse(p.AllocatedStorage, db.AllocatedStorage)
		db.BackupRetentionPeriod = common.OrElse(p.BackupRetentionPeriod, db.BackupRetentionPeriod)
		db.PendingModifiedValues = nil
	}

	if group := pendingOptionGroup(db); group != nil {
		db.OptionGroupMemberships = optionMembership(*group)
	}
}

func applyClusterPending(db *api.DBCluster) {
	if p := db.PendingModifiedValues; p != nil {
		db.BackupRetentionPeriod = common.OrElse(p.BackupRetentionPeriod, db.BackupRetentionPeriod)
		db.PendingModifiedValues = nil
	}
}

// StartMaintenance starts the same scheduler for records and Docker resources.
func (s *Service) StartMaintenance() {
	if s.bg == nil {
		s.bg = background.New()
	}

	s.bg.Every(time.Second, s.visitMaintenance)
	s.bg.Every(time.Second, s.visitEventDeliveries)
	s.bg.Every(time.Second, s.visitBackups)
}

func (s *Service) visitMaintenance(ctx context.Context) {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(maintenanceIndex)
		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		parts := strings.Split(strings.TrimPrefix(key, maintenanceIndex), "/")
		if len(parts) != 4 {
			continue
		}

		s.applyMaintenance(ctx, request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2], parts[3])
	}
}

func (s *Service) applyMaintenance(ctx context.Context, scope request.Scope, kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r *record
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		due, found, err := storage.Read[time.Time](tx, maintenanceKey(scope, kind, name))
		if err != nil || !found || time.Now().Before(*due) {
			return err
		}

		r, err = readRecord(tx, scope, kind, name)
		return err
	})
	if err != nil || r == nil {
		return
	}

	_ = s.applyPendingMaintenance(ctx, scope, r)
}

func saveModification(tx storage.WriteTx, scope request.Scope, r *record, category, message string) error {
	if err := writeMaintenance(tx, scope, r); err != nil {
		return err
	}

	var err error
	if r.Kind == kindCluster {
		err = writeCluster(tx, scope, r)
	} else {
		err = writeRecord(tx, scope, r)
	}

	if err != nil {
		return err
	}

	return recordEvent(tx, scope, r, category, message)
}

package rds

import (
	"errors"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestRuntimeAutomatedBackupRetryRestoreAndRetention(t *testing.T) {
	rt := &fakeRuntime{ready: true}
	s := New(&memory.Store{})
	s.runtime = rt
	now := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := runtimeContext()
	runtimeCreate(t, s, "backup")
	s.visit(ctx)

	s.visitBackups(ctx)
	if len(rt.copies) != 0 {
		t.Fatal("copied outside the backup window")
	}

	now = now.Add(time.Hour)
	rt.copyErr = errors.New("copy failed")
	s.visitBackups(ctx)
	rt.copyErr = nil
	s.visitBackups(ctx)
	if len(rt.copies) != 1 {
		t.Fatal("failed copy retried without delay")
	}

	now = now.Add(backupRetry)
	s.visitBackups(ctx)
	s.visitBackups(ctx)
	if len(rt.copies) != 2 {
		t.Fatal("successful copy repeated in the same window")
	}

	firstCopy := rt.copies[1]
	if firstCopy[0].Name == "" || firstCopy[1].Name != "" || firstCopy[0].Volume == firstCopy[1].Volume {
		t.Fatal("did not copy the live database into a dedicated snapshot volume")
	}

	// A longer retention policy keeps two restore points. The older point should be used for this request.
	if _, err := s.ModifyDBInstance(ctx, &api.ModifyDBInstanceMessage{
		DBInstanceIdentifier: new("backup"), BackupRetentionPeriod: new(int64(2)), ApplyImmediately: new(true),
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(time.Minute)
	now = now.Add(backupDay)
	s.visitBackups(ctx)
	latestCopy := rt.copies[2]
	if firstCopy[1].Volume == latestCopy[1].Volume {
		t.Fatal("backup overwritten on the following day")
	}

	if _, err := s.RestoreDBInstanceToPointInTime(ctx, &api.RestoreDBInstanceToPointInTimeMessage{
		SourceDBInstanceIdentifier: new("backup"), TargetDBInstanceIdentifier: new("restore"), RestoreTime: new(cutoff),
	}); err != nil {
		t.Fatal(err)
	}

	restored := rt.copies[3]
	if restored[0].Volume != firstCopy[1].Volume || restored[1].Volume == restored[0].Volume {
		t.Fatal("restore read the wrong snapshot volume")
	}

	now = now.Add(backupDay + time.Hour)
	s.visitBackups(ctx)
	if len(rt.removedSpecs) != 1 || rt.removedSpecs[0].Volume != firstCopy[1].Volume || !rt.removed[0] {
		t.Fatal("only the expired automated volume should be removed", rt.removedSpecs)
	}

	db, err := s.DescribeDBInstances(ctx, &api.DescribeDBInstancesMessage{DBInstanceIdentifier: new("backup")})
	if err != nil || !common.Value(db.DBInstances[0].LatestRestorableTime).Equal(cutoff.Add(backupDay-time.Minute)) {
		t.Fatal(db, err)
	}
}

func TestRuntimeAutomatedAuroraBackupAndRestore(t *testing.T) {
	rt := &fakeRuntime{ready: true}
	s := New(&memory.Store{})
	s.runtime = rt
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := runtimeContext()
	in := clusterInput("aurora-backup", EngineAuroraPostgres)
	in.PreferredBackupWindow = new("23:45-00:15")
	in.MasterUserPassword = new("password123")
	in.MasterUsername = new("master")
	if _, err := s.CreateDBCluster(ctx, in); err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	s.visitBackups(ctx)
	if len(rt.copies) != 1 || rt.copies[0][0].Name == "" || rt.copies[0][1].Name != "" {
		t.Fatal("the overnight window did not copy a cluster snapshot", rt.copies)
	}

	out, err := s.RestoreDBClusterToPointInTime(ctx, &api.RestoreDBClusterToPointInTimeMessage{
		SourceDBClusterIdentifier: in.DBClusterIdentifier, DBClusterIdentifier: new("aurora-restore"),
		UseLatestRestorableTime: new(true),
	})
	if err != nil || common.Value(out.DBCluster.Status) != creating || out.DBCluster.LatestRestorableTime != nil {
		t.Fatal(out, err)
	}

	if len(rt.copies) != 2 || rt.copies[1][0].Volume != rt.copies[0][1].Volume {
		t.Fatal("Aurora restore did not use its automated snapshot volume", rt.copies)
	}
}

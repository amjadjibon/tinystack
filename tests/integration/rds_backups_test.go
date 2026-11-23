package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func activeRDSBackupWindow() *string {
	now := time.Now().UTC()
	return new(now.Add(-time.Minute).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04"))
}

func TestRDSAutomatedInstanceBackups(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := t.Context()
	in := rdsInput("backup-source")
	in.PreferredBackupWindow = activeRDSBackupWindow()
	in.BackupRetentionPeriod = new(int32(2))
	db, err := c.CreateDBInstance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	var backup types.DBInstanceAutomatedBackup
	waitFor(t, "automated instance backup", func() bool {
		out, describeErr := c.DescribeDBInstanceAutomatedBackups(ctx, &rds.DescribeDBInstanceAutomatedBackupsInput{
			DbiResourceId: db.DBInstance.DbiResourceId,
		})
		if describeErr != nil || len(out.DBInstanceAutomatedBackups) != 1 {
			t.Fatal(out, describeErr)
		}

		backup = out.DBInstanceAutomatedBackups[0]
		return backup.RestoreWindow != nil && aws.ToString(backup.Status) == "active"
	})

	latest := backup.RestoreWindow.LatestTime
	snapshots, err := c.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{
		SnapshotType: new("automated"), DBInstanceIdentifier: in.DBInstanceIdentifier,
	})
	if err != nil || len(snapshots.DBSnapshots) != 1 || !snapshots.DBSnapshots[0].SnapshotCreateTime.Equal(*latest) {
		t.Fatal(snapshots, err)
	}

	described, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier,
	})
	if err != nil || !latest.Equal(aws.ToTime(described.DBInstances[0].LatestRestorableTime)) {
		t.Fatal(described, err)
	}

	_, err = c.DeleteDBInstanceAutomatedBackup(ctx, &rds.DeleteDBInstanceAutomatedBackupInput{
		DbiResourceId: backup.DbiResourceId,
	})
	code(t, err, "InvalidDBInstanceAutomatedBackupState")

	restore := &rds.RestoreDBInstanceToPointInTimeInput{
		SourceDBInstanceIdentifier: in.DBInstanceIdentifier, TargetDBInstanceIdentifier: new("backup-target"),
		RestoreTime: new(latest.Add(-time.Minute)), BackupRetentionPeriod: new(int32(0)),
	}
	_, err = c.RestoreDBInstanceToPointInTime(ctx, restore)
	code(t, err, "InvalidRestoreFault")
	if !strings.Contains(err.Error(), "without transaction log replay") {
		t.Fatal(err)
	}

	restore.RestoreTime = latest
	restored, err := c.RestoreDBInstanceToPointInTime(ctx, restore)
	if err != nil || aws.ToString(restored.DBInstance.Engine) != "postgres" ||
		aws.ToString(restored.DBInstance.DbiResourceId) == aws.ToString(backup.DbiResourceId) ||
		restored.DBInstance.LatestRestorableTime != nil {
		t.Fatal(restored, err)
	}

	_, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier, SkipFinalSnapshot: new(true), DeleteAutomatedBackups: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	retained, err := c.DescribeDBInstanceAutomatedBackups(ctx, &rds.DescribeDBInstanceAutomatedBackupsInput{
		Filters: []types.Filter{{Name: new("status"), Values: []string{"retained"}}},
	})
	if err != nil || len(retained.DBInstanceAutomatedBackups) != 1 ||
		aws.ToString(retained.DBInstanceAutomatedBackups[0].DbiResourceId) != *backup.DbiResourceId {
		t.Fatal(retained, err)
	}

	restored, err = c.RestoreDBInstanceToPointInTime(ctx, &rds.RestoreDBInstanceToPointInTimeInput{
		SourceDBInstanceAutomatedBackupsArn: backup.DBInstanceAutomatedBackupsArn,
		TargetDBInstanceIdentifier:          new("retained-target"), UseLatestRestorableTime: new(true),
	})
	if err != nil || aws.ToString(restored.DBInstance.DBInstanceIdentifier) != "retained-target" {
		t.Fatal(restored, err)
	}

	if _, deleteErr := c.DeleteDBInstanceAutomatedBackup(ctx, &rds.DeleteDBInstanceAutomatedBackupInput{
		DBInstanceAutomatedBackupsArn: backup.DBInstanceAutomatedBackupsArn,
	}); deleteErr != nil {
		t.Fatal(deleteErr)
	}

	_, err = c.DescribeDBInstanceAutomatedBackups(ctx, &rds.DescribeDBInstanceAutomatedBackupsInput{
		DbiResourceId: backup.DbiResourceId,
	})
	code(t, err, "DBInstanceAutomatedBackupNotFound")
}

func TestRDSAutomatedClusterBackups(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := t.Context()
	in := auroraInput("cluster-backup-source", "aurora-postgresql")
	in.PreferredBackupWindow = activeRDSBackupWindow()
	if _, err := c.CreateDBCluster(ctx, in); err != nil {
		t.Fatal(err)
	}

	var cluster types.DBCluster
	waitFor(t, "Aurora restore window", func() bool {
		out, err := c.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: in.DBClusterIdentifier})
		if err != nil || len(out.DBClusters) != 1 {
			t.Fatal(out, err)
		}

		cluster = out.DBClusters[0]
		return cluster.LatestRestorableTime != nil && cluster.EarliestRestorableTime != nil
	})

	snapshots, err := c.DescribeDBClusterSnapshots(ctx, &rds.DescribeDBClusterSnapshotsInput{
		DBClusterIdentifier: in.DBClusterIdentifier, SnapshotType: new("automated"),
	})
	if err != nil || len(snapshots.DBClusterSnapshots) != 1 ||
		!snapshots.DBClusterSnapshots[0].SnapshotCreateTime.Equal(*cluster.LatestRestorableTime) {
		t.Fatal(snapshots, err)
	}

	target, err := c.RestoreDBClusterToPointInTime(ctx, &rds.RestoreDBClusterToPointInTimeInput{
		SourceDbClusterResourceId: cluster.DbClusterResourceId, DBClusterIdentifier: new("cluster-backup-target"),
		RestoreType: new("full-copy"), UseLatestRestorableTime: new(true),
	})
	if err != nil || aws.ToString(target.DBCluster.EngineVersion) != aws.ToString(cluster.EngineVersion) ||
		len(target.DBCluster.DBClusterMembers) != 0 || target.DBCluster.LatestRestorableTime != nil {
		t.Fatal(target, err)
	}
}

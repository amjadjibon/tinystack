package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func waitRDSClusterSnapshot(t *testing.T, c *rds.Client, id string) types.DBClusterSnapshot {
	t.Helper()
	var snap types.DBClusterSnapshot
	waitFor(t, "available DB cluster snapshot", func() bool {
		out, err := c.DescribeDBClusterSnapshots(t.Context(), &rds.DescribeDBClusterSnapshotsInput{
			DBClusterSnapshotIdentifier: new(id),
		})
		if err != nil || len(out.DBClusterSnapshots) != 1 {
			t.Fatal(out, err)
		}

		snap = out.DBClusterSnapshots[0]
		return aws.ToString(snap.Status) == "available" && aws.ToInt32(snap.PercentProgress) == 100
	})
	return snap
}

func TestRDSClusterSnapshots(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	for _, tc := range []struct {
		engine, family string
		port           int32
	}{
		{"aurora-postgresql", "aurora-postgresql17", 5441},
		{"docdb", "docdb5.0", 27018},
		{"neptune", "neptune1.3", 8183},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			source, backup, copied, target, final := tc.engine+"-source", tc.engine+"-backup",
				tc.engine+"-copy", tc.engine+"-restored", tc.engine+"-final"
			in := &rds.CreateDBClusterInput{
				DBClusterIdentifier: new(source), Engine: new(tc.engine), Port: new(tc.port),
				StorageEncrypted: new(true), KmsKeyId: new("alias/test-key"),
			}
			if tc.engine != "neptune" {
				in.MasterUsername, in.MasterUserPassword = new("master"), new("password123")
			}

			if _, err := c.CreateDBCluster(ctx, in); err != nil {
				t.Fatal(err)
			}

			original := waitAurora(t, ctx, c, source)
			created, err := c.CreateDBClusterSnapshot(ctx, &rds.CreateDBClusterSnapshotInput{
				DBClusterIdentifier: original.DBClusterArn, DBClusterSnapshotIdentifier: new(backup),
				Tags: []types.Tag{{Key: new("stage"), Value: new("original")}},
			})
			if err != nil || aws.ToString(created.DBClusterSnapshot.Status) != "creating" {
				t.Fatal(created, err)
			}

			snap := waitRDSClusterSnapshot(t, c, backup)
			filtered, err := c.DescribeDBClusterSnapshots(ctx, &rds.DescribeDBClusterSnapshotsInput{
				DBClusterIdentifier: original.DBClusterArn, SnapshotType: new("manual"),
			})
			if err != nil || len(filtered.DBClusterSnapshots) != 1 ||
				aws.ToInt32(snap.Port) != tc.port || !aws.ToBool(snap.StorageEncrypted) {
				t.Fatal(filtered, snap, err)
			}

			share := &rds.ModifyDBClusterSnapshotAttributeInput{
				DBClusterSnapshotIdentifier: snap.DBClusterSnapshotArn,
				AttributeName:               new("restore"), ValuesToAdd: []string{"all"},
			}
			_, err = c.ModifyDBClusterSnapshotAttribute(ctx, share)
			code(t, err, "InvalidParameterValue")
			share.ValuesToAdd = []string{"123456789012"}
			shared, err := c.ModifyDBClusterSnapshotAttribute(ctx, share)
			if err != nil || !slices.Equal(shared.DBClusterSnapshotAttributesResult.
				DBClusterSnapshotAttributes[0].AttributeValues, share.ValuesToAdd) {
				t.Fatal(shared, err)
			}

			restore := &rds.RestoreDBClusterFromSnapshotInput{
				DBClusterIdentifier: new(target), SnapshotIdentifier: snap.DBClusterSnapshotArn,
				Engine: new(tc.engine), EngineVersion: new("invalid"),
			}
			_, err = c.RestoreDBClusterFromSnapshot(ctx, restore)
			code(t, err, "InvalidRestoreFault")

			copyIn := &rds.CopyDBClusterSnapshotInput{
				SourceDBClusterSnapshotIdentifier: snap.DBClusterSnapshotArn,
				TargetDBClusterSnapshotIdentifier: new(copied),
				CopyTags:                          new(true),
				Tags:                              []types.Tag{{Key: new("stage"), Value: new("copy")}},
			}
			copyIn.KmsKeyId = new("alias/unsupported")
			_, err = c.CopyDBClusterSnapshot(ctx, copyIn)
			code(t, err, "InvalidParameterValue")
			copyIn.KmsKeyId = nil
			if _, err = c.CopyDBClusterSnapshot(ctx, copyIn); err != nil {
				t.Fatal(err)
			}

			_, err = c.CopyDBClusterSnapshot(ctx, copyIn)
			code(t, err, "DBClusterSnapshotAlreadyExistsFault")
			copySnap := waitRDSClusterSnapshot(t, c, copied)
			if aws.ToString(copySnap.SourceDBClusterSnapshotArn) != aws.ToString(snap.DBClusterSnapshotArn) ||
				len(copySnap.TagList) != 1 || aws.ToString(copySnap.TagList[0].Value) != "copy" {
				t.Fatal(copySnap)
			}

			attrs, err := c.DescribeDBClusterSnapshotAttributes(ctx, &rds.DescribeDBClusterSnapshotAttributesInput{
				DBClusterSnapshotIdentifier: new(copied),
			})
			if err != nil ||
				len(attrs.DBClusterSnapshotAttributesResult.DBClusterSnapshotAttributes[0].AttributeValues) != 0 {
				t.Fatal(attrs, err)
			}

			del := &rds.DeleteDBClusterInput{DBClusterIdentifier: new(source), FinalDBSnapshotIdentifier: new(backup)}
			_, err = c.DeleteDBCluster(ctx, del)
			code(t, err, "DBClusterSnapshotAlreadyExistsFault")
			del.FinalDBSnapshotIdentifier = new(final)
			if _, err = c.DeleteDBCluster(ctx, del); err != nil {
				t.Fatal(err)
			}

			waitRDSClusterSnapshot(t, c, final)
			other := rdsClient(s, "111111111111", "us-east-1")
			_, err = other.RestoreDBClusterFromSnapshot(ctx, restore)
			code(t, err, "InvalidParameterValue")
			restore.SnapshotIdentifier = copySnap.DBClusterSnapshotArn
			restore.EngineVersion = snap.EngineVersion
			restore.BackupRetentionPeriod = new(int32(5))
			restore.DBClusterParameterGroupName = new("missing-group")
			_, err = c.RestoreDBClusterFromSnapshot(ctx, restore)
			code(t, err, "DBClusterParameterGroupNotFound")
			restore.DBClusterParameterGroupName = nil
			if _, err = c.RestoreDBClusterFromSnapshot(ctx, restore); err != nil {
				t.Fatal(err)
			}

			restored := waitAurora(t, ctx, c, target)
			if aws.ToString(restored.DbClusterResourceId) == aws.ToString(original.DbClusterResourceId) ||
				aws.ToString(restored.Endpoint) == aws.ToString(original.Endpoint) ||
				aws.ToString(restored.KmsKeyId) != "alias/test-key" || len(restored.DBClusterMembers) != 0 ||
				aws.ToInt32(restored.Port) != tc.port || aws.ToInt32(restored.BackupRetentionPeriod) != 5 ||
				aws.ToString(restored.DBClusterParameterGroup) != "default."+tc.family {
				t.Fatal(restored)
			}

			for _, id := range []string{backup, copied, final} {
				removed, deleteErr := c.DeleteDBClusterSnapshot(ctx, &rds.DeleteDBClusterSnapshotInput{
					DBClusterSnapshotIdentifier: new(id),
				})
				if deleteErr != nil || aws.ToString(removed.DBClusterSnapshot.Status) != "deleted" {
					t.Fatal(removed, deleteErr)
				}
			}

			_, err = c.DescribeDBClusterSnapshots(ctx, &rds.DescribeDBClusterSnapshotsInput{
				DBClusterSnapshotIdentifier: new(copied),
			})
			code(t, err, "DBClusterSnapshotNotFoundFault")
		})
	}
}

func TestRDSInstanceSnapshotAttributes(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := t.Context()
	const id = "attribute-test"
	if _, err := c.CreateDBInstance(ctx, rdsInput(id)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new(id), DBSnapshotIdentifier: new(id),
	}); err != nil {
		t.Fatal(err)
	}

	snap := waitRDSSnapshot(t, c, id)
	modify := &rds.ModifyDBSnapshotAttributeInput{
		DBSnapshotIdentifier: snap.DBSnapshotArn, AttributeName: new("restore"),
		ValuesToAdd: []string{"all", "123456789012", "234567890123", "123456789012"},
	}
	out, err := c.ModifyDBSnapshotAttribute(ctx, modify)
	if err != nil || !slices.Equal(out.DBSnapshotAttributesResult.DBSnapshotAttributes[0].AttributeValues,
		[]string{"123456789012", "234567890123", "all"}) {
		t.Fatal(out, err)
	}

	modify.ValuesToAdd, modify.ValuesToRemove = []string{"invalid"}, []string{"all", "123456789012"}
	_, err = c.ModifyDBSnapshotAttribute(ctx, modify)
	code(t, err, "InvalidParameterValue")
	modify.ValuesToAdd = nil
	if _, err = c.ModifyDBSnapshotAttribute(ctx, modify); err != nil {
		t.Fatal(err)
	}

	describe := &rds.DescribeDBSnapshotAttributesInput{DBSnapshotIdentifier: new(id)}
	attrs, err := c.DescribeDBSnapshotAttributes(ctx, describe)
	if err != nil || !slices.Equal(attrs.DBSnapshotAttributesResult.DBSnapshotAttributes[0].AttributeValues,
		[]string{"234567890123"}) {
		t.Fatal(attrs, err)
	}

	other := rdsClient(s, "111111111111", "us-east-1")
	_, err = other.DescribeDBSnapshotAttributes(ctx, describe)
	code(t, err, "DBSnapshotNotFound")
	if _, err = c.CopyDBSnapshot(ctx, &rds.CopyDBSnapshotInput{
		SourceDBSnapshotIdentifier: new(id), TargetDBSnapshotIdentifier: new("attribute-copy"),
	}); err != nil {
		t.Fatal(err)
	}

	describe.DBSnapshotIdentifier = new("attribute-copy")
	attrs, err = c.DescribeDBSnapshotAttributes(ctx, describe)
	if err != nil || len(attrs.DBSnapshotAttributesResult.DBSnapshotAttributes[0].AttributeValues) != 0 {
		t.Fatal(attrs, err)
	}
}

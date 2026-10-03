package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBBackups(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := create(ctx, c, "orders"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"a", "b"} {
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: key(id)}); err != nil {
			t.Fatal(err)
		}
	}

	created, err := c.CreateBackup(ctx, &sdk.CreateBackupInput{
		TableName: aws.String("orders"), BackupName: aws.String("nightly"),
	})
	if err != nil || created.BackupDetails.BackupStatus != types.BackupStatusAvailable {
		t.Fatalf("created: %+v %v", created, err)
	}

	arn := created.BackupDetails.BackupArn
	// The backup keeps the table as it was.
	if _, err = c.DeleteItem(ctx, &sdk.DeleteItemInput{TableName: aws.String("orders"), Key: key("a")}); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeBackup(ctx, &sdk.DescribeBackupInput{BackupArn: arn})
	if err != nil || aws.ToInt64(described.BackupDescription.SourceTableDetails.ItemCount) != 2 ||
		described.BackupDescription.SourceTableDetails.BillingMode != types.BillingModePayPerRequest {
		t.Fatalf("described: %+v %v", described, err)
	}

	listed, err := c.ListBackups(ctx, &sdk.ListBackupsInput{TableName: aws.String("orders")})
	if err != nil || len(listed.BackupSummaries) != 1 || aws.ToString(listed.BackupSummaries[0].BackupArn) != *arn {
		t.Fatalf("listed: %+v %v", listed, err)
	}

	restored, err := c.RestoreTableFromBackup(ctx, &sdk.RestoreTableFromBackupInput{
		TargetTableName: aws.String("orders-restored"), BackupArn: arn,
	})
	if err != nil || aws.ToString(restored.TableDescription.RestoreSummary.SourceBackupArn) != *arn {
		t.Fatalf("restored: %+v %v", restored, err)
	}

	scanned, err := c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("orders-restored")})
	if err != nil || scanned.Count != 2 {
		t.Fatalf("restored items: %+v %v", scanned, err)
	}

	_, err = c.RestoreTableFromBackup(ctx, &sdk.RestoreTableFromBackupInput{
		TargetTableName: aws.String("orders"), BackupArn: arn,
	})
	code(t, err, "TableAlreadyExistsException")

	_, err = c.RestoreTableToPointInTime(ctx, &sdk.RestoreTableToPointInTimeInput{
		SourceTableName: aws.String("orders"), TargetTableName: aws.String("orders-now"),
		UseLatestRestorableTime: aws.Bool(true),
	})
	code(t, err, "PointInTimeRecoveryUnavailableException")

	updated, err := c.UpdateContinuousBackups(ctx, &sdk.UpdateContinuousBackupsInput{
		TableName: aws.String("orders"),
		PointInTimeRecoverySpecification: &types.PointInTimeRecoverySpecification{
			PointInTimeRecoveryEnabled: aws.Bool(true),
		},
	})
	pitr := updated.ContinuousBackupsDescription.PointInTimeRecoveryDescription
	if err != nil || pitr.PointInTimeRecoveryStatus != types.PointInTimeRecoveryStatusEnabled ||
		aws.ToInt32(pitr.RecoveryPeriodInDays) != 35 {
		t.Fatalf("updated: %+v %v", updated, err)
	}

	if _, err = c.RestoreTableToPointInTime(ctx, &sdk.RestoreTableToPointInTimeInput{
		SourceTableName: aws.String("orders"), TargetTableName: aws.String("orders-now"),
		UseLatestRestorableTime: aws.Bool(true), BillingModeOverride: types.BillingModeProvisioned,
		ProvisionedThroughputOverride: &types.ProvisionedThroughput{
			ReadCapacityUnits: aws.Int64(5), WriteCapacityUnits: aws.Int64(5),
		},
	}); err != nil {
		t.Fatal(err)
	}

	now, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("orders-now")})
	if err != nil || aws.ToInt64(now.Table.ItemCount) != 1 ||
		now.Table.BillingModeSummary.BillingMode != types.BillingModeProvisioned {
		t.Fatalf("point in time: %+v %v", now, err)
	}

	if _, err = c.DeleteBackup(ctx, &sdk.DeleteBackupInput{BackupArn: arn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeBackup(ctx, &sdk.DescribeBackupInput{BackupArn: arn})
	code(t, err, "BackupNotFoundException")
}

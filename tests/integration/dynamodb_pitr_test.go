package integration_test

import (
	"context"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestDynamoDBHistoricalRecovery(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	objects := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	created, err := create(ctx, c, "history-source")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("history-exports")}); err != nil {
		t.Fatal(err)
	}

	enabled, err := c.UpdateContinuousBackups(ctx, &sdk.UpdateContinuousBackupsInput{
		TableName: new("history-source"),
		PointInTimeRecoverySpecification: &types.PointInTimeRecoverySpecification{
			PointInTimeRecoveryEnabled: new(true), RecoveryPeriodInDays: new(int32(1)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	earliest := *enabled.ContinuousBackupsDescription.PointInTimeRecoveryDescription.EarliestRestorableDateTime
	marker := func(after time.Time) time.Time {
		t.Helper()
		var at time.Time
		waitFor(t, "a later restorable timestamp", func() bool {
			reply, describeErr := c.DescribeContinuousBackups(ctx, &sdk.DescribeContinuousBackupsInput{
				TableName: new("history-source"),
			})
			if describeErr != nil {
				t.Fatal(describeErr)
			}

			at = *reply.ContinuousBackupsDescription.PointInTimeRecoveryDescription.LatestRestorableDateTime
			return at.After(after)
		})

		return at
	}

	putAt := marker(earliest)
	initial := key("order")
	initial["state"] = &types.AttributeValueMemberS{Value: "open"}
	if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: new("history-source"), Item: initial}); err != nil {
		t.Fatal(err)
	}

	open := marker(putAt)
	_ = marker(open)
	if _, err = c.UpdateItem(ctx, &sdk.UpdateItemInput{
		TableName:                new("history-source"),
		Key:                      key("order"),
		UpdateExpression:         new("SET #s = :value"),
		ExpressionAttributeNames: map[string]string{"#s": "state"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":value": &types.AttributeValueMemberS{Value: "closed"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	closed := marker(open)
	_ = marker(closed)
	if _, err = c.DeleteItem(
		ctx,
		&sdk.DeleteItemInput{TableName: new("history-source"), Key: key("order")},
	); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		at          *time.Time
		name, state string
	}{
		{&earliest, "history-empty", ""},
		{&open, "history-open", "open"},
		{&closed, "history-closed", "closed"},
		{nil, "history-latest", ""},
	} {
		restored, restoreErr := c.RestoreTableToPointInTime(ctx, &sdk.RestoreTableToPointInTimeInput{
			SourceTableName: new("history-source"), TargetTableName: &tc.name,
			RestoreDateTime: tc.at, UseLatestRestorableTime: new(tc.at == nil),
		})
		if restoreErr != nil {
			t.Fatal(restoreErr)
		}

		r := restored.TableDescription.RestoreSummary
		if tc.at != nil && !r.RestoreDateTime.Equal(*tc.at) {
			t.Fatalf("restore date = %v, want %v", r.RestoreDateTime, tc.at)
		}

		item, getErr := c.GetItem(ctx, &sdk.GetItemInput{TableName: &tc.name, Key: key("order")})
		if getErr != nil {
			t.Fatal(getErr)
		}

		if tc.state == "" {
			if len(item.Item) != 0 || *restored.TableDescription.ItemCount != 0 {
				t.Fatalf("%s should be empty: %+v", tc.name, item)
			}
		} else if state, ok := item.Item["state"].(*types.AttributeValueMemberS); !ok || state.Value != tc.state ||
			*restored.TableDescription.ItemCount != 1 {
			t.Fatalf("%s: got %+v, want state %q", tc.name, item.Item, tc.state)
		}
	}

	if _, err = c.RestoreTableToPointInTime(ctx, &sdk.RestoreTableToPointInTimeInput{
		SourceTableName: new("history-source"), TargetTableName: new("history-before"),
		RestoreDateTime: new(earliest.Add(-time.Second)),
	}); err == nil {
		t.Fatal("accepted restore before enablement")
	}

	code(t, err, "InvalidRestoreTimeException")

	exported, err := c.ExportTableToPointInTime(ctx, &sdk.ExportTableToPointInTimeInput{
		TableArn: created.TableDescription.TableArn, S3Bucket: new("history-exports"), ExportTime: &open,
	})
	if err != nil {
		t.Fatal(err)
	}

	d := exported.ExportDescription
	if d.ExportStatus != types.ExportStatusCompleted || !d.ExportTime.Equal(open) || *d.ItemCount != 1 {
		t.Fatalf("historical export: %+v", d)
	}
}

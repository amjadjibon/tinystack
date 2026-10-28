package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/timestreamwrite"
	"github.com/aws/aws-sdk-go-v2/service/timestreamwrite/types"
	"github.com/aws/smithy-go"
)

func timestreamWriteClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantTimestreamError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestTimestreamWriteSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := timestreamWriteClient(s, "test", "us-east-1")

	db, err := c.CreateDatabase(ctx, &sdk.CreateDatabaseInput{
		DatabaseName: aws.String("metrics"), Tags: []types.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || aws.ToString(db.Database.KmsKeyId) == "" || db.Database.TableCount != 0 {
		t.Fatal(db, err)
	}

	_, err = c.CreateDatabase(ctx, &sdk.CreateDatabaseInput{DatabaseName: aws.String("metrics")})
	wantTimestreamError(t, err, "ConflictException")
	_, err = c.CreateDatabase(ctx, &sdk.CreateDatabaseInput{DatabaseName: aws.String("x")})
	wantTimestreamError(t, err, "ValidationException")

	updated, err := c.UpdateDatabase(ctx, &sdk.UpdateDatabaseInput{
		DatabaseName: aws.String("metrics"), KmsKeyId: aws.String("alias/custom"),
	})
	if err != nil || aws.ToString(updated.Database.KmsKeyId) != "alias/custom" {
		t.Fatal(updated, err)
	}

	table, err := c.CreateTable(ctx, &sdk.CreateTableInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("cpu"),
		RetentionProperties: &types.RetentionProperties{
			MemoryStoreRetentionPeriodInHours: aws.Int64(24), MagneticStoreRetentionPeriodInDays: aws.Int64(30),
		},
		MagneticStoreWriteProperties: &types.MagneticStoreWriteProperties{EnableMagneticStoreWrites: aws.Bool(true)},
		Schema: &types.Schema{CompositePartitionKey: []types.PartitionKey{
			{
				Type: types.PartitionKeyTypeDimension, Name: aws.String("host"),
				EnforcementInRecord: types.PartitionKeyEnforcementLevelRequired,
			},
		}},
	})
	if err != nil || aws.ToInt64(table.Table.RetentionProperties.MemoryStoreRetentionPeriodInHours) != 24 ||
		table.Table.TableStatus != types.TableStatusActive {
		t.Fatal(table, err)
	}

	_, err = c.CreateTable(ctx, &sdk.CreateTableInput{
		DatabaseName: aws.String("missing"), TableName: aws.String("cpu"),
	})
	wantTimestreamError(t, err, "ResourceNotFoundException")

	// A table with defaults.
	plain, err := c.CreateTable(
		ctx,
		&sdk.CreateTableInput{DatabaseName: aws.String("metrics"), TableName: aws.String("mem")},
	)
	if err != nil || aws.ToInt64(plain.Table.RetentionProperties.MemoryStoreRetentionPeriodInHours) != 6 ||
		aws.ToInt64(plain.Table.RetentionProperties.MagneticStoreRetentionPeriodInDays) != 73000 {
		t.Fatal(plain, err)
	}

	described, err := c.DescribeDatabase(ctx, &sdk.DescribeDatabaseInput{DatabaseName: aws.String("metrics")})
	if err != nil || described.Database.TableCount != 2 {
		t.Fatal(described, err)
	}

	retained, err := c.UpdateTable(ctx, &sdk.UpdateTableInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("mem"),
		RetentionProperties: &types.RetentionProperties{
			MemoryStoreRetentionPeriodInHours: aws.Int64(12), MagneticStoreRetentionPeriodInDays: aws.Int64(90),
		},
	})
	if err != nil || aws.ToInt64(retained.Table.RetentionProperties.MemoryStoreRetentionPeriodInHours) != 12 {
		t.Fatal(retained, err)
	}

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("mem"),
		RetentionProperties: &types.RetentionProperties{
			MemoryStoreRetentionPeriodInHours: aws.Int64(0), MagneticStoreRetentionPeriodInDays: aws.Int64(1),
		},
	})
	wantTimestreamError(t, err, "ValidationException")

	// Writes go through endpoint discovery and are validated, counted and stored.
	now := time.Now()
	written, err := c.WriteRecords(ctx, &sdk.WriteRecordsInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("cpu"),
		CommonAttributes: &types.Record{
			Dimensions:       []types.Dimension{{Name: aws.String("host"), Value: aws.String("a")}},
			MeasureValueType: types.MeasureValueTypeDouble,
			Time:             aws.String(strconv.FormatInt(now.UnixMilli(), 10)),
		},
		Records: []types.Record{
			{MeasureName: aws.String("cpu"), MeasureValue: aws.String("0.5")},
			{MeasureName: aws.String("cpu2"), MeasureValue: aws.String("0.7")},
			{
				MeasureName:  aws.String("old"),
				MeasureValue: aws.String("1"),
				Time: aws.String(
					strconv.FormatInt(now.Add(-10*24*time.Hour).Unix(), 10),
				),
				TimeUnit: types.TimeUnitSeconds,
			},
		},
	})
	if err != nil || written.RecordsIngested.Total != 3 ||
		written.RecordsIngested.MemoryStore != 2 || written.RecordsIngested.MagneticStore != 1 {
		t.Fatal(written, err)
	}

	// Records outside every retention window are rejected with their indexes.
	_, err = c.WriteRecords(ctx, &sdk.WriteRecordsInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("mem"),
		Records: []types.Record{
			{
				MeasureName: aws.String(
					"ok",
				),
				MeasureValue:     aws.String("1"),
				MeasureValueType: types.MeasureValueTypeBigint,
				Time:             aws.String(strconv.FormatInt(now.UnixMilli(), 10)),
			},
			{
				MeasureName: aws.String(
					"stale",
				),
				MeasureValue:     aws.String("1"),
				MeasureValueType: types.MeasureValueTypeBigint,
				Time:             aws.String(strconv.FormatInt(now.Add(-48*time.Hour).UnixMilli(), 10)),
			},
		},
	})
	var rejected *types.RejectedRecordsException
	if !errors.As(err, &rejected) || len(rejected.RejectedRecords) != 1 ||
		rejected.RejectedRecords[0].RecordIndex != 1 ||
		aws.ToString(rejected.RejectedRecords[0].Reason) == "" {
		t.Fatalf("want a rejected record at index 1, got %v", err)
	}

	// Malformed records fail the whole request.
	_, err = c.WriteRecords(ctx, &sdk.WriteRecordsInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("cpu"),
		Records: []types.Record{
			{
				MeasureName: aws.String(
					"cpu",
				),
				MeasureValue:     aws.String("fast"),
				MeasureValueType: types.MeasureValueTypeDouble,
				Time:             aws.String(strconv.FormatInt(now.UnixMilli(), 10)),
			},
		},
	})
	wantTimestreamError(t, err, "ValidationException")
	_, err = c.WriteRecords(ctx, &sdk.WriteRecordsInput{
		DatabaseName: aws.String("metrics"), TableName: aws.String("nope"),
		Records: []types.Record{{
			MeasureName: aws.String("a"), MeasureValue: aws.String("1"), MeasureValueType: types.MeasureValueTypeBigint,
			Time: aws.String(strconv.FormatInt(now.UnixMilli(), 10)),
		}},
	})
	wantTimestreamError(t, err, "ResourceNotFoundException")

	// Listing and pagination.
	tables, err := c.ListTables(
		ctx,
		&sdk.ListTablesInput{DatabaseName: aws.String("metrics"), MaxResults: aws.Int32(1)},
	)
	if err != nil || len(tables.Tables) != 1 || tables.NextToken == nil {
		t.Fatal(tables, err)
	}

	allTables, err := c.ListTables(ctx, &sdk.ListTablesInput{})
	if err != nil || len(allTables.Tables) != 2 {
		t.Fatal(allTables, err)
	}

	dbs, err := c.ListDatabases(ctx, &sdk.ListDatabasesInput{})
	if err != nil || len(dbs.Databases) != 1 {
		t.Fatal(dbs, err)
	}

	// Tags on a database and a table, also through the Resource Groups Tagging API.
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceARN: table.Table.Arn, Tags: []types.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(db.Database.Arn), aws.ToString(table.Table.Arn)},
		Tags:            map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: table.Table.Arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: table.Table.Arn, TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}

	// Another account sees none of it.
	other := timestreamWriteClient(s, "team-b", "us-east-1")
	_, err = other.DescribeDatabase(ctx, &sdk.DescribeDatabaseInput{DatabaseName: aws.String("metrics")})
	wantTimestreamError(t, err, "ResourceNotFoundException")

	// A database with tables cannot be deleted.
	_, err = c.DeleteDatabase(ctx, &sdk.DeleteDatabaseInput{DatabaseName: aws.String("metrics")})
	wantTimestreamError(t, err, "ValidationException")

	for _, name := range []string{"cpu", "mem"} {
		if _, err = c.DeleteTable(ctx, &sdk.DeleteTableInput{
			DatabaseName: aws.String("metrics"), TableName: aws.String(name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteDatabase(ctx, &sdk.DeleteDatabaseInput{DatabaseName: aws.String("metrics")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeTable(
		ctx,
		&sdk.DescribeTableInput{DatabaseName: aws.String("metrics"), TableName: aws.String("cpu")},
	)
	wantTimestreamError(t, err, "ResourceNotFoundException")
}

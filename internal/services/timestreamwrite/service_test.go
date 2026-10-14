package timestreamwrite_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(endpoint string) context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}, Endpoint: endpoint,
	})
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func fixture(t *testing.T) (*timestreamwrite.Service, context.Context) {
	t.Helper()
	s := timestreamwrite.New(memory.New(nil, nil))
	ctx := testContext("")
	if _, err := s.CreateDatabase(ctx, &api.CreateDatabaseRequest{DatabaseName: new("db")}); err == nil {
		t.Fatal("a two-letter database name should be rejected")
	}

	if _, err := s.CreateDatabase(ctx, &api.CreateDatabaseRequest{DatabaseName: new("db1")}); err != nil {
		t.Fatal(err)
	}

	table := &api.CreateTableRequest{DatabaseName: new("db1"), TableName: new("tab1")}
	if _, err := s.CreateTable(ctx, table); err != nil {
		t.Fatal(err)
	}

	return s, ctx
}

func TestDescribeEndpointsReturnsTheClientAddress(t *testing.T) {
	s := timestreamwrite.New(memory.New(nil, nil))
	out, err := s.DescribeEndpoints(testContext("http://127.0.0.1:4566"), &api.DescribeEndpointsRequest{})
	if err != nil || len(out.Endpoints) != 1 || *out.Endpoints[0].Address != "http://127.0.0.1:4566" {
		t.Fatal(out, err)
	}
}

func TestWriteRecordsRules(t *testing.T) {
	s, ctx := fixture(t)
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	write := func(r api.Record, common *api.Record) error {
		_, err := s.WriteRecords(ctx, &api.WriteRecordsRequest{
			DatabaseName: new("db1"), TableName: new("tab1"), CommonAttributes: common, Records: []api.Record{r},
		})
		return err
	}

	double := api.Record{MeasureName: new("m"), MeasureValue: new("1.5"), MeasureValueType: new("DOUBLE"), Time: &now}
	if err := write(double, nil); err != nil {
		t.Fatal(err)
	}

	// Common attributes complete a record.
	if err := write(api.Record{MeasureName: new("m")}, &api.Record{
		MeasureValue: new("true"), MeasureValueType: new("BOOLEAN"), Time: &now, TimeUnit: new("MILLISECONDS"),
	}); err != nil {
		t.Fatal(err)
	}

	// A MULTI record takes MeasureValues.
	multi := api.Record{
		MeasureName: new("m"), MeasureValueType: new("MULTI"), Time: &now,
		MeasureValues: []api.MeasureValue{{Name: new("a"), Value: new("1"), Type: new("BIGINT")}},
	}
	if err := write(multi, nil); err != nil {
		t.Fatal(err)
	}

	multi.MeasureValue = new("1")
	wantCode(t, write(multi, nil), "ValidationException")

	for name, bad := range map[string]api.Record{
		"no measure name": {MeasureValue: new("1"), MeasureValueType: new("BIGINT"), Time: &now},
		"bad bigint":      {MeasureName: new("m"), MeasureValue: new("1.5"), MeasureValueType: new("BIGINT"), Time: &now},
		"bad boolean":     {MeasureName: new("m"), MeasureValue: new("yes"), MeasureValueType: new("BOOLEAN"), Time: &now},
		"no time":         {MeasureName: new("m"), MeasureValue: new("1"), MeasureValueType: new("BIGINT")},
		"bad time unit": {
			MeasureName: new("m"), MeasureValue: new("1"), MeasureValueType: new("BIGINT"), Time: &now,
			TimeUnit: new("DAYS"),
		},
		"empty dimension": {
			MeasureName: new("m"), MeasureValue: new("1"), MeasureValueType: new("BIGINT"), Time: &now,
			Dimensions: []api.Dimension{{Name: new("d"), Value: new("")}},
		},
	} {
		if err := write(bad, nil); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}

	// Seconds are read as seconds: this time is a day ahead, so it is rejected, not ingested.
	future := strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
	double.Time, double.TimeUnit = &future, new("SECONDS")
	wantCode(t, write(double, nil), "RejectedRecordsException")

	// Too many records.
	_, err := s.WriteRecords(ctx, &api.WriteRecordsRequest{
		DatabaseName: new("db1"), TableName: new("tab1"), Records: make([]api.Record, 101),
	})
	wantCode(t, err, "ValidationException")
}

func TestSchemaAndMagneticStoreRules(t *testing.T) {
	s, ctx := fixture(t)
	create := func(in api.CreateTableRequest) error {
		in.DatabaseName, in.TableName = new("db1"), new("tab2")
		_, err := s.CreateTable(ctx, &in)
		return err
	}

	wantCode(t, create(api.CreateTableRequest{Schema: &api.Schema{CompositePartitionKey: []api.PartitionKey{
		{Type: new("DIMENSION")},
	}}}), "ValidationException")
	wantCode(t, create(api.CreateTableRequest{MagneticStoreWriteProperties: &api.MagneticStoreWriteProperties{
		MagneticStoreRejectedDataLocation: &api.MagneticStoreRejectedDataLocation{
			S3Configuration: &api.S3Configuration{BucketName: new("bucket"), EncryptionOption: new("SSE_KMS")},
		},
	}}), "ValidationException")
	wantCode(t, create(api.CreateTableRequest{RetentionProperties: &api.RetentionProperties{
		MemoryStoreRetentionPeriodInHours: new(int64(9000)),
	}}), "ValidationException")

	if err := create(api.CreateTableRequest{}); err != nil {
		t.Fatal(err)
	}

	wantCode(t, create(api.CreateTableRequest{}), "ConflictException")
}

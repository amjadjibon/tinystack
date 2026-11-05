package timestreamquery

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestTranslate(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`SELECT bin(time,1h),avg(measure_value::double) FROM "my-db"."cpu" WHERE time>ago(1d) GROUP BY bin(time,1h)`,
		`SELECT date_trunc('day',time),from_milliseconds(0),to_milliseconds(time),now() FROM db.cpu`,
		`SELECT 'ago(1h); DELETE' FROM db.cpu`,
	} {
		translated, db, table, err := translate(q, now)
		if err != nil || db == "" || table == "" || translated == "" {
			t.Fatalf("%s: %s %v", q, translated, err)
		}
	}

	for _, q := range []string{
		"DELETE FROM db.cpu", "SELECT load_extension('x')", "SELECT * FROM db.cpu JOIN db.other",
		"SELECT time OVER() FROM db.cpu", "SELECT ago(1year)", "SELECT 1; SELECT 2", "SELECT 1/2",
		"SELECT `sqlite_version`()",
	} {
		_, _, _, err := translate(q, now)
		if err == nil || !strings.Contains(err.Error(), "Unsupported") {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if got := now.Format(timestampLayout); got != "2024-01-01 00:00:00.000000000" {
		t.Fatal(got)
	}
}

func TestMapResults(t *testing.T) {
	at := time.Now().UTC()
	result := &api.QueryResponse{
		ColumnInfo: []api.ColumnInfo{{Name: new("t")}, {Name: new("host")}, {Name: new("v")}},
		Rows: []api.Row{
			{
				Data: []api.Datum{
					{ScalarValue: new(at.Format(timestampLayout))},
					{ScalarValue: new("a")},
					{ScalarValue: new("2")},
				},
			},
		},
	}

	target := &api.TimestreamConfiguration{
		TimeColumn:        new("t"),
		DimensionMappings: []api.DimensionMapping{{Name: new("host")}},
		MixedMeasureMappings: []api.MixedMeasureMapping{
			{SourceColumn: new("v"), TargetMeasureName: new("cpu"), MeasureValueType: new("DOUBLE")},
		},
		MultiMeasureMappings: &api.MultiMeasureMappings{
			TargetMultiMeasureName: new("metrics"),
			MultiMeasureAttributeMappings: []api.MultiMeasureAttributeMapping{
				{
					SourceColumn:                    new("v"),
					TargetMultiMeasureAttributeName: new("temperature"),
					MeasureValueType:                new("DOUBLE"),
				},
			},
		},
	}

	mixed := target.MixedMeasureMappings
	target.MixedMeasureMappings = nil
	records, err := mapResults(result, target, at)
	if err != nil || len(records) != 1 || *records[0].MeasureValues[0].Name != "temperature" {
		t.Fatal(records, err)
	}

	target.MultiMeasureMappings = nil
	target.MixedMeasureMappings = mixed
	records, err = mapResults(result, target, at)
	if err != nil || len(records) != 1 || *records[0].MeasureName != "cpu" {
		t.Fatal(records, err)
	}

	target.TimeColumn = new("missing")
	if _, err = mapResults(result, target, at); err == nil {
		t.Fatal("missing time column accepted")
	}
}

func TestQueryModelConstraints(t *testing.T) {
	svc := New(memory.New(nil, nil), nil, nil, nil, nil, nil)
	defer svc.Stop()
	for _, in := range []*api.QueryRequest{
		{},
		{QueryString: new("SELECT 1"), MaxRows: new(int64(0))},
		{QueryString: new("SELECT 1"), MaxRows: new(int64(1001))},
	} {
		_, err := api.Descriptor(svc).Operations["Query"].Invoke(context.Background(), in)
		if err == nil {
			t.Fatal("invalid model input accepted")
		}
	}
}

func TestScheduledClaimsSkipMissedRuns(t *testing.T) {
	store := memory.New(nil, nil)
	svc := New(store, nil, nil, nil, nil, nil)
	defer svc.Stop()
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	now := time.Now()
	r := &scheduledQueryRecord{
		Name:        "scheduled",
		QueryString: "SELECT now() AS t",
		State:       stateEnabled,
		Enabled: now.Add(
			-10 * time.Minute,
		),
		ScheduleConfiguration: &api.ScheduleConfiguration{ScheduleExpression: new("rate(1 minute)")},
	}

	err := store.Update(ctx, func(tx storage.WriteTx) error {
		if e := storage.Write(tx, key(scope, kindScheduledQuery, r.Name), r); e != nil {
			return e
		}

		return storage.Write(tx, scheduledIndex(scope, r.Name), scheduledIndexRecord{Scope: scope, Name: r.Name})
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.tick(ctx)
	err = store.View(ctx, func(tx storage.ReadTx) error {
		stored, _, e := storage.Read[scheduledQueryRecord](tx, key(scope, kindScheduledQuery, r.Name))
		if e != nil {
			return e
		}

		if stored.LastRun == nil || stored.LastRun.FailureReason != nil || !stored.NextDue.After(now) ||
			stored.Running {
			t.Fatalf("unexpected run: %+v", stored)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = svc.claim(ctx, scope, r.Name, now, true, nil); err == nil {
		t.Fatal("claimed a missed run twice")
	}
}

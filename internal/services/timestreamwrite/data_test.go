package timestreamwrite

import (
	"context"
	"strconv"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestStoredVersionsAndExpiry(t *testing.T) {
	store := memory.New(nil, nil)
	svc := New(store)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	_, err := svc.CreateDatabase(ctx, &api.CreateDatabaseRequest{DatabaseName: new("metrics")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.CreateTable(ctx, &api.CreateTableRequest{DatabaseName: new("metrics"), TableName: new("cpu")})
	if err != nil {
		t.Fatal(err)
	}

	point := api.Record{
		MeasureName:      new("cpu"),
		MeasureValue:     new("1"),
		MeasureValueType: new("BIGINT"),
		Time:             new(strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}

	write := func() error {
		_, e := svc.WriteRecords(
			ctx,
			&api.WriteRecordsRequest{DatabaseName: new("metrics"), TableName: new("cpu"), Records: []api.Record{point}},
		)
		return e
	}

	if err = write(); err != nil {
		t.Fatal(err)
	}

	if err = write(); err != nil {
		t.Fatal(err)
	}

	point.MeasureValue = new("2")
	if err = write(); err == nil {
		t.Fatal("conflicting version accepted")
	}

	point.Version = new(int64(2))
	if err = write(); err != nil {
		t.Fatal(err)
	}

	records, err := svc.ReadRecords(ctx, scope, "metrics", "cpu")
	if err != nil || len(records) != 1 || records[0].MeasureValue != "2" {
		t.Fatal(records, err)
	}

	err = store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(recordPrefix(scope, "metrics", "cpu")) {
			r, _, e := storage.Read[StoredRecord](tx, k)
			if e != nil {
				return e
			}

			r.Time = time.Now().AddDate(-300, 0, 0)
			if writeErr := storage.Write(tx, k, r); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	records, err = svc.ReadRecords(ctx, scope, "metrics", "cpu")
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
}

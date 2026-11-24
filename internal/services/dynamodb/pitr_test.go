package dynamodb

import (
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestPITRPrunesExpiredHistory(t *testing.T) {
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	s := &Service{store: &memory.Store{}, now: func() time.Time { return now }}
	ctx := request.WithMetadata(t.Context(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	name := new("pitr-test")
	if _, err := s.CreateTable(ctx, &api.CreateTableInput{
		TableName: name, BillingMode: new(billingOnDemand),
		KeySchema:            []api.KeySchemaElement{{AttributeName: new("pk"), KeyType: new("HASH")}},
		AttributeDefinitions: []api.AttributeDefinition{{AttributeName: new("pk"), AttributeType: new("S")}},
	}); err != nil {
		t.Fatal(err)
	}

	setDays := func(days int64) {
		t.Helper()
		if _, err := s.UpdateContinuousBackups(ctx, &api.UpdateContinuousBackupsInput{
			TableName: name,
			PointInTimeRecoverySpecification: &api.PointInTimeRecoverySpecification{
				PointInTimeRecoveryEnabled: new(true), RecoveryPeriodInDays: &days,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	put := func(value string) {
		t.Helper()
		if _, err := s.PutItem(ctx, &api.PutItemInput{
			TableName: name, Item: item{"pk": {S: new("key")}, "value": {S: &value}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	setDays(1)
	now = now.Add(time.Hour)
	put("first")
	now = now.Add(2 * time.Hour)
	put("second")
	now = now.Add(23 * time.Hour)
	key, err := tableKey(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	if err = s.prunePITRTable(ctx, pitrIndexKey(key)); err != nil {
		t.Fatal(err)
	}

	if err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if count := len(tx.Keys(pitrPrefix(key))); count != 1 {
			t.Fatalf("journal records = %d, want 1", count)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, err := s.DescribeContinuousBackups(ctx, &api.DescribeContinuousBackupsInput{TableName: name})
	if err != nil {
		t.Fatal(err)
	}

	d := out.ContinuousBackupsDescription.PointInTimeRecoveryDescription
	earliest := common.Unix(now.Add(-24 * time.Hour))
	if *d.EarliestRestorableDateTime != earliest || *d.LatestRestorableDateTime != common.Unix(now) {
		t.Fatalf("incorrect recovery bounds: %+v", d)
	}

	setDays(35)
	if _, err = s.RestoreTableToPointInTime(ctx, &api.RestoreTableToPointInTimeInput{
		SourceTableName: name, TargetTableName: new("restored-too-old"), RestoreDateTime: new(earliest - 1),
	}); !isCode(err, "InvalidRestoreTimeException") {
		t.Fatalf("expired restore accepted after expanding window: %v", err)
	}

	tgt := new("restored-earliest")
	if _, err = s.RestoreTableToPointInTime(ctx, &api.RestoreTableToPointInTimeInput{
		SourceTableName: name, TargetTableName: tgt, RestoreDateTime: &earliest,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetItem(ctx, &api.GetItemInput{TableName: tgt, Key: item{"pk": {S: new("key")}}})
	if err != nil || common.Value(got.Item["value"].S) != "first" {
		t.Fatalf("restored at earliest timestamp: %+v (%v)", got, err)
	}
}

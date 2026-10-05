package storage_test

import (
	"context"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type item struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestJSONReadWriteRecords(t *testing.T) {
	t.Parallel()

	store := &memory.Store{}
	ctx := context.Background()

	err := store.Update(ctx, func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, "items/b", item{Name: "b", Count: 2}); writeErr != nil {
			return writeErr
		}
		if writeErr := storage.Write(tx, "items/a", item{Name: "a", Count: 1}); writeErr != nil {
			return writeErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	err = store.View(ctx, func(tx storage.ReadTx) error {
		v, found, readErr := storage.Read[item](tx, "items/a")
		if readErr != nil {
			t.Fatalf("Read failed: %v", readErr)
		}
		if !found {
			t.Fatal("expected item/a to be found")
		}
		if v.Name != "a" || v.Count != 1 {
			t.Fatalf("unexpected value: %+v", v)
		}

		_, found, readErr = storage.Read[item](tx, "items/nonexistent")
		if readErr != nil {
			t.Fatalf("Read nonexistent failed: %v", readErr)
		}
		if found {
			t.Fatal("expected items/nonexistent to not be found")
		}

		all, recordsErr := storage.Records[item](tx, "items/")
		if recordsErr != nil {
			t.Fatalf("Records failed: %v", recordsErr)
		}
		if len(all) != 2 {
			t.Fatalf("expected 2 items, got %d", len(all))
		}
		if all[0].Name != "a" || all[1].Name != "b" {
			t.Fatalf("records not in key order: %+v, %+v", all[0], all[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View failed: %v", err)
	}
}

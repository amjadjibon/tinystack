package redshiftdata

import (
	"context"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftdata"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestStatementExpiry(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store, nil, nil)
	s.Stop()
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}

	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	r := &record{
		DescribeStatementResponse: api.DescribeStatementResponse{Id: new("expired"), Status: new(statusFinished)},
		Expires:                   time.Now().Add(-time.Second),
	}

	err := store.Update(ctx, func(tx storage.WriteTx) error {
		if e := save(tx, scope, r); e != nil {
			return e
		}

		return storage.Write(tx, indexPrefix+"expired", work{Scope: scope, ID: "expired"})
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, lookupErr := s.get(ctx, new("expired")); lookupErr == nil {
		t.Fatal("expired statement is visible")
	}

	s.advance(ctx)
	err = store.View(ctx, func(tx storage.ReadTx) error {
		if _, ok := tx.Get(scope.Key(serviceName, kindStatement, "expired")); ok {
			t.Fatal("statement was not swept")
		}

		if len(tx.Keys(indexPrefix)) != 0 {
			t.Fatal("index was not swept")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

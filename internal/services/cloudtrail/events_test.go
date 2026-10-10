package cloudtrail

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudtrail"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestHistoryBoundAndCursor(t *testing.T) {
	store := &memory.Store{}
	s := New(store, nil)
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	// Seed a full history to exercise eviction without 1000 redundant transactions.
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		h := &history{Sequence: maxHistory}
		for i := range maxHistory {
			h.Events = append(h.Events, storedEvent{Sequence: uint64(i + 1), Event: api.Event{
				EventName: new("CreateBucket"), EventId: new(fmt.Sprint(i)), EventTime: new(float64(time.Now().Unix())),
			}})
		}

		return storage.Write(tx, scope.Key(serviceName, "history", "events"), h)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Record(
		ctx,
		&catalog.ManagementEvent{
			Metadata: request.Metadata{Scope: scope, Service: "s3", Operation: "CreateBucket"},
			Time:     time.Now(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	err = store.View(ctx, func(tx storage.ReadTx) error {
		h, _, e := storage.Read[history](tx, scope.Key(serviceName, "history", "events"))
		if e != nil {
			return e
		}

		if len(h.Events) != maxHistory || h.Events[0].Sequence != 2 {
			t.Fatalf("bounded history: %d, first %d", len(h.Events), h.Events[0].Sequence)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.LookupEvents(ctx, &api.LookupEventsRequest{MaxResults: new(int64(1))})
	if err != nil || first.NextToken == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}

	// A newer event does not shift the continuation cursor.
	err = s.Record(
		ctx,
		&catalog.ManagementEvent{
			Metadata: request.Metadata{Scope: scope, Service: "s3", Operation: "GetBucketTagging"},
			Time:     time.Now(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.LookupEvents(ctx, &api.LookupEventsRequest{MaxResults: new(int64(1)), NextToken: first.NextToken})
	if err != nil || common.Value(second.Events[0].EventId) == common.Value(first.Events[0].EventId) {
		t.Fatalf("second page: %+v %v", second, err)
	}

	_, err = s.LookupEvents(
		ctx,
		&api.LookupEventsRequest{NextToken: first.NextToken, LookupAttributes: []api.LookupAttribute{
			{AttributeKey: new("EventName"), AttributeValue: new("GetBucketTagging")},
		}},
	)
	if err == nil {
		t.Fatal("token accepted changed filters")
	}

	s.Reset(ctx)
	events, err := s.LookupEvents(ctx, &api.LookupEventsRequest{})
	if err != nil || len(events.Events) != 0 {
		t.Fatalf("reset: %+v %v", events, err)
	}
}

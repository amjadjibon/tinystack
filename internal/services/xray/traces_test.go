package xray

import (
	"context"
	"fmt"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestTracePruning(t *testing.T) {
	store := memory.New(nil, nil)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	now := float64(2000000000)
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		stamps := []float64{now - 30*86400 - 1, now - 30*86400}
		for i := range maxTraces + 1 {
			stamps = append(stamps, now-float64(maxTraces-i))
		}

		for i, stamp := range stamps {
			tx.Put(scope.Key(serviceName, traceKind, fmt.Sprintf("1-%08x-%024d", int64(stamp), i)), []byte("{}"))
		}

		pruneTraces(tx, scope, now)
		keys := tx.Keys(scope.Prefix(serviceName, traceKind))
		if len(keys) != maxTraces {
			t.Fatalf("kept %d traces, want %d", len(keys), maxTraces)
		}

		// The expired trace goes first, then the oldest retained ones until the cap holds.
		want := scope.Key(serviceName, traceKind, fmt.Sprintf("1-%08x-%024d", int64(now-float64(maxTraces-1)), 3))
		if keys[0] != want {
			t.Fatalf("oldest kept trace %s, want %s", keys[0], want)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTraceSummaryPagination(t *testing.T) {
	store := memory.New(nil, nil)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	// Seed fresh records without 101 serial ingestion transactions.
	now := common.Unix(time.Now())
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		for i := range 101 {
			id := fmt.Sprintf("1-%08x-%024d", int64(now), i)
			rec := traceRecord{
				ID:       id,
				Segments: map[string]storedSegment{"a": {Segment: &segmentDocument{Start: now, End: new(now + 1)}}},
			}

			if err := storage.Write(tx, scope.Key(serviceName, traceKind, id), rec); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := New(store)
	in := &api.GetTraceSummariesRequest{StartTime: new(now - 1), EndTime: new(now + 1)}
	first, err := svc.GetTraceSummaries(ctx, in)
	if err != nil || len(first.TraceSummaries) != 100 || first.NextToken == nil {
		t.Fatalf("first page: %v %v", first, err)
	}

	in.NextToken = first.NextToken
	last, err := svc.GetTraceSummaries(ctx, in)
	if err != nil || len(last.TraceSummaries) != 1 || last.NextToken != nil {
		t.Fatalf("last page: %v %v", last, err)
	}
}

func TestGraphAggregation(t *testing.T) {
	records := []traceRecord{}
	for i := range 3 {
		downstream := &segmentDocument{Name: "DynamoDB", Namespace: awsNamespace, Start: 10, End: new(float64(11))}
		root := &segmentDocument{
			Name:        "api",
			Start:       10,
			End:         new(float64(12)),
			Subsegments: []*segmentDocument{downstream},
		}

		root.Error = i == 0
		root.Fault = i == 1
		root.Throttle = i == 2
		records = append(records, traceRecord{Segments: map[string]storedSegment{"a": {Segment: root}}})
	}

	nodes := buildGraph(records, "123456789012")
	if len(nodes) != 2 {
		t.Fatal(nodes)
	}

	for _, node := range nodes {
		if *node.Name != "api" {
			continue
		}

		stats := node.SummaryStatistics
		if *stats.TotalCount != 3 || *stats.ErrorStatistics.TotalCount != 2 ||
			*stats.ErrorStatistics.ThrottleCount != 1 ||
			*stats.FaultStatistics.TotalCount != 1 ||
			*stats.TotalResponseTime != 6 {
			t.Fatal(stats)
		}

		if len(node.Edges) != 1 || *node.Edges[0].SummaryStatistics.TotalCount != 3 ||
			*node.Edges[0].SummaryStatistics.TotalResponseTime != 3 {
			t.Fatal(node.Edges)
		}
	}
}

func TestTraceCapacity(t *testing.T) {
	store := memory.New(nil, nil)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	now := common.Unix(time.Now())
	oldest := ""
	err := store.Update(ctx, func(tx storage.WriteTx) error {
		for i := range maxTraces {
			id := fmt.Sprintf("1-%08x-%024d", int64(now), i)
			if i == 0 {
				oldest = id
			}
			rec := traceRecord{
				ID: id,
				Segments: map[string]storedSegment{
					"a": {Segment: &segmentDocument{Start: now - 10, End: new(now - 9)}},
				},
			}
			if err := storage.Write(tx, scope.Key(serviceName, traceKind, id), rec); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(
		`{"trace_id":"1-%08x-ffffffffffffffffffffffff","id":"1234567890abcdef","name":"api","start_time":%f,"end_time":%f}`,
		int64(now),
		now,
		now+1,
	)
	out, err := New(store).PutTraceSegments(ctx, &api.PutTraceSegmentsRequest{TraceSegmentDocuments: []string{raw}})
	if err != nil || len(out.UnprocessedTraceSegments) != 0 {
		t.Fatalf("put: %v %v", out, err)
	}
	err = store.View(ctx, func(tx storage.ReadTx) error {
		if len(tx.Keys(scope.Prefix(serviceName, traceKind))) != maxTraces {
			t.Fatal("trace capacity not enforced")
		}
		_, found, readErr := storage.Read[traceRecord](tx, scope.Key(serviceName, traceKind, oldest))
		if found {
			t.Fatal("oldest trace not evicted")
		}
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
}

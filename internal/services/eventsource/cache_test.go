package eventsource

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestReadCache(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0)
	const source = "arn:aws:sqs:us-east-1:000000000000:work"
	d := catalog.ServiceDelivery{
		Principal: "lambda.amazonaws.com", SourceARN: "function",
		Role: &catalog.DeliveryRole{ARN: "role", Session: "worker"},
	}

	var cache ReadCache
	calls := 0
	denied := errors.New("denied")
	var result error
	authorize := catalog.ServiceAuthorizer(func(_ context.Context, got catalog.ServiceDelivery) error {
		calls++
		if got.Role.ARN != d.Role.ARN || got.Role.Session != "worker" || got.SourceARN != "function" {
			t.Fatalf("delivery = %+v, role = %+v", got, got.Role)
		}

		return result
	})
	check := func(reader, src string, at time.Time, wantCalls int, wantErr error) {
		t.Helper()
		call, err := cache.Authorize(ctx, authorize, reader, src, d, at)
		if !errors.Is(err, wantErr) || calls != wantCalls {
			t.Fatalf("check: calls=%d err=%v, want %d %v", calls, err, wantCalls, wantErr)
		}

		if err != nil && call.Operation != "ReceiveMessage" {
			t.Fatalf("denied operation = %s", call.Operation)
		}
	}

	check("mapping", source, now, 3, nil)
	result = denied
	check("mapping", source, now.Add(ReadCheckInterval-time.Nanosecond), 3, nil)
	check("mapping", source, now.Add(ReadCheckInterval), 4, denied)
	result = nil
	check("mapping", source, now.Add(ReadCheckInterval+time.Second), 4, denied)
	check("mapping", source, now.Add(2*ReadCheckInterval), 7, nil)
	// A second reader never shares the first reader's permission result.
	check("other", source, now, 10, nil)
	d.Role.ARN = "changed-role"
	check("mapping", source, now, 13, nil)
	check("mapping", source+"-changed", now, 16, nil)
	cache.Delete("mapping")
	check("mapping", source+"-changed", now, 19, nil)
	cache.Reset(ctx)
	check("mapping", source+"-changed", now, 22, nil)
}

func TestReadCacheConcurrent(t *testing.T) {
	var cache ReadCache
	var wg sync.WaitGroup
	calls := 0
	authorize := catalog.ServiceAuthorizer(func(context.Context, catalog.ServiceDelivery) error {
		calls++ // Serialized by the cache; the race detector checks coalescing too.
		return nil
	})
	for range 20 {
		wg.Go(func() {
			_, err := cache.Authorize(
				context.Background(),
				authorize,
				"pipe",
				"arn:aws:sqs:us-east-1:000000000000:work",
				catalog.ServiceDelivery{Role: &catalog.DeliveryRole{ARN: "role"}},
				time.Unix(100, 0),
			)
			if err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

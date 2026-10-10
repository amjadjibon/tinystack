package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/faults"
)

func TestFaultEmptyPathAllocations(t *testing.T) {
	s := &Server{}
	s.faults.reset(nil)
	c := &call{service: &catalog.Service{Name: "s3", SigningName: "s3"}}
	if allocations := testing.AllocsPerRun(100, func() {
		if err := s.inject(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("empty fault path allocates %g times", allocations)
	}
}

func TestFaultFirstMatchAndCancellation(t *testing.T) {
	s := &Server{}
	s.faults.reset([]faults.Rule{
		{Error: &faults.Error{Code: "First"}, Probability: new(0.0)},
		{Error: &faults.Error{Code: "Second"}},
	})
	c := &call{service: &catalog.Service{SigningName: "s3"}, meta: request.Metadata{Service: "s3"}}
	if err := s.inject(context.Background(), c); err != nil || c.injected {
		t.Fatalf("disabled first match fell through: %v", err)
	}

	s.faults.reset([]faults.Rule{{Error: &faults.Error{Code: "First"}}, {Error: &faults.Error{Code: "Second"}}})
	var awsErr *awserror.Error
	if err := s.inject(context.Background(), c); !errors.As(err, &awsErr) || awsErr.Code != "First" ||
		awsErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first fault = %v", err)
	}

	s.faults.reset([]faults.Rule{{LatencyMS: 3600000}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.inject(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep = %v", err)
	}
}

func TestFaultConcurrentSnapshots(t *testing.T) {
	s := &Server{}
	s.faults.reset(nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 40 {
				w := httptest.NewRecorder()
				r := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/_tinystack/faults",
					strings.NewReader(`[{"error":{"code":"SlowDown"}}]`))
				s.serveFaults(w, r)
				if w.Code != http.StatusOK {
					t.Errorf("PATCH: %d", w.Code)
				}

				c := &call{service: &catalog.Service{SigningName: "s3"}}
				_ = s.inject(context.Background(), c)
			}
		})
	}

	wg.Wait()
	if got := len(s.faults.current.Load().rules); got != 320 {
		t.Fatalf("concurrent appends lost rules: %d", got)
	}
}

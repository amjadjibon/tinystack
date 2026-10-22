package lambda

import (
	"sync"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

type metricRecorder struct {
	datums []awsmetrics.Datum
	mu     sync.Mutex
}

func (r *metricRecorder) Publish(_ request.Scope, _ string, data ...awsmetrics.Datum) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.datums = append(r.datums, data...)
}

func TestPublishIteratorAge(t *testing.T) {
	now := time.Unix(1700000010, 500000000)
	svc := &Service{
		now: func() time.Time {
			return now
		},
	}
	recorder := &metricRecorder{}
	svc.UseMetrics(recorder)

	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}

	// 1. Zero arrival timestamp should not publish anything.
	svc.publishIteratorAge(scope, "fn", "", time.Time{})
	if len(recorder.datums) != 0 {
		t.Fatalf("expected 0 datums for zero arrival, got %d", len(recorder.datums))
	}

	// 2. Future arrival timestamp should be clamped to 0.
	future := now.Add(5 * time.Second)
	svc.publishIteratorAge(scope, "fn", "", future)
	if len(recorder.datums) != 1 {
		t.Fatalf("expected 1 datum, got %d", len(recorder.datums))
	}

	if recorder.datums[0].Name != metricIteratorAge ||
		recorder.datums[0].Unit != awsmetrics.Milliseconds ||
		recorder.datums[0].Value != 0 {
		t.Fatalf("expected 0 value for future arrival, got %+v", recorder.datums[0])
	}

	recorder.datums = nil

	// 3. Normal arrival 1.5s ago, unqualified: should publish 1 datum with FunctionName dimension only.
	arrival := now.Add(-1500 * time.Millisecond)
	svc.publishIteratorAge(scope, "my-function", "$LATEST", arrival)
	if len(recorder.datums) != 1 {
		t.Fatalf("expected 1 datum, got %d", len(recorder.datums))
	}

	d0 := recorder.datums[0]
	if d0.Name != metricIteratorAge || d0.Unit != awsmetrics.Milliseconds {
		t.Fatalf("unexpected metric name or unit: %+v", d0)
	}

	if d0.Value < 1499 || d0.Value > 1501 {
		t.Fatalf("unexpected IteratorAge value: %v", d0.Value)
	}

	if len(d0.Dimensions) != 1 ||
		d0.Dimensions[0].Name != dimensionFunction ||
		d0.Dimensions[0].Value != "my-function" {
		t.Fatalf("unexpected dimensions for unqualified function: %+v", d0.Dimensions)
	}

	recorder.datums = nil

	// 4. Qualified (alias): should publish 2 datums (FunctionName and FunctionName+Resource).
	svc.publishIteratorAge(scope, "my-function", "prod", arrival)
	if len(recorder.datums) != 2 {
		t.Fatalf("expected 2 datums for qualified function, got %d", len(recorder.datums))
	}

	foundFunctionOnly := false
	foundFunctionAndResource := false
	for _, d := range recorder.datums {
		if d.Name != metricIteratorAge || d.Unit != awsmetrics.Milliseconds {
			t.Fatalf("unexpected metric: %+v", d)
		}

		if len(d.Dimensions) == 1 &&
			d.Dimensions[0].Name == dimensionFunction &&
			d.Dimensions[0].Value == "my-function" {
			foundFunctionOnly = true
		}

		if len(d.Dimensions) == 2 {
			hasFunc := false
			hasRes := false
			for _, dim := range d.Dimensions {
				if dim.Name == dimensionFunction && dim.Value == "my-function" {
					hasFunc = true
				}

				if dim.Name == dimensionResource && dim.Value == "my-function:prod" {
					hasRes = true
				}
			}

			if hasFunc && hasRes {
				foundFunctionAndResource = true
			}
		}
	}

	if !foundFunctionOnly || !foundFunctionAndResource {
		t.Fatalf("missing expected dimensions for qualified function: %+v", recorder.datums)
	}
}

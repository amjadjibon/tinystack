package cloudwatch

import (
	"context"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// TestEvaluateAcrossPeriods checks that a data point sent just before a
// period ends still sets the alarm when the evaluator first runs after it.
func TestEvaluateAcrossPeriods(t *testing.T) {
	s := New(&memory.Store{}, nil)
	// Stop the evaluator so the test drives the clock.
	s.Stop()
	now := time.Date(2026, time.March, 14, 10, 0, 59, 500_000_000, time.UTC)
	s.now = func() time.Time { return now }
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	_, err := s.PutMetricAlarm(ctx, &api.PutMetricAlarmInput{
		AlarmName: new("high"), Namespace: new("App"), MetricName: new("Load"), Statistic: new("Maximum"),
		Period: new(int64(60)), EvaluationPeriods: new(int64(1)), Threshold: new(5.0),
		ComparisonOperator: new("GreaterThanThreshold"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.PutMetricData(ctx, &api.PutMetricDataInput{
		Namespace: new("App"), MetricData: []api.MetricDatum{{MetricName: new("Load"), Value: new(9.0)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	state := func() string {
		t.Helper()
		out, describeErr := s.DescribeAlarms(ctx, &api.DescribeAlarmsInput{AlarmNames: []string{"high"}})
		if describeErr != nil || len(out.MetricAlarms) != 1 {
			t.Fatalf("%+v %v", out, describeErr)
		}

		return common.Value(out.MetricAlarms[0].StateValue)
	}

	// The next period has begun, and has no data yet.
	now = now.Add(time.Second)
	if err = s.evaluate(context.Background(), target{scope, "high"}, false); err != nil {
		t.Fatal(err)
	}

	if got := state(); got != stateAlarm {
		t.Fatalf("after the period ended: %s", got)
	}

	// A period with no data at all is missing data.
	now = now.Add(time.Minute)
	if err = s.evaluate(context.Background(), target{scope, "high"}, false); err != nil {
		t.Fatal(err)
	}

	if got := state(); got != stateNoData {
		t.Fatalf("after an empty period: %s", got)
	}
}

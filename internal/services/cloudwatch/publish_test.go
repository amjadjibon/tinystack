package cloudwatch

import (
	"context"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

var publishScope = request.Scope{AccountID: "000000000000", Region: "us-east-1"}

func publishFixture(t *testing.T) (*Service, context.Context, *time.Time) {
	t.Helper()
	s := New(&memory.Store{}, nil)
	now := time.Date(2026, time.March, 14, 10, 0, 30, 0, time.UTC)
	s.now = func() time.Time { return now }
	t.Cleanup(s.Stop)
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: publishScope})
	return s, ctx, &now
}

func sent(value float64) awsmetrics.Datum {
	return awsmetrics.Datum{
		Name: "NumberOfMessagesSent", Unit: awsmetrics.Count, Value: value,
		Dimensions: []awsmetrics.Dimension{{Name: "QueueName", Value: "jobs"}},
	}
}

func statistics(t *testing.T, s *Service, ctx context.Context, now time.Time) []api.Datapoint {
	t.Helper()
	out, err := s.GetMetricStatistics(ctx, &api.GetMetricStatisticsInput{
		Namespace: new("AWS/SQS"), MetricName: new("NumberOfMessagesSent"), Period: new(int64(60)),
		StartTime: new(common.Unix(now.Add(-time.Hour))), EndTime: new(common.Unix(now.Add(time.Hour))),
		Dimensions: []api.Dimension{{Name: new("QueueName"), Value: new("jobs")}},
		Statistics: []string{"Sum", "SampleCount", "Minimum", "Maximum", "Average"},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Datapoints
}

func TestPublishAggregatesPerMinute(t *testing.T) {
	s, ctx, now := publishFixture(t)
	s.Publish(publishScope, "AWS/SQS", sent(1), sent(4))
	s.flushPublished(ctx)
	// A later flush in the same minute extends the stored point.
	s.Publish(publishScope, "AWS/SQS", sent(2))
	s.flushPublished(ctx)
	points := statistics(t, s, ctx, *now)
	if len(points) != 1 {
		t.Fatalf("%d data points: %+v", len(points), points)
	}

	p := points[0]
	if common.Value(p.Sum) != 7 || common.Value(p.SampleCount) != 3 || common.Value(p.Minimum) != 1 ||
		common.Value(p.Maximum) != 4 || common.Value(p.Unit) != "Count" {
		t.Fatalf("%+v", p)
	}

	*now = now.Add(time.Minute)
	s.Publish(publishScope, "AWS/SQS", sent(5))
	s.flushPublished(ctx)
	if points = statistics(t, s, ctx, *now); len(points) != 2 || common.Value(points[1].Sum) != 5 {
		t.Fatalf("%+v", points)
	}

	listed, err := s.ListMetrics(ctx, &api.ListMetricsInput{Namespace: new("AWS/SQS")})
	if err != nil || len(listed.Metrics) != 1 || common.Value(listed.Metrics[0].MetricName) != "NumberOfMessagesSent" {
		t.Fatalf("%+v %v", listed, err)
	}

	_, err = s.PutMetricData(ctx, &api.PutMetricDataInput{
		Namespace: new("AWS/SQS"), MetricData: []api.MetricDatum{{MetricName: new("X"), Value: new(1.0)}},
	})
	if err == nil {
		t.Fatal("PutMetricData accepted an AWS/ namespace")
	}
}

func TestPublishSeparatesSeries(t *testing.T) {
	s, ctx, now := publishFixture(t)
	other := sent(10)
	other.Dimensions = []awsmetrics.Dimension{{Name: "QueueName", Value: "other"}}
	s.Publish(publishScope, "AWS/SQS", sent(1), other)
	s.Publish(request.Scope{AccountID: "111111111111", Region: "us-east-1"}, "AWS/SQS", sent(100))
	s.flushPublished(ctx)
	points := statistics(t, s, ctx, *now)
	if len(points) != 1 || common.Value(points[0].Sum) != 1 {
		t.Fatalf("%+v", points)
	}
}

func TestStopFlushesPublished(t *testing.T) {
	s, ctx, now := publishFixture(t)
	s.Publish(publishScope, "AWS/SQS", sent(3))
	s.Stop()
	if points := statistics(t, s, ctx, *now); len(points) != 1 || common.Value(points[0].Sum) != 3 {
		t.Fatalf("%+v", points)
	}

	s.Publish(publishScope, "AWS/SQS", sent(9))
	s.flushPublished(ctx)
	if points := statistics(t, s, ctx, *now); common.Value(points[0].Sum) != 3 {
		t.Fatalf("published after Stop: %+v", points)
	}
}

func TestPublishedMetricRaisesAlarm(t *testing.T) {
	s, ctx, now := publishFixture(t)
	_, err := s.PutMetricAlarm(ctx, &api.PutMetricAlarmInput{
		AlarmName: new("errors"), Namespace: new("AWS/Lambda"), MetricName: new("Errors"), Statistic: new("Sum"),
		Dimensions: []api.Dimension{{Name: new("FunctionName"), Value: new("f")}},
		Period:     new(int64(60)), EvaluationPeriods: new(int64(1)), Threshold: new(0.0),
		ComparisonOperator: new("GreaterThanThreshold"),
	})
	if err != nil {
		t.Fatal(err)
	}

	s.Publish(publishScope, "AWS/Lambda", awsmetrics.Datum{
		Name: "Errors", Unit: awsmetrics.Count, Value: 1,
		Dimensions: []awsmetrics.Dimension{{Name: "FunctionName", Value: "f"}},
	})
	s.flushPublished(ctx)
	if err = s.evaluate(ctx, target{publishScope, "errors"}, false); err != nil {
		t.Fatal(err)
	}

	out, err := s.DescribeAlarms(ctx, &api.DescribeAlarmsInput{AlarmNames: []string{"errors"}})
	if err != nil || len(out.MetricAlarms) != 1 || common.Value(out.MetricAlarms[0].StateValue) != stateAlarm {
		t.Fatalf("%+v %v at %s", out, err, now)
	}
}

func BenchmarkPublish(b *testing.B) {
	s := New(&memory.Store{}, nil)
	defer s.Stop()
	dims := []awsmetrics.Dimension{{Name: "QueueName", Value: "jobs"}}
	b.ReportAllocs()
	for b.Loop() {
		s.Publish(publishScope, "AWS/SQS",
			awsmetrics.Datum{Name: "NumberOfMessagesSent", Unit: awsmetrics.Count, Value: 1, Dimensions: dims},
			awsmetrics.Datum{Name: "SentMessageSize", Unit: awsmetrics.Bytes, Value: 100, Dimensions: dims},
		)
	}
}

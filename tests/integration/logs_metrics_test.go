package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

func TestLogsMetricFilters(t *testing.T) {
	s := server(t)
	c := logsClient(s, "test")
	ctx := context.Background()
	if _, err := c.CreateLogGroup(
		ctx,
		&cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("api")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName: aws.String("api"), LogStreamName: aws.String("a"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutMetricFilter(ctx, &cloudwatchlogs.PutMetricFilterInput{
		LogGroupName: aws.String("api"), FilterName: aws.String("errors"), FilterPattern: aws.String("ERROR"),
		MetricTransformations: []logstypes.MetricTransformation{{
			MetricName: aws.String("Errors"), MetricNamespace: aws.String("App"), MetricValue: aws.String("1"),
			Unit: logstypes.StandardUnitCount,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutMetricFilter(ctx, &cloudwatchlogs.PutMetricFilterInput{
		LogGroupName: aws.String("api"), FilterName: aws.String("by-field"), FilterPattern: aws.String(""),
		MetricTransformations: []logstypes.MetricTransformation{{
			MetricName: aws.String("Latency"), MetricNamespace: aws.String("App"), MetricValue: aws.String("$.ms"),
		}},
	}); err == nil {
		t.Fatal("a field metric value was accepted")
	}

	now := time.Now()
	if _, err := c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: aws.String("api"), LogStreamName: aws.String("a"),
		LogEvents: []logstypes.InputLogEvent{
			{Timestamp: aws.Int64(now.UnixMilli()), Message: aws.String("ERROR one")},
			{Timestamp: aws.Int64(now.UnixMilli() + 1), Message: aws.String("INFO fine")},
			{Timestamp: aws.Int64(now.UnixMilli() + 2), Message: aws.String("ERROR two")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := cloudwatchClient(s).GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace: aws.String("App"), MetricName: aws.String("Errors"),
		StartTime: aws.Time(now.Add(-time.Minute)), EndTime: aws.Time(now.Add(time.Minute)), Period: aws.Int32(60),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
	})
	sum := 0.0
	for _, d := range stats.Datapoints {
		sum += aws.ToFloat64(d.Sum)
	}

	if err != nil || sum != 2 {
		t.Fatalf("errors metric: %+v %v", stats, err)
	}

	described, err := c.DescribeMetricFilters(ctx, &cloudwatchlogs.DescribeMetricFiltersInput{
		MetricName: aws.String("Errors"), MetricNamespace: aws.String("App"),
	})
	if err != nil || len(described.MetricFilters) != 1 ||
		aws.ToString(described.MetricFilters[0].LogGroupName) != "api" {
		t.Fatalf("described: %+v %v", described, err)
	}

	tested, err := c.TestMetricFilter(ctx, &cloudwatchlogs.TestMetricFilterInput{
		FilterPattern: aws.String("ERROR -timeout"), LogEventMessages: []string{"ERROR boom", "ERROR timeout", "ok"},
	})
	if err != nil || len(tested.Matches) != 1 || tested.Matches[0].EventNumber != 1 {
		t.Fatalf("tested: %+v %v", tested, err)
	}

	if _, err = c.DeleteMetricFilter(ctx, &cloudwatchlogs.DeleteMetricFilterInput{
		LogGroupName: aws.String("api"), FilterName: aws.String("errors"),
	}); err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeMetricFilters(ctx, &cloudwatchlogs.DescribeMetricFiltersInput{
		LogGroupName: aws.String("api"),
	})
	if err != nil || len(described.MetricFilters) != 0 {
		t.Fatalf("after delete: %+v %v", described, err)
	}
}

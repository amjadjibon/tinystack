package integration_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

func TestCloudWatchMetricsInsights(t *testing.T) {
	ctx := t.Context()
	c := cloudwatchClient(server(t))
	now := time.Now().UTC().Truncate(time.Minute)
	_, err := c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: new("App/Prod"),
		MetricData: []cwtypes.MetricDatum{
			{MetricName: new("Latency.p"), Timestamp: &now, Dimensions: dimensions("Host", "a"), Value: new(1.0)},
			{MetricName: new("Latency.p"), Timestamp: &now, Dimensions: dimensions("Host", "b"), Value: new(9.0)},
			{MetricName: new("Latency.p"), Timestamp: &now, Dimensions: dimensions("Host", "c"), Value: new(10.0)},
			{MetricName: new("Latency.p"), Timestamp: &now, Dimensions: dimensions("Host", "d"), Value: new(1.0)},
			{MetricName: new("Latency.p"), Timestamp: &now, Value: new(20.0)},
			{
				MetricName: new("Latency.p"), Timestamp: &now, Dimensions: dimensions("Host", "a", "Env", "prod"),
				Value: new(30.0),
			},
			{
				MetricName: new("Latency.p"), Timestamp: &now, Dimensions: dimensions("Host", "a"),
				Values: []float64{3}, Counts: []float64{2},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	query := func(expr string, queries ...cwtypes.MetricDataQuery) (*cloudwatch.GetMetricDataOutput, error) {
		sql := cwtypes.MetricDataQuery{Id: new("q"), Expression: new(expr), Period: new(int32(60))}
		return c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
			MetricDataQueries: append([]cwtypes.MetricDataQuery{sql}, queries...),
			StartTime:         new(now.Add(-time.Minute)), EndTime: new(now.Add(time.Minute)),
		})
	}

	out, err := query(`select SUM("Latency.p") from SCHEMA("App/Prod", Host)
WHERE Host != 'c' AND Host != 'missing' GROUP BY Host ORDER BY MAX() DESC LIMIT 2`,
		cwtypes.MetricDataQuery{Id: new("total"), Expression: new("SUM(q)")})
	if err != nil || len(out.MetricDataResults) != 3 {
		t.Fatalf("grouped and reduced results: %+v %v", out, err)
	}

	for i, want := range []float64{9, 7, 16} {
		got := out.MetricDataResults[i]
		if !slices.Equal(got.Values, []float64{want}) ||
			aws.ToString(got.Label) != []string{"b", "a", "total"}[i] {
			t.Fatalf("result %d: %+v", i, got)
		}
	}

	out, err = query(`SELECT COUNT("Latency.p") FROM "App/Prod" GROUP BY Host`)
	if err != nil || len(out.MetricDataResults) != 5 || aws.ToString(out.MetricDataResults[0].Label) != "Other" ||
		!slices.Equal(out.MetricDataResults[1].Values, []float64{4}) {
		t.Fatalf("count with missing labels: %+v %v", out, err)
	}

	expr := `SELECT AVG("Latency.p") FROM SCHEMA("App/Prod", Host) WHERE Host='a'`
	out, err = query(expr)
	if err != nil || len(out.MetricDataResults) != 1 ||
		!slices.Equal(out.MetricDataResults[0].Values, []float64{7.0 / 3}) {
		t.Fatalf("weighted average: %+v %v", out, err)
	}

	alarm := &cloudwatch.PutMetricAlarmInput{
		AlarmName: new("fleet"), EvaluationPeriods: new(int32(1)), Threshold: new(2.0),
		ComparisonOperator: cwtypes.ComparisonOperatorGreaterThanThreshold,
		Metrics:            []cwtypes.MetricDataQuery{{Id: new("q"), Expression: new(expr), Period: new(int32(60))}},
	}
	if _, err = c.PutMetricAlarm(ctx, alarm); err != nil {
		t.Fatal(err)
	}

	waitFor(
		t,
		"the Metrics Insights alarm",
		func() bool { return alarmState(t, c, "fleet") == cwtypes.StateValueAlarm },
	)

	for _, unsupported := range []string{
		`SELECT MEDIAN("Latency.p") FROM "App/Prod"`,
		`SELECT AVG("Latency.p") FROM "App/Prod" WHERE Host LIKE 'a%'`,
		`SELECT AVG("Latency.p") FROM "App/Prod" WHERE tag.Environment='prod'`,
		`SELECT AVG("Latency.p") FROM "App/Prod" GROUP BY Host LIMIT 0`,
	} {
		_, err = query(unsupported)
		var invalid *cwtypes.InvalidParameterValueException
		if !errors.As(err, &invalid) {
			t.Fatalf("unsupported query %q: %v", unsupported, err)
		}
	}
}

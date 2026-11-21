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

func TestCloudWatchAnomalyDetection(t *testing.T) {
	ctx := t.Context()
	c := cloudwatchClient(server(t))
	now := time.Now().UTC().Truncate(time.Minute)
	dims := dimensions("Host", "api")
	_, err := c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: new("App"),
		MetricData: []cwtypes.MetricDatum{
			{MetricName: new("Load"), Dimensions: dims, Timestamp: new(now.Add(-3 * time.Minute)), Value: new(9.0)},
			{MetricName: new("Load"), Dimensions: dims, Timestamp: new(now.Add(-2 * time.Minute)), Value: new(11.0)},
			{MetricName: new("Load"), Dimensions: dims, Timestamp: new(now.Add(-time.Minute)), Value: new(100.0)},
			{MetricName: new("Load"), Dimensions: dims, Timestamp: &now, Value: new(30.0)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	input := &cloudwatch.PutAnomalyDetectorInput{
		SingleMetricAnomalyDetector: &cwtypes.SingleMetricAnomalyDetector{
			Namespace: new("App"), MetricName: new("Load"), Stat: new("Average"), Dimensions: dims,
		},
		Configuration: &cwtypes.AnomalyDetectorConfiguration{ExcludedTimeRanges: []cwtypes.Range{
			{StartTime: new(now.Add(-time.Minute)), EndTime: &now},
		}},
	}
	created, err := c.PutAnomalyDetector(ctx, input)
	if err != nil || aws.ToString(created.AnomalyDetectorId) == "" {
		t.Fatalf("creating detector: %+v %v", created, err)
	}

	list, err := c.DescribeAnomalyDetectors(ctx, &cloudwatch.DescribeAnomalyDetectorsInput{
		AnomalyDetectorIds: []string{*created.AnomalyDetectorId}, Dimensions: dims,
	})
	if err != nil || len(list.AnomalyDetectors) != 1 {
		t.Fatalf("describing detector: %+v %v", list, err)
	}

	d := list.AnomalyDetectors[0]
	if d.StateValue != cwtypes.AnomalyDetectorStateValueTrained || d.SingleMetricAnomalyDetector == nil ||
		aws.ToString(d.SingleMetricAnomalyDetector.MetricName) != "Load" ||
		d.Configuration == nil || len(d.Configuration.ExcludedTimeRanges) != 1 {
		t.Fatalf("detector: %+v", d)
	}

	metric := cwtypes.MetricDataQuery{Id: new("m"), MetricStat: &cwtypes.MetricStat{
		Metric: &cwtypes.Metric{Namespace: new("App"), MetricName: new("Load"), Dimensions: dims},
		Period: new(int32(60)), Stat: new("Average"),
	}}
	band := cwtypes.MetricDataQuery{Id: new("band"), Expression: new("ANOMALY_DETECTION_BAND(m)")}
	result, err := c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: &now, EndTime: new(now.Add(time.Minute)),
		MetricDataQueries: []cwtypes.MetricDataQuery{
			metric, band, {Id: new("upper"), Expression: new("LAST(ANOMALY_DETECTION_BAND(m, 2))")},
		},
	})
	if err != nil || len(result.MetricDataResults) != 4 ||
		!slices.Equal(result.MetricDataResults[1].Values, []float64{8}) ||
		!slices.Equal(result.MetricDataResults[2].Values, []float64{12}) ||
		!slices.Equal(result.MetricDataResults[3].Values, []float64{12}) {
		t.Fatalf("band bounds with the excluded 100 sample: %+v %v", result, err)
	}

	alarm := &cloudwatch.PutMetricAlarmInput{
		AlarmName: new("anomalous"), EvaluationPeriods: new(int32(1)), ThresholdMetricId: band.Id,
		ComparisonOperator: cwtypes.ComparisonOperatorLessThanLowerOrGreaterThanUpperThreshold,
		Metrics:            []cwtypes.MetricDataQuery{band, metric},
	}
	if _, err = c.PutMetricAlarm(ctx, alarm); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the anomaly alarm", func() bool { return alarmState(t, c, "anomalous") == cwtypes.StateValueAlarm })

	described, err := c.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{AlarmNames: []string{"anomalous"}})
	if err != nil || len(described.MetricAlarms) != 1 ||
		aws.ToString(
			described.MetricAlarms[0].ThresholdMetricId,
		) != "band" || described.MetricAlarms[0].Threshold != nil {
		t.Fatalf("described anomaly alarm: %+v %v", described, err)
	}

	alarm.ThresholdMetricId = new("m")
	_, err = c.PutMetricAlarm(ctx, alarm)
	var invalid *cwtypes.InvalidParameterValueException
	if !errors.As(err, &invalid) {
		t.Fatalf("expected an invalid threshold query error, got %v", err)
	}

	del := &cloudwatch.DeleteAnomalyDetectorInput{AnomalyDetectorId: created.AnomalyDetectorId}
	if _, err = c.DeleteAnomalyDetector(ctx, del); err != nil {
		t.Fatal(err)
	}

	list, err = c.DescribeAnomalyDetectors(ctx, &cloudwatch.DescribeAnomalyDetectorsInput{})
	if err != nil || len(list.AnomalyDetectors) != 0 {
		t.Fatalf("detector was not deleted: %+v %v", list, err)
	}
}

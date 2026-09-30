package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

func cloudwatchClient(s *httptest.Server) *cloudwatch.Client {
	return cloudwatch.New(cloudwatch.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func TestCloudWatchMetrics(t *testing.T) {
	c := cloudwatchClient(server(t))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	dims := []cwtypes.Dimension{{Name: aws.String("Service"), Value: aws.String("api")}}
	_, err := c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String("App"),
		MetricData: []cwtypes.MetricDatum{
			{MetricName: aws.String("Latency"), Dimensions: dims, Timestamp: &now, Value: aws.Float64(10)},
			{
				MetricName: aws.String("Latency"), Dimensions: dims, Timestamp: &now,
				Values: []float64{20, 30}, Counts: []float64{1, 2},
			},
			{MetricName: aws.String("Errors"), Timestamp: &now, Value: aws.Float64(1), Unit: cwtypes.StandardUnitCount},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stats, err := c.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace: aws.String("App"), MetricName: aws.String("Latency"), Dimensions: dims,
		StartTime: aws.Time(now.Add(-time.Minute)), EndTime: aws.Time(now.Add(time.Minute)), Period: aws.Int32(60),
		Statistics: []cwtypes.Statistic{
			cwtypes.StatisticSum, cwtypes.StatisticAverage, cwtypes.StatisticSampleCount, cwtypes.StatisticMaximum,
		},
		ExtendedStatistics: []string{"p50"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(stats.Datapoints) != 1 {
		t.Fatalf("datapoints: %+v", stats.Datapoints)
	}

	dp := stats.Datapoints[0]
	if aws.ToFloat64(dp.Sum) != 90 || aws.ToFloat64(dp.Average) != 22.5 || aws.ToFloat64(dp.SampleCount) != 4 ||
		aws.ToFloat64(dp.Maximum) != 30 || dp.ExtendedStatistics["p50"] != 20 || !dp.Timestamp.Equal(now) {
		t.Fatalf("datapoint: %+v", dp)
	}

	data, err := c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(now.Add(-time.Minute)), EndTime: aws.Time(now.Add(time.Minute)),
		MetricDataQueries: []cwtypes.MetricDataQuery{
			{Id: aws.String("latency"), ReturnData: aws.Bool(false), MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace:  aws.String("App"),
					MetricName: aws.String("Latency"),
					Dimensions: dims,
				},
				Period: aws.Int32(60),
				Stat:   aws.String("Maximum"),
			}},
			{Id: aws.String("errors"), MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{Namespace: aws.String("App"), MetricName: aws.String("Errors")},
				Period: aws.Int32(60), Stat: aws.String("Sum"),
			}},
			{Id: aws.String("doubled"), Expression: aws.String("latency * 2 + errors"), Label: aws.String("Doubled")},
			{Id: aws.String("total"), Expression: aws.String("SUM(METRICS())")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	results := map[string]cwtypes.MetricDataResult{}
	for _, r := range data.MetricDataResults {
		results[aws.ToString(r.Id)] = r
	}

	if _, ok := results["latency"]; ok || len(results) != 3 {
		t.Fatalf("results: %+v", data.MetricDataResults)
	}

	if r := results["doubled"]; aws.ToString(r.Label) != "Doubled" || !slices.Equal(r.Values, []float64{61}) ||
		!r.Timestamps[0].Equal(now) || r.StatusCode != cwtypes.StatusCodeComplete {
		t.Fatalf("doubled: %+v", r)
	}

	if r := results["total"]; !slices.Equal(r.Values, []float64{31}) {
		t.Fatalf("total: %+v", r)
	}

	listed, err := c.ListMetrics(ctx, &cloudwatch.ListMetricsInput{
		Namespace:  aws.String("App"),
		Dimensions: []cwtypes.DimensionFilter{{Name: aws.String("Service")}},
	})
	if err != nil || len(listed.Metrics) != 1 || aws.ToString(listed.Metrics[0].MetricName) != "Latency" {
		t.Fatalf("%+v %v", listed, err)
	}

	var invalid *cwtypes.InvalidParameterValueException
	_, err = c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace:  aws.String("AWS/EC2"),
		MetricData: []cwtypes.MetricDatum{{MetricName: aws.String("x"), Value: aws.Float64(1)}},
	})
	if !errors.As(err, &invalid) {
		t.Fatalf("reserved namespace: %v", err)
	}

	var combination *cwtypes.InvalidParameterCombinationException
	_, err = c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String("App"),
		MetricData: []cwtypes.MetricDatum{
			{MetricName: aws.String("x"), Value: aws.Float64(1), Values: []float64{2}},
		},
	})
	if !errors.As(err, &combination) {
		t.Fatalf("value and values: %v", err)
	}

	_, err = c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(now.Add(-time.Minute)), EndTime: aws.Time(now),
		MetricDataQueries: []cwtypes.MetricDataQuery{
			{Id: aws.String("e"), Expression: aws.String("ANOMALY_DETECTION_BAND(m)")},
		},
	})
	if !errors.As(err, &invalid) {
		t.Fatalf("unsupported function: %v", err)
	}
}

// alarmState reads an alarm's state.
func alarmState(t *testing.T, c *cloudwatch.Client, name string) cwtypes.StateValue {
	t.Helper()
	out, err := c.DescribeAlarms(context.Background(), &cloudwatch.DescribeAlarmsInput{
		AlarmNames: []string{name},
		AlarmTypes: []cwtypes.AlarmType{cwtypes.AlarmTypeMetricAlarm, cwtypes.AlarmTypeCompositeAlarm},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.MetricAlarms) == 1 {
		return out.MetricAlarms[0].StateValue
	}

	if len(out.CompositeAlarms) == 1 {
		return out.CompositeAlarms[0].StateValue
	}

	t.Fatalf("no alarm %s", name)
	return ""
}

func TestCloudWatchAlarms(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	q := sqsClient(s, "test")
	topics := snsClient(s, "test")
	ctx := context.Background()
	queueURL, queue := queueARN(t, q, "alarms")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("alarms")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &queue,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutMetricAlarm(ctx, &cloudwatch.PutMetricAlarmInput{
		AlarmName: aws.String("high"), Namespace: aws.String("App"), MetricName: aws.String("Load"),
		Statistic: cwtypes.StatisticMaximum, Period: aws.Int32(60), EvaluationPeriods: aws.Int32(1),
		Threshold: aws.Float64(5), ComparisonOperator: cwtypes.ComparisonOperatorGreaterThanThreshold,
		AlarmActions: []string{aws.ToString(topic.TopicArn)},
		Tags:         []cwtypes.Tag{{Key: aws.String("team"), Value: aws.String("ops")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := alarmState(t, c, "high"); got != cwtypes.StateValueInsufficientData {
		t.Fatalf("initial state %s", got)
	}

	_, err = c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace:  aws.String("App"),
		MetricData: []cwtypes.MetricDatum{{MetricName: aws.String("Load"), Value: aws.Float64(9)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the alarm", func() bool { return alarmState(t, c, "high") == cwtypes.StateValueAlarm })
	var message map[string]any
	if err = json.Unmarshal([]byte(receiveBody(t, q, queueURL)), &message); err != nil {
		t.Fatal(err)
	}

	if message["AlarmName"] != "high" || message["NewStateValue"] != "ALARM" ||
		message["OldStateValue"] != "INSUFFICIENT_DATA" {
		t.Fatalf("notification: %v", message)
	}

	_, err = c.PutCompositeAlarm(ctx, &cloudwatch.PutCompositeAlarmInput{
		AlarmName: aws.String("any"), AlarmRule: aws.String(`ALARM("high") OR FALSE`),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the composite alarm", func() bool { return alarmState(t, c, "any") == cwtypes.StateValueAlarm })
	children, err := c.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{ChildrenOfAlarmName: aws.String("any")})
	if err != nil || len(children.MetricAlarms) != 1 || aws.ToString(children.MetricAlarms[0].AlarmName) != "high" {
		t.Fatalf("children: %+v %v", children, err)
	}

	var invalid *cwtypes.InvalidParameterValueException
	_, err = c.PutCompositeAlarm(ctx, &cloudwatch.PutCompositeAlarmInput{
		AlarmName: aws.String("broken"), AlarmRule: aws.String(`ALARM("nothing")`),
	})
	if !errors.As(err, &invalid) {
		t.Fatalf("missing child: %v", err)
	}

	_, err = c.SetAlarmState(ctx, &cloudwatch.SetAlarmStateInput{
		AlarmName: aws.String("high"), StateValue: cwtypes.StateValueOk, StateReason: aws.String("testing"),
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)
	if got := alarmState(t, c, "high"); got != cwtypes.StateValueOk {
		t.Fatalf("a state set by hand changed to %s", got)
	}

	history, err := c.DescribeAlarmHistory(ctx, &cloudwatch.DescribeAlarmHistoryInput{
		AlarmName: aws.String("high"), HistoryItemType: cwtypes.HistoryItemTypeStateUpdate,
	})
	if err != nil || len(history.AlarmHistoryItems) != 2 ||
		aws.ToString(history.AlarmHistoryItems[0].HistorySummary) != "Alarm updated from ALARM to OK" {
		t.Fatalf("history: %+v %v", history, err)
	}

	actions, err := c.DescribeAlarmHistory(ctx, &cloudwatch.DescribeAlarmHistoryInput{
		AlarmName: aws.String("high"), HistoryItemType: cwtypes.HistoryItemTypeAction,
	})
	if err != nil || len(actions.AlarmHistoryItems) != 1 ||
		!strings.HasPrefix(aws.ToString(actions.AlarmHistoryItems[0].HistorySummary), "Successfully executed") {
		t.Fatalf("actions: %+v %v", actions, err)
	}

	if _, err = c.DisableAlarmActions(ctx, &cloudwatch.DisableAlarmActionsInput{
		AlarmNames: []string{"high"},
	}); err != nil {
		t.Fatal(err)
	}

	forMetric, err := c.DescribeAlarmsForMetric(ctx, &cloudwatch.DescribeAlarmsForMetricInput{
		Namespace: aws.String("App"), MetricName: aws.String("Load"),
	})
	if err != nil || len(forMetric.MetricAlarms) != 1 || aws.ToBool(forMetric.MetricAlarms[0].ActionsEnabled) {
		t.Fatalf("for metric: %+v %v", forMetric, err)
	}

	arn := forMetric.MetricAlarms[0].AlarmArn
	tags, err := c.ListTagsForResource(ctx, &cloudwatch.ListTagsForResourceInput{ResourceARN: arn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "ops" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	var notFound *cwtypes.ResourceNotFound
	if _, err = c.DeleteAlarms(ctx, &cloudwatch.DeleteAlarmsInput{
		AlarmNames: []string{"high", "nothing"},
	}); !errors.As(err, &notFound) {
		t.Fatalf("deleting a missing alarm: %v", err)
	}

	if _, err = c.DeleteAlarms(ctx, &cloudwatch.DeleteAlarmsInput{AlarmNames: []string{"high", "any"}}); err != nil {
		t.Fatal(err)
	}

	left, err := c.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{})
	if err != nil || len(left.MetricAlarms) != 0 {
		t.Fatalf("left: %+v %v", left, err)
	}
}

func TestCloudWatchDashboards(t *testing.T) {
	c := cloudwatchClient(server(t))
	ctx := context.Background()
	body := `{"widgets":[{"type":"text","x":0,"y":0,"width":6,"height":3,"properties":{"markdown":"hi"}}]}`
	if _, err := c.PutDashboard(ctx, &cloudwatch.PutDashboardInput{
		DashboardName: aws.String("main"), DashboardBody: aws.String(body),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetDashboard(ctx, &cloudwatch.GetDashboardInput{DashboardName: aws.String("main")})
	if err != nil || aws.ToString(got.DashboardBody) != body ||
		aws.ToString(got.DashboardArn) != "arn:aws:cloudwatch::000000000000:dashboard/main" {
		t.Fatalf("%+v %v", got, err)
	}

	list, err := c.ListDashboards(ctx, &cloudwatch.ListDashboardsInput{DashboardNamePrefix: aws.String("ma")})
	if err != nil || len(list.DashboardEntries) != 1 || aws.ToInt64(list.DashboardEntries[0].Size) != int64(len(body)) {
		t.Fatalf("%+v %v", list, err)
	}

	var invalid *cwtypes.DashboardInvalidInputError
	if _, err = c.PutDashboard(ctx, &cloudwatch.PutDashboardInput{
		DashboardName: aws.String("bad"), DashboardBody: aws.String("[]"),
	}); !errors.As(err, &invalid) {
		t.Fatalf("invalid body: %v", err)
	}

	if _, err = c.DeleteDashboards(ctx, &cloudwatch.DeleteDashboardsInput{
		DashboardNames: []string{"main"},
	}); err != nil {
		t.Fatal(err)
	}

	var notFound *cwtypes.DashboardNotFoundError
	if _, err = c.GetDashboard(ctx, &cloudwatch.GetDashboardInput{
		DashboardName: aws.String("main"),
	}); !errors.As(err, &notFound) {
		t.Fatalf("deleted dashboard: %v", err)
	}
}

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

func billingStatistics(t *testing.T, c *cloudwatch.Client, dims []cwtypes.Dimension) []cwtypes.Datapoint {
	t.Helper()
	now := time.Now().UTC()
	out, err := c.GetMetricStatistics(context.Background(), &cloudwatch.GetMetricStatisticsInput{
		Namespace: new("AWS/Billing"), MetricName: new("EstimatedCharges"), Dimensions: dims,
		StartTime: new(now.Add(-5 * time.Minute)), EndTime: new(now.Add(time.Minute)), Period: new(int32(60)),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum, cwtypes.StatisticMinimum},
		Unit:       cwtypes.StandardUnitNone,
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Datapoints
}

func putBillingFixture(t *testing.T, s *httptest.Server, amount string) {
	t.Helper()
	// A first-of-month line remains current even when its date is older than retention.
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"lines":[
{"id":"s3","start":%q,"end":%q,"service":"Amazon Simple Storage Service",
"unblendedCost":%q,"currency":"USD"},
{"id":"unknown","start":%q,"end":%q,"service":"unmapped service",
"unblendedCost":"0.2","currency":"USD"},
{"id":"credit","start":%q,"end":%q,"service":"Amazon Simple Storage Service",
"unblendedCost":"-0.1","currency":"USD"}]}`,
		start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339), amount,
		start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339),
		start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339))
	fixtureRequest(t, s, "PUT", "costs/fixtures/billing", body, 200)
}

func TestCloudWatchBilling(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	ctx := context.Background()
	dims := []cwtypes.Dimension{{Name: new("Currency"), Value: new("USD")}}
	if points := billingStatistics(t, c, dims); len(points) != 0 {
		t.Fatalf("unseeded account has billing data: %+v", points)
	}

	q := sqsClient(s, "test")
	topics := snsClient(s, "test")
	queueURL, queue := queueARN(t, q, "billing")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("billing")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: new(queue),
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutMetricAlarm(ctx, &cloudwatch.PutMetricAlarmInput{
		AlarmName: new("billing"), Namespace: new("AWS/Billing"), MetricName: new("EstimatedCharges"),
		Dimensions: dims, Statistic: cwtypes.StatisticMaximum, Unit: cwtypes.StandardUnitNone,
		Period: new(int32(60)), EvaluationPeriods: new(int32(1)), DatapointsToAlarm: new(int32(1)),
		Threshold: new(80.0), ComparisonOperator: cwtypes.ComparisonOperatorGreaterThanThreshold,
		AlarmActions: []string{aws.ToString(topic.TopicArn)},
	})
	if err != nil {
		t.Fatal(err)
	}

	regionOptions := c.Options()
	regionOptions.Region = "eu-west-1"
	otherRegion := cloudwatch.New(regionOptions)
	_, err = otherRegion.PutMetricAlarm(ctx, &cloudwatch.PutMetricAlarmInput{
		AlarmName: new("billing"), Namespace: new("AWS/Billing"), MetricName: new("EstimatedCharges"),
		Dimensions: dims, Statistic: cwtypes.StatisticMaximum, Period: new(int32(60)),
		EvaluationPeriods: new(int32(1)), Threshold: new(80.0),
		ComparisonOperator: cwtypes.ComparisonOperatorGreaterThanThreshold,
	})
	if err != nil {
		t.Fatal(err)
	}

	putBillingFixture(t, s, "81")
	waitFor(t, "billing statistics", func() bool {
		for _, p := range billingStatistics(t, c, dims) {
			if aws.ToFloat64(p.Maximum) == 81.1 && p.Unit == cwtypes.StandardUnitNone {
				return true
			}
		}

		return false
	})
	waitFor(t, "billing alarm", func() bool { return alarmState(t, c, "billing") == cwtypes.StateValueAlarm })
	var message struct{ AlarmName, NewStateValue string }
	if err = json.Unmarshal([]byte(receiveBody(t, q, queueURL)), &message); err != nil {
		t.Fatal(err)
	}

	if message.AlarmName != "billing" || message.NewStateValue != "ALARM" {
		t.Fatalf("billing notification: %+v", message)
	}

	history, err := c.DescribeAlarmHistory(ctx, &cloudwatch.DescribeAlarmHistoryInput{
		AlarmName: new("billing"), HistoryItemType: cwtypes.HistoryItemTypeStateUpdate,
	})
	if err != nil || len(history.AlarmHistoryItems) == 0 {
		t.Fatalf("billing history: %+v %v", history, err)
	}

	listed, err := c.ListMetrics(ctx, &cloudwatch.ListMetricsInput{Namespace: new("AWS/Billing")})
	if err != nil || len(listed.Metrics) != 2 {
		t.Fatalf("billing metrics: %+v %v", listed, err)
	}

	for _, metric := range listed.Metrics {
		if aws.ToString(metric.MetricName) != "EstimatedCharges" {
			t.Fatalf("unexpected metric: %+v", metric)
		}

		foundCurrency, foundService := false, false
		for _, d := range metric.Dimensions {
			switch aws.ToString(d.Name) {
			case "Currency":
				foundCurrency = aws.ToString(d.Value) == "USD"
			case "ServiceName":
				foundService = aws.ToString(d.Value) == "AmazonS3"
			default:
				t.Fatalf("unexpected dimension: %+v", d)
			}
		}

		if !foundCurrency || (len(metric.Dimensions) != 1 && (len(metric.Dimensions) != 2 || !foundService)) {
			t.Fatalf("billing dimensions: %+v", metric.Dimensions)
		}
	}

	serviceDims := append([]cwtypes.Dimension{{Name: new("ServiceName"), Value: new("AmazonS3")}}, dims...)
	points := billingStatistics(t, c, serviceDims)
	if len(points) == 0 || aws.ToFloat64(points[0].Maximum) != 80.9 {
		t.Fatalf("service charges: %+v", points)
	}

	if points := billingStatistics(t, otherRegion, dims); len(points) != 0 {
		t.Fatalf("other Region has billing data: %+v", points)
	}

	if state := alarmState(t, otherRegion, "billing"); state != cwtypes.StateValueInsufficientData {
		t.Fatalf("other Region alarm: %s", state)
	}

	options := c.Options()
	options.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"}, nil
	})
	otherAccount := cloudwatch.New(options)
	if points := billingStatistics(t, otherAccount, dims); len(points) != 0 {
		t.Fatalf("other account has billing data: %+v", points)
	}

	_, err = c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace:  new("AWS/Billing"),
		MetricData: []cwtypes.MetricDatum{{MetricName: new("EstimatedCharges"), Value: new(999.0), Dimensions: dims}},
	})
	var invalid *cwtypes.InvalidParameterValueException
	if !errors.As(err, &invalid) {
		t.Fatalf("public billing publication: %v", err)
	}

	putBillingFixture(t, s, "91")
	waitFor(t, "replaced billing fixture", func() bool {
		out, getErr := c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
			StartTime: new(time.Now().Add(-5 * time.Minute)), EndTime: new(time.Now().Add(time.Minute)),
			MetricDataQueries: []cwtypes.MetricDataQuery{{Id: new("charges"), MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace:  new("AWS/Billing"),
					MetricName: new("EstimatedCharges"),
					Dimensions: dims,
				},
				Period: new(int32(60)),
				Stat:   new("Maximum"),
				Unit:   cwtypes.StandardUnitNone,
			}}},
		})
		if getErr != nil {
			t.Fatal(getErr)
		}

		for _, result := range out.MetricDataResults {
			for _, v := range result.Values {
				if v == 91.1 {
					return true
				}
			}
		}

		return false
	})
	fixtureRequest(t, s, "DELETE", "costs/fixtures/billing", "", 204)
	waitFor(t, "final zero billing sample", func() bool {
		for _, p := range billingStatistics(t, c, dims) {
			if aws.ToFloat64(p.Minimum) == 0 {
				return true
			}
		}

		return false
	})
	fixtureRequest(t, s, "POST", "reset", "", 200)
	// Wait across a worker tick: reset must not be followed by a resurrected sample.
	time.Sleep(1100 * time.Millisecond)
	if points := billingStatistics(t, c, dims); len(points) != 0 {
		t.Fatalf("billing data after reset: %+v", points)
	}

	listed, err = c.ListMetrics(ctx, &cloudwatch.ListMetricsInput{Namespace: new("AWS/Billing")})
	if err != nil || len(listed.Metrics) != 0 {
		t.Fatalf("billing metrics after reset: %+v %v", listed, err)
	}
}

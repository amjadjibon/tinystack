package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestConsoleCloudWatchMetricsAlarmsAndDashboards(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation,
			account, region, input), http.StatusOK)["output"].(map[string]any)
	}
	watch := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return call("cloudwatch", operation, input)
	}
	options := cloudwatchClient(srv).Options()
	options.Region = region
	client := cloudwatch.New(options)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	dimensions := []any{map[string]any{"Name": "Service", "Value": "api"}}
	watch("PutMetricData", map[string]any{"Namespace": "Console", "MetricData": []any{
		map[string]any{"MetricName": "Latency", "Dimensions": dimensions, "Value": 12, "Timestamp": now.Unix()},
		map[string]any{
			"MetricName": "Latency",
			"Dimensions": dimensions,
			"Value":      20,
			"Timestamp":  now.Add(-time.Minute).Unix(),
		},
	}})
	metrics := watch("ListMetrics", map[string]any{})["Metrics"].([]any)
	if len(metrics) != 1 || metrics[0].(map[string]any)["MetricName"] != "Latency" {
		t.Fatal(metrics)
	}
	metric := map[string]any{"Namespace": "Console", "MetricName": "Latency", "Dimensions": dimensions}
	query := map[string]any{
		"Id":         "m0",
		"ReturnData": false,
		"MetricStat": map[string]any{"Metric": metric, "Period": 60, "Stat": "Average"},
	}
	result := watch("GetMetricData", map[string]any{
		"StartTime": now.Add(-2 * time.Minute).Unix(),
		"EndTime":   now.Add(time.Minute).Unix(), "ScanBy": "TimestampAscending",
		"MetricDataQueries": []any{query, map[string]any{"Id": "e0", "Expression": "m0 * 2", "Label": "Doubled"}},
	})["MetricDataResults"].([]any)
	values := result[0].(map[string]any)["Values"].([]any)
	if len(values) != 2 || values[0] != float64(40) || values[1] != float64(24) {
		t.Fatal(result)
	}
	partial := watch("GetMetricData", map[string]any{
		"StartTime": now.Add(-2 * time.Minute).Unix(),
		"EndTime":   now.Add(time.Minute).Unix(), "MaxDatapoints": 1,
		"MetricDataQueries": []any{map[string]any{
			"Id": "m0", "MetricStat": map[string]any{"Metric": metric, "Period": 60, "Stat": "Average"},
		}},
	})["MetricDataResults"].([]any)[0].(map[string]any)
	if partial["StatusCode"] != "PartialData" || len(partial["Values"].([]any)) != 1 {
		t.Fatal(partial)
	}
	if len(watch("GetMetricStatistics", map[string]any{
		"Namespace": "Console", "MetricName": "Latency",
		"Dimensions": dimensions, "StartTime": now.Add(-2 * time.Minute).Unix(), "EndTime": now.Add(time.Minute).Unix(),
		"Period": 60, "Statistics": []string{"Average"},
	})["Datapoints"].([]any)) != 2 {
		t.Fatal("statistics missing")
	}

	queue := call("sqs", "CreateQueue", map[string]any{"QueueName": "console-alarm-actions"})["QueueUrl"].(string)
	topic := call("sns", "CreateTopic", map[string]any{"Name": "console-alarm-actions"})["TopicArn"].(string)
	call("sns", "Subscribe", map[string]any{
		"TopicArn": topic, "Protocol": "sqs",
		"Endpoint": "arn:aws:sqs:" + region + ":" + account + ":console-alarm-actions",
	})
	alarm := map[string]any{
		"AlarmName":          "latency",
		"Namespace":          "Console",
		"MetricName":         "Latency",
		"Dimensions":         dimensions,
		"Statistic":          "Average",
		"Period":             60,
		"EvaluationPeriods":  1,
		"Threshold":          100,
		"ComparisonOperator": "GreaterThanThreshold",
		"AlarmActions":       []string{topic},
		"ActionsEnabled":     false,
		"Tags":               []any{map[string]any{"Key": "team", "Value": "console"}},
	}
	watch("PutMetricAlarm", alarm)
	watch("PutCompositeAlarm", map[string]any{"AlarmName": "combined", "AlarmRule": `ALARM("latency")`})
	all := watch(
		"DescribeAlarms",
		map[string]any{"AlarmTypes": []string{"MetricAlarm", "CompositeAlarm"}, "MaxRecords": 1},
	)
	if all["NextToken"] == nil {
		t.Fatal("alarm pagination missing")
	}
	next := watch(
		"DescribeAlarms",
		map[string]any{
			"AlarmTypes": []string{"MetricAlarm", "CompositeAlarm"},
			"NextToken":  all["NextToken"],
			"MaxRecords": 1,
		},
	)
	if len(next["MetricAlarms"].([]any))+len(next["CompositeAlarms"].([]any)) != 1 {
		t.Fatal(next)
	}
	forMetric := watch("DescribeAlarmsForMetric", map[string]any{
		"Namespace": "Console", "MetricName": "Latency", "Dimensions": dimensions,
	})
	if len(forMetric["MetricAlarms"].([]any)) != 1 {
		t.Fatal("metric alarm missing")
	}
	watch("EnableAlarmActions", map[string]any{"AlarmNames": []string{"latency"}})
	watch(
		"SetAlarmState",
		map[string]any{"AlarmName": "latency", "StateValue": "ALARM", "StateReason": "console action test"},
	)
	sqsOptions := sqsClient(srv, "test").Options()
	sqsOptions.Region = region
	received, err := sqs.New(sqsOptions).ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: new(queue)})
	if err != nil || len(received.Messages) != 1 {
		t.Fatalf("SNS alarm action: %+v %v", received, err)
	}
	watch("DisableAlarmActions", map[string]any{"AlarmNames": []string{"latency"}})
	read, err := client.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{AlarmNames: []string{"latency"}})
	if err != nil || len(read.MetricAlarms) != 1 || aws.ToBool(read.MetricAlarms[0].ActionsEnabled) {
		t.Fatalf("%+v %v", read, err)
	}
	history := watch("DescribeAlarmHistory", map[string]any{"AlarmName": "latency", "MaxRecords": 1})
	if history["NextToken"] == nil {
		t.Fatal(history)
	}
	older := watch("DescribeAlarmHistory", map[string]any{
		"AlarmName": "latency", "NextToken": history["NextToken"], "MaxRecords": 1,
	})
	if len(older["AlarmHistoryItems"].([]any)) != 1 {
		t.Fatal("history next page missing")
	}
	arn := "arn:aws:cloudwatch:" + region + ":" + account + ":alarm:latency"
	watch(
		"TagResource",
		map[string]any{"ResourceARN": arn, "Tags": []any{map[string]any{"Key": "owner", "Value": "dev"}}},
	)
	watch("UntagResource", map[string]any{"ResourceARN": arn, "TagKeys": []string{"team"}})
	if len(watch("ListTagsForResource", map[string]any{"ResourceARN": arn})["Tags"].([]any)) != 1 {
		t.Fatal("tags missing")
	}
	alarm["Threshold"] = 200
	watch("PutMetricAlarm", alarm)
	tagged, err := client.ListTagsForResource(ctx, &cloudwatch.ListTagsForResourceInput{ResourceARN: new(arn)})
	if err != nil || len(tagged.Tags) != 1 || aws.ToString(tagged.Tags[0].Key) != "owner" {
		t.Fatalf("tags on edit: %+v %v", tagged, err)
	}

	body := `{"widgets":[{"type":"metric","properties":{"metrics":[["Console","Latency","Service","api"]],"period":60}}]}`
	watch("PutDashboard", map[string]any{"DashboardName": "console", "DashboardBody": body})
	if watch("GetDashboard", map[string]any{"DashboardName": "console"})["DashboardBody"] != body {
		t.Fatal("dashboard body changed")
	}
	if len(watch("ListDashboards", map[string]any{})["DashboardEntries"].([]any)) != 1 {
		t.Fatal("dashboard inventory")
	}
	dashboardARN := "arn:aws:cloudwatch::" + account + ":dashboard/console"
	watch(
		"TagResource",
		map[string]any{"ResourceARN": dashboardARN, "Tags": []any{map[string]any{"Key": "owner", "Value": "dev"}}},
	)

	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		req := consoleLambdaRequest(t, srv, "cloudwatch", "ListMetrics", scope.account, scope.region, map[string]any{})
		output := consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
		if len(output["Metrics"].([]any)) != 0 {
			t.Fatal("cross-scope metrics", output)
		}
		consoleResponse(t, srv, consoleLambdaRequest(t, srv, "cloudwatch", "GetDashboard", scope.account, scope.region,
			map[string]any{"DashboardName": "console"}), http.StatusNotFound)
	}
	watch("DeleteDashboards", map[string]any{"DashboardNames": []string{"console"}})
	watch("DeleteAlarms", map[string]any{"AlarmNames": []string{"combined", "latency"}})
	left, err := client.DescribeAlarms(
		ctx,
		&cloudwatch.DescribeAlarmsInput{
			AlarmTypes: []cwtypes.AlarmType{cwtypes.AlarmTypeMetricAlarm, cwtypes.AlarmTypeCompositeAlarm},
		},
	)
	if err != nil || len(left.MetricAlarms)+len(left.CompositeAlarms) != 0 {
		t.Fatalf("%+v %v", left, err)
	}
}

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestConsoleEventBridgeLifecycleAndDelivery(t *testing.T) {
	srv := server(t)
	const account, region, bus, rule = "000000000000", "eu-west-1", "console-bus", "console-rule"
	write := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation,
			account, region, input), http.StatusOK)["output"].(map[string]any)
	}
	event := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return write("eventbridge", operation, input)
	}
	queue := write("sqs", "CreateQueue", map[string]any{"QueueName": "console-events"})["QueueUrl"].(string)
	queueARN := "arn:aws:sqs:" + region + ":" + account + ":console-events"
	created := event("CreateEventBus", map[string]any{"Name": bus, "Description": "console", "Tags": []any{
		map[string]any{"Key": "team", "Value": "dev"},
	}})
	arn := created["EventBusArn"].(string)
	event("UpdateEventBus", map[string]any{"Name": bus, "Description": "updated"})
	if event("DescribeEventBus", map[string]any{"Name": bus})["Description"] != "updated" {
		t.Fatal("description not updated")
	}
	if len(event("ListEventBuses", map[string]any{})["EventBuses"].([]any)) != 2 {
		t.Fatal("default or custom bus missing")
	}
	pattern := `{"source":["my.app"],"detail":{"kind":["created"]}}`
	ruleARN := event("PutRule", map[string]any{
		"Name": rule, "EventBusName": bus, "EventPattern": pattern, "State": "ENABLED",
	})["RuleArn"]
	if event("TestEventPattern", map[string]any{
		"EventPattern": pattern, "Event": `{"source":"my.app","detail":{"kind":"created"}}`,
	})["Result"] != true {
		t.Fatal("pattern should match")
	}
	if event("TestEventPattern", map[string]any{
		"EventPattern": pattern, "Event": `{"source":"other","detail":{"kind":"created"}}`,
	})["Result"] != false {
		t.Fatal("pattern should not match")
	}
	event("PutTargets", map[string]any{"Rule": rule, "EventBusName": bus, "Targets": []any{
		map[string]any{
			"Id":          "queue",
			"Arn":         queueARN,
			"InputPath":   "$.detail",
			"RetryPolicy": map[string]any{"MaximumRetryAttempts": 0},
		},
	}})
	targets := event("ListTargetsByRule", map[string]any{"Rule": rule, "EventBusName": bus})["Targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["InputPath"] != "$.detail" {
		t.Fatal(targets)
	}
	event(
		"TagResource",
		map[string]any{"ResourceARN": ruleARN, "Tags": []any{map[string]any{"Key": "owner", "Value": "console"}}},
	)
	event(
		"TagResource",
		map[string]any{"ResourceARN": arn, "Tags": []any{map[string]any{"Key": "stage", "Value": "test"}}},
	)
	event("UntagResource", map[string]any{"ResourceARN": arn, "TagKeys": []string{"team"}})
	if len(event("ListTagsForResource", map[string]any{"ResourceARN": arn})["Tags"].([]any)) != 1 {
		t.Fatal("bus tags not updated")
	}
	detail := `{"kind":"created","message":"hello 世界","precise":900719925474099312345}`
	result := event("PutEvents", map[string]any{"Entries": []any{
		map[string]any{"EventBusName": bus, "Source": "my.app", "DetailType": "Created", "Detail": detail},
		map[string]any{"EventBusName": bus, "Source": "my.app", "DetailType": "Broken", "Detail": "{"},
	}})
	entries := result["Entries"].([]any)
	if result["FailedEntryCount"] != float64(1) || entries[0].(map[string]any)["EventId"] == nil ||
		entries[1].(map[string]any)["ErrorCode"] != "MalformedDetail" {
		t.Fatal(result)
	}
	options := sqsClient(srv, "test").Options()
	options.Region = region
	client := sqs.New(options)
	got, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: new(queue)})
	if err != nil || len(got.Messages) != 1 || aws.ToString(got.Messages[0].Body) != detail {
		t.Fatalf("delivered = %+v, %v", got, err)
	}
	event("DisableRule", map[string]any{"Name": rule, "EventBusName": bus})
	if event("DescribeRule", map[string]any{"Name": rule, "EventBusName": bus})["State"] != "DISABLED" {
		t.Fatal("rule not disabled")
	}
	event(
		"PutEvents",
		map[string]any{
			"Entries": []any{
				map[string]any{"EventBusName": bus, "Source": "my.app", "DetailType": "Created", "Detail": detail},
			},
		},
	)
	got, err = client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: new(queue)})
	if err != nil || len(got.Messages) != 0 {
		t.Fatalf("disabled rule delivered = %+v, %v", got, err)
	}
	event("EnableRule", map[string]any{"Name": rule, "EventBusName": bus})
	event("PutRule", map[string]any{"Name": "scheduled", "ScheduleExpression": "rate(1 minute)", "State": "DISABLED"})
	if event("DescribeRule", map[string]any{"Name": "scheduled"})["ScheduleExpression"] != "rate(1 minute)" {
		t.Fatal("schedule missing")
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(
				t,
				srv,
				"eventbridge",
				"DescribeEventBus",
				scope.account,
				scope.region,
				map[string]any{"Name": bus},
			),
			http.StatusBadRequest,
		)
	}
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "eventbridge", "DeleteEventBus", account, region, map[string]any{"Name": bus}),
		http.StatusBadRequest,
	)
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"eventbridge",
			"DeleteRule",
			account,
			region,
			map[string]any{"Name": rule, "EventBusName": bus},
		),
		http.StatusBadRequest,
	)
	event("RemoveTargets", map[string]any{"Rule": rule, "EventBusName": bus, "Ids": []string{"queue"}})
	event("DeleteRule", map[string]any{"Name": rule, "EventBusName": bus})
	event("DeleteRule", map[string]any{"Name": "scheduled"})
	event("DeleteEventBus", map[string]any{"Name": bus})
}

func TestConsoleCloudWatchLogsLifecyclePagingAndInsights(t *testing.T) {
	srv := server(t)
	const account, region, group, stream = "000000000000", "eu-west-1", "/console/logs", "app/世界"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, "logs", operation,
			account, region, input), http.StatusOK)["output"].(map[string]any)
	}
	scoped := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		input["LogGroupName"] = group
		return call(operation, input)
	}
	scoped("CreateLogGroup", map[string]any{"LogGroupClass": "STANDARD", "Tags": map[string]any{"team": "dev"}})
	scoped("PutRetentionPolicy", map[string]any{"RetentionInDays": 7})
	scoped("CreateLogStream", map[string]any{"LogStreamName": stream})
	scoped("CreateLogStream", map[string]any{"LogStreamName": "other"})
	now := time.Now().UnixMilli()
	scoped("PutLogEvents", map[string]any{"LogStreamName": stream, "LogEvents": []any{
		map[string]any{"Timestamp": now - 2000, "Message": "ERROR first 世界"},
		map[string]any{"Timestamp": now - 1000, "Message": "DEBUG second"},
		map[string]any{"Timestamp": now, "Message": "ERROR third"},
	}})
	listed := call("DescribeLogGroups", map[string]any{"LogGroupNamePrefix": group})["LogGroups"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["RetentionInDays"] != float64(7) {
		t.Fatal(listed)
	}
	streams := scoped("DescribeLogStreams", map[string]any{"Limit": 1})
	if len(streams["LogStreams"].([]any)) != 1 || streams["NextToken"] == nil {
		t.Fatal(streams)
	}
	if len(
		scoped("DescribeLogStreams", map[string]any{"Limit": 1, "NextToken": streams["NextToken"]})["LogStreams"].([]any),
	) != 1 {
		t.Fatal("missing stream continuation")
	}
	first := scoped("FilterLogEvents", map[string]any{"FilterPattern": "ERROR -DEBUG", "Limit": 1})
	if first["Events"].([]any)[0].(map[string]any)["Message"] != "ERROR first 世界" || first["NextToken"] == nil {
		t.Fatal(first)
	}
	second := scoped(
		"FilterLogEvents",
		map[string]any{"FilterPattern": "ERROR -DEBUG", "Limit": 1, "NextToken": first["NextToken"]},
	)
	if second["Events"].([]any)[0].(map[string]any)["Message"] != "ERROR third" || second["NextToken"] != nil {
		t.Fatal(second)
	}
	latest := scoped("GetLogEvents", map[string]any{"LogStreamName": stream, "Limit": 1, "StartFromHead": false})
	if latest["Events"].([]any)[0].(map[string]any)["Message"] != "ERROR third" {
		t.Fatal(latest)
	}
	older := scoped(
		"GetLogEvents",
		map[string]any{"LogStreamName": stream, "Limit": 1, "NextToken": latest["NextBackwardToken"]},
	)
	if older["Events"].([]any)[0].(map[string]any)["Message"] != "DEBUG second" {
		t.Fatal(older)
	}
	end := scoped(
		"GetLogEvents",
		map[string]any{"LogStreamName": stream, "Limit": 1, "NextToken": latest["NextForwardToken"]},
	)
	if len(end["Events"].([]any)) != 0 || end["NextForwardToken"] != latest["NextForwardToken"] {
		t.Fatal(end)
	}
	arn := "arn:aws:logs:" + region + ":" + account + ":log-group:" + group
	call("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]any{"stage": "test"}})
	call("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})
	if call("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any)["stage"] != "test" {
		t.Fatal("tags missing")
	}
	query := scoped(
		"StartQuery",
		map[string]any{
			"StartTime":   now/1000 - 10,
			"EndTime":     now/1000 + 1,
			"QueryString": "filter @message like /ERROR/ | stats count(*) as errors",
		},
	)
	result := call("GetQueryResults", map[string]any{"QueryId": query["QueryId"]})
	if result["Status"] != "Complete" {
		t.Fatal(result)
	}
	fields := result["Results"].([]any)[0].([]any)
	if fields[0].(map[string]any)["Value"] != "2" {
		t.Fatal(fields)
	}
	if len(scoped("DescribeQueries", map[string]any{})["Queries"].([]any)) != 1 {
		t.Fatal("query history missing")
	}
	rejected := scoped(
		"PutLogEvents",
		map[string]any{
			"LogStreamName": stream,
			"LogEvents":     []any{map[string]any{"Timestamp": now - 15*86400000, "Message": "expired"}},
		},
	)
	if rejected["RejectedLogEventsInfo"] == nil {
		t.Fatal("expired event not rejected")
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(
				t,
				srv,
				"logs",
				"GetLogEvents",
				scope.account,
				scope.region,
				map[string]any{"LogGroupName": group, "LogStreamName": stream},
			),
			http.StatusBadRequest,
		)
	}
	scoped("DeleteRetentionPolicy", map[string]any{})
	scoped("DeleteLogStream", map[string]any{"LogStreamName": stream})
	scoped("DeleteLogGroup", map[string]any{})
}

func TestConsoleRegionalActionsValidationAndSignatures(t *testing.T) {
	srv := server(t)
	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["eventbridge"] != true ||
		capabilities["logs"] != true || capabilities["cloudwatch"] != true ||
		capabilities["apigateway"] != true || capabilities["apigatewayv2"] != true || capabilities["sfn"] != true {
		t.Fatal(capabilities)
	}
	for _, service := range []string{"eventbridge", "logs", "cloudwatch", "apigateway", "apigatewayv2", "sfn"} {
		operation := "ListEventBuses"
		if service == "logs" {
			operation = "DescribeLogGroups"
		}
		if service == "cloudwatch" {
			operation = "ListMetrics"
		}
		if service == "apigateway" {
			operation = "GetRestApis"
		}
		if service == "apigatewayv2" {
			operation = "GetApis"
		}
		if service == "sfn" {
			operation = "ListStateMachines"
		}
		for _, region := range []string{"", "../us-east-1"} {
			consoleResponse(
				t,
				srv,
				consoleLambdaRequest(t, srv, service, operation, "000000000000", region, map[string]any{}),
				http.StatusBadRequest,
			)
		}
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(
				t,
				srv,
				service,
				operation,
				"000000000000",
				"us-east-1",
				map[string]any{"Unknown": true},
			),
			http.StatusBadRequest,
		)
		req := consoleLambdaRequest(t, srv, service, operation, "000000000000", "us-east-1", map[string]any{})
		req.Header.Set("Origin", "https://evil.example")
		consoleResponse(t, srv, req, http.StatusForbidden)
	}
	verified := verifyingServer(t)
	for _, tc := range []struct{ service, signing, operation string }{
		{"eventbridge", "events", "ListEventBuses"},
		{"logs", "logs", "DescribeLogGroups"},
		{"cloudwatch", "monitoring", "ListMetrics"},
		{"apigateway", "apigateway", "GetRestApis"},
		{"apigatewayv2", "apigateway", "GetApis"},
		{"sfn", "states", "ListStateMachines"},
	} {
		for _, mode := range []string{"unsigned", "wrong-service", "wrong-region", "wrong-account", "valid"} {
			account, region, signing := "111111111111", "us-east-1", tc.signing
			if mode == "wrong-account" {
				account = "000000000000"
			}
			if mode == "wrong-region" {
				region = "eu-west-1"
			}
			if mode == "wrong-service" {
				signing = "lambda"
			}
			req := consoleLambdaRequest(t, verified, tc.service, tc.operation, account, "us-east-1", map[string]any{})
			if mode != "unsigned" {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				sum := sha256.Sum256(body)
				if err = v4.NewSigner().
					SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"},
						req, hex.EncodeToString(sum[:]), signing, region, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			status := http.StatusForbidden
			if mode == "valid" {
				status = http.StatusOK
			}
			response := consoleResponse(t, verified, req, status)
			if mode == "valid" {
				if _, err := json.Marshal(response["output"]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

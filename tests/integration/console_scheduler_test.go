package integration_test

import (
	"net/http"
	"testing"
	"time"
)

func TestConsoleSchedulerLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	callStatus := func(service, operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, region, input), status)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callStatus("scheduler", operation, input, http.StatusOK)["output"].(map[string]any)
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		!consolePage(capabilities, "scheduler") {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	queue := callStatus("sqs", "CreateQueue", map[string]any{"QueueName": "console-jobs"},
		http.StatusOK)["output"].(map[string]any)
	if queue["QueueUrl"] == nil {
		t.Fatal(queue)
	}

	const queueARN = "arn:aws:sqs:eu-west-1:000000000000:console-jobs"
	const roleARN = "arn:aws:iam::000000000000:role/scheduler-role"
	const groupARN = "arn:aws:scheduler:eu-west-1:000000000000:schedule-group/console-group"

	call("CreateScheduleGroup", map[string]any{
		"Name": "console-group", "Tags": []map[string]any{{"Key": "env", "Value": "dev"}},
	})
	call("TagResource", map[string]any{
		"ResourceArn": groupARN, "Tags": []map[string]any{{"Key": "team", "Value": "core"}},
	})
	tags := call("ListTagsForResource", map[string]any{"ResourceArn": groupARN})["Tags"].([]any)
	if len(tags) != 2 {
		t.Fatal(tags)
	}

	call("UntagResource", map[string]any{"ResourceArn": groupARN, "TagKeys": []string{"team"}})
	tags = call("ListTagsForResource", map[string]any{"ResourceArn": groupARN})["Tags"].([]any)
	if len(tags) != 1 {
		t.Fatal(tags)
	}

	groups := call("ListScheduleGroups", map[string]any{})["ScheduleGroups"].([]any)
	if len(groups) != 2 {
		t.Fatal(groups)
	}

	schedule := map[string]any{
		"Name": "nightly", "GroupName": "console-group", "ScheduleExpression": "rate(1 hour)",
		"ScheduleExpressionTimezone": "UTC", "State": "ENABLED", "Description": "console",
		"FlexibleTimeWindow": map[string]any{"Mode": "OFF"},
		"EndDate":            float64(time.Now().Add(240 * time.Hour).Unix()),
		"Target": map[string]any{
			"Arn": queueARN, "RoleArn": roleARN, "Input": `{"job":"nightly"}`,
			"RetryPolicy": map[string]any{"MaximumRetryAttempts": 3},
		},
	}
	call("CreateSchedule", schedule)

	got := call("GetSchedule", map[string]any{"Name": "nightly", "GroupName": "console-group"})
	if got["ScheduleExpression"] != "rate(1 hour)" || got["State"] != "ENABLED" ||
		got["Target"].(map[string]any)["Input"] != `{"job":"nightly"}` {
		t.Fatal(got)
	}

	schedule["ScheduleExpression"] = "cron(0 12 * * ? *)"
	schedule["State"] = "DISABLED"
	schedule["FlexibleTimeWindow"] = map[string]any{"Mode": "FLEXIBLE", "MaximumWindowInMinutes": 15}
	call("UpdateSchedule", schedule)

	got = call("GetSchedule", map[string]any{"Name": "nightly", "GroupName": "console-group"})
	if got["ScheduleExpression"] != "cron(0 12 * * ? *)" || got["State"] != "DISABLED" ||
		got["FlexibleTimeWindow"].(map[string]any)["MaximumWindowInMinutes"] != float64(15) {
		t.Fatal(got)
	}

	listed := call("ListSchedules", map[string]any{"GroupName": "console-group"})["Schedules"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["State"] != "DISABLED" {
		t.Fatal(listed)
	}

	callStatus("scheduler", "CreateSchedule", schedule, http.StatusConflict)
	callStatus("scheduler", "DeleteScheduleGroup", map[string]any{"Name": "default"}, http.StatusBadRequest)

	call("DeleteSchedule", map[string]any{"Name": "nightly", "GroupName": "console-group"})
	callStatus("scheduler", "GetSchedule", map[string]any{"Name": "nightly", "GroupName": "console-group"},
		http.StatusNotFound)

	call("CreateSchedule", schedule)
	call("DeleteScheduleGroup", map[string]any{"Name": "console-group"})
	callStatus("scheduler", "GetSchedule", map[string]any{"Name": "nightly", "GroupName": "console-group"},
		http.StatusNotFound)
}

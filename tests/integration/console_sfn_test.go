package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestConsoleStepFunctionsLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "sfn", operation, account, region, input)
		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}
	options := sfnClient(srv).Options()
	options.Region = region
	client := sfn.New(options)
	ctx := context.Background()
	pass := `{"StartAt":"Pass","States":{"Pass":{"Type":"Pass","End":true}}}`
	invalid := `{"StartAt":"Missing","States":{"Pass":{"Type":"Pass","End":true}}}`
	for _, tc := range []struct{ definition, result string }{{pass, "OK"}, {invalid, "FAIL"}} {
		got := call("ValidateStateMachineDefinition", map[string]any{"Definition": tc.definition, "Type": "STANDARD"})
		if got["Result"] != tc.result {
			t.Fatal(got)
		}
	}
	arn := call("CreateStateMachine", map[string]any{
		"Name": "console-workflow", "Definition": pass, "RoleArn": sfnRole, "Type": "STANDARD",
		"Tags":                 []any{map[string]any{"Key": "team", "Value": "dev"}},
		"LoggingConfiguration": map[string]any{"Level": "OFF", "IncludeExecutionData": false},
		"TracingConfiguration": map[string]any{"Enabled": true},
	})["StateMachineArn"].(string)
	if arn != "arn:aws:states:"+region+":"+account+":stateMachine:console-workflow" {
		t.Fatal(arn)
	}
	call("CreateStateMachine", map[string]any{"Name": "another-workflow", "Definition": pass, "RoleArn": sfnRole})
	list := call("ListStateMachines", map[string]any{"MaxResults": 1})
	if len(list["StateMachines"].([]any)) != 1 || list["NextToken"] == nil {
		t.Fatal(list)
	}
	next := call("ListStateMachines", map[string]any{"MaxResults": 1, "NextToken": list["NextToken"]})
	if len(next["StateMachines"].([]any)) != 1 {
		t.Fatal(next)
	}
	if len(call("ListExecutions", map[string]any{"StateMachineArn": arn})["Executions"].([]any)) != 0 {
		t.Fatal("creation started an execution")
	}
	call(
		"UpdateStateMachine",
		map[string]any{"StateMachineArn": arn, "Definition": pass, "RoleArn": "arn:aws:iam::000000000000:role/updated"},
	)
	described, err := client.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{StateMachineArn: new(arn)})
	if err != nil || aws.ToString(described.RoleArn) != "arn:aws:iam::000000000000:role/updated" ||
		described.TracingConfiguration == nil || !described.TracingConfiguration.Enabled {
		t.Fatalf("SDK machine = %+v, %v", described, err)
	}
	call(
		"TagResource",
		map[string]any{"ResourceArn": arn, "Tags": []any{map[string]any{"Key": "owner", "Value": "console"}}},
	)
	call("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})
	if len(call("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].([]any)) != 1 {
		t.Fatal("tags not updated")
	}
	// The bridge accepts all valid JSON values, not just objects.
	for _, tc := range []struct{ name, input string }{{"array", "[1,true,null]"}, {"scalar", "42"}, {"null", "null"}} {
		execution := call("StartExecution", map[string]any{
			"StateMachineArn": arn, "Name": tc.name, "Input": tc.input,
		})["ExecutionArn"].(string)
		done := finish(t, client, new(execution))
		if done.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(done.Output) != tc.input {
			t.Fatalf("execution = %+v", done)
		}
		got := call("DescribeExecution", map[string]any{"ExecutionArn": execution})
		if got["Status"] != "SUCCEEDED" || got["Input"] != tc.input || got["Output"] != tc.input {
			t.Fatal(got)
		}
	}
	executions := call(
		"ListExecutions",
		map[string]any{"StateMachineArn": arn, "StatusFilter": "SUCCEEDED", "MaxResults": 1},
	)
	if len(executions["Executions"].([]any)) != 1 || executions["NextToken"] == nil {
		t.Fatal(executions)
	}
	if len(
		call("ListExecutions", map[string]any{
			"StateMachineArn": arn, "MaxResults": 1, "StatusFilter": "SUCCEEDED", "NextToken": executions["NextToken"],
		})["Executions"].([]any),
	) != 1 {
		t.Fatal("missing execution page")
	}
	execution := executions["Executions"].([]any)[0].(map[string]any)["ExecutionArn"].(string)
	history := call(
		"GetExecutionHistory",
		map[string]any{"ExecutionArn": execution, "MaxResults": 1, "IncludeExecutionData": true},
	)
	first := history["Events"].([]any)[0].(map[string]any)
	if first["Type"] != "ExecutionStarted" || first["ExecutionStartedEventDetails"].(map[string]any)["Input"] == nil ||
		history["NextToken"] == nil {
		t.Fatal(history)
	}
	nextHistory := call(
		"GetExecutionHistory",
		map[string]any{"ExecutionArn": execution, "MaxResults": 1, "NextToken": history["NextToken"]},
	)
	if nextHistory["Events"].([]any)[0].(map[string]any)["Id"] == first["Id"] {
		t.Fatal("repeated history page")
	}
	reverse := call(
		"GetExecutionHistory",
		map[string]any{"ExecutionArn": execution, "MaxResults": 1, "ReverseOrder": true, "IncludeExecutionData": false},
	)
	last := reverse["Events"].([]any)[0].(map[string]any)
	if last["Type"] != "ExecutionSucceeded" ||
		last["ExecutionSucceededEventDetails"].(map[string]any)["Output"] != nil {
		t.Fatal(reverse)
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		for _, tc := range []struct {
			input     map[string]any
			operation string
		}{
			{operation: "DescribeStateMachine", input: map[string]any{"StateMachineArn": arn}},
			{operation: "UpdateStateMachine", input: map[string]any{"StateMachineArn": arn, "Definition": pass}},
			{operation: "StartExecution", input: map[string]any{"StateMachineArn": arn, "Input": "{}"}},
			{operation: "DeleteStateMachine", input: map[string]any{"StateMachineArn": arn}},
			{operation: "TagResource", input: map[string]any{
				"ResourceArn": arn, "Tags": []any{map[string]any{"Key": "bad", "Value": "scope"}},
			}},
			{operation: "DescribeExecution", input: map[string]any{"ExecutionArn": execution}},
			{operation: "GetExecutionHistory", input: map[string]any{"ExecutionArn": execution}},
			{operation: "StopExecution", input: map[string]any{"ExecutionArn": execution}},
		} {
			consoleResponse(
				t,
				srv,
				consoleLambdaRequest(t, srv, "sfn", tc.operation, scope.account, scope.region, tc.input),
				http.StatusBadRequest,
			)
		}
	}
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"sfn",
			"StartExecution",
			account,
			region,
			map[string]any{"StateMachineArn": arn, "Input": "{"},
		),
		http.StatusBadRequest,
	)
	fail := `{ "StartAt":"Failed", "States":{ "Failed":{
		"Type":"Fail", "Error":"ConsoleFailure", "Cause":"expected test failure"
	}}}`
	call("UpdateStateMachine", map[string]any{"StateMachineArn": arn, "Definition": fail})
	failed := call("StartExecution", map[string]any{"StateMachineArn": arn, "Name": "failed"})["ExecutionArn"].(string)
	finish(t, client, new(failed))
	got := call("DescribeExecution", map[string]any{"ExecutionArn": failed})
	if got["Status"] != "FAILED" || got["Error"] != "ConsoleFailure" || got["Cause"] != "expected test failure" {
		t.Fatal(got)
	}
	wait := `{"StartAt":"Wait","States":{"Wait":{"Type":"Wait","Seconds":120,"End":true}}}`
	call("UpdateStateMachine", map[string]any{"StateMachineArn": arn, "Definition": wait})
	waiting := call("StartExecution", map[string]any{"StateMachineArn": arn, "Name": "waiting"})["ExecutionArn"].(string)
	entered := false
	for range 100 {
		events := call("GetExecutionHistory", map[string]any{"ExecutionArn": waiting})["Events"].([]any)
		for _, e := range events {
			if e.(map[string]any)["Type"] == "WaitStateEntered" {
				entered = true
			}
		}
		if entered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !entered {
		t.Fatal("execution never entered wait")
	}
	call(
		"StopExecution",
		map[string]any{"ExecutionArn": waiting, "Error": "ConsoleStopped", "Cause": "stop from console"},
	)
	stopped := finish(t, client, new(waiting))
	if stopped.Status != sfntypes.ExecutionStatusAborted || aws.ToString(stopped.Cause) != "stop from console" {
		t.Fatal(stopped)
	}
	call("DeleteStateMachine", map[string]any{"StateMachineArn": arn})
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"sfn",
			"DescribeExecution",
			account,
			region,
			map[string]any{"ExecutionArn": waiting},
		),
		http.StatusBadRequest,
	)
}

func TestConsoleStepFunctionsExpressResult(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "us-east-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "sfn", operation, account, region, input)
		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}
	arn := call("CreateStateMachine", map[string]any{
		"Name": "console-express", "Definition": `{"StartAt":"Echo","States":{"Echo":{"Type":"Pass","End":true}}}`,
		"RoleArn": sfnRole, "Type": "EXPRESS",
	})["StateMachineArn"].(string)
	got := call("StartSyncExecution", map[string]any{"StateMachineArn": arn, "Name": "express", "Input": "[1,2]"})
	if got["Status"] != "SUCCEEDED" || got["Output"] != "[1,2]" || got["StartDate"] == nil || got["StopDate"] == nil {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		input     map[string]any
		operation string
	}{
		{operation: "ListExecutions", input: map[string]any{"StateMachineArn": arn}},
		{operation: "DescribeExecution", input: map[string]any{"ExecutionArn": got["ExecutionArn"]}},
		{operation: "GetExecutionHistory", input: map[string]any{"ExecutionArn": got["ExecutionArn"]}},
	} {
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(t, srv, "sfn", tc.operation, account, region, tc.input),
			http.StatusBadRequest,
		)
	}
	call(
		"UpdateStateMachine",
		map[string]any{
			"StateMachineArn": arn,
			"Definition": `{"StartAt":"Fail","States":{"Fail":{
				"Type":"Fail","Error":"ExpressFailed","Cause":"express failure"
			}}}`,
		},
	)
	failed := call("StartSyncExecution", map[string]any{"StateMachineArn": arn, "Name": "failed"})
	if failed["Status"] != "FAILED" || failed["Error"] != "ExpressFailed" || failed["Cause"] != "express failure" {
		t.Fatal(failed)
	}
	call("DeleteStateMachine", map[string]any{"StateMachineArn": arn})
}

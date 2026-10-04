package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestStepFunctionsRedrive(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sfnClient(s)
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "flaky", "app.fail")

	machine := createMachine(t, c, "redrive", `{
  "StartAt": "Start",
  "States": {
    "Start": {"Type": "Pass", "Assign": {"greeting": "hi"}, "Next": "Call"},
    "Call": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:000000000000:function:flaky",
      "Parameters": {"g.$": "$greeting"},
      "ResultSelector": {"g.$": "$.event.g"},
      "Next": "Done"
    },
    "Done": {"Type": "Pass", "Parameters": {"greeting.$": "$greeting", "echo.$": "$.g"}, "End": true}
  }
}`, sfntypes.StateMachineTypeStandard)

	exec := start(t, c, machine, `{}`)
	failed := finish(t, c, exec)
	if failed.Status != sfntypes.ExecutionStatusFailed ||
		failed.RedriveStatus != sfntypes.ExecutionRedriveStatusRedrivable {
		t.Fatalf("%+v", failed)
	}

	// The function is fixed, and the redrive picks up at Call with the
	// variables Start assigned.
	if _, err := functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("flaky"), Handler: aws.String("app.echo"),
	}); err != nil {
		t.Fatal(err)
	}

	redriven, err := c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: exec})
	if err != nil || redriven.RedriveDate == nil {
		t.Fatalf("%+v %v", redriven, err)
	}

	done := finish(t, c, exec)
	if done.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToString(done.Output) != `{"echo":"hi","greeting":"hi"}` || aws.ToInt32(done.RedriveCount) != 1 ||
		done.RedriveStatus != sfntypes.ExecutionRedriveStatusNotRedrivable {
		t.Fatalf("%+v %s", done, aws.ToString(done.Output))
	}

	history, err := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: exec})
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[sfntypes.HistoryEventType]int{}
	for _, e := range history.Events {
		kinds[e.Type]++
	}

	if kinds[sfntypes.HistoryEventTypeExecutionRedriven] != 1 ||
		kinds[sfntypes.HistoryEventTypePassStateEntered] != 2 ||
		kinds[sfntypes.HistoryEventTypeTaskStateEntered] != 2 {
		t.Fatalf("history %v", kinds)
	}

	_, err = c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: exec})
	code(t, err, "ExecutionNotRedrivable")
}

package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestStepFunctionsTestState(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sfnClient(s)
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "echo", "app.echo")
	createFunction(t, functions, "fail", "app.fail")

	cases := []struct {
		in     sfn.TestStateInput
		status sfntypes.TestExecutionStatus
		output string
		next   string
		err    string
	}{
		{
			in: sfn.TestStateInput{
				Definition: aws.String(`{"Type": "Pass", "Parameters": {"sum.$": "States.MathAdd($.a, $n)"}, ` +
					`"Next": "After"}`),
				Input: aws.String(`{"a": 1}`), Variables: aws.String(`{"n": 2}`),
			},
			status: sfntypes.TestExecutionStatusSucceeded, output: `{"sum":3}`, next: "After",
		},
		{
			in: sfn.TestStateInput{
				Definition: aws.String(`{"Type": "Choice", "Choices": [` +
					`{"Variable": "$.a", "NumericGreaterThan": 0, "Next": "Positive"}], "Default": "Other"}`),
				Input: aws.String(`{"a": 1}`),
			},
			status: sfntypes.TestExecutionStatusSucceeded, output: `{"a":1}`, next: "Positive",
		},
		{
			in: sfn.TestStateInput{
				Definition: aws.String(`{"Type": "Task", "Resource": ` +
					`"arn:aws:lambda:us-east-1:000000000000:function:echo", "ResultSelector": {"got.$": "$.event"}, ` +
					`"End": true}`),
				RoleArn: aws.String(sfnRole), Input: aws.String(`{"x": 1}`),
			},
			status: sfntypes.TestExecutionStatusSucceeded, output: `{"got":{"x":1}}`,
		},
		{
			in: sfn.TestStateInput{
				Definition: aws.String(`{"Type": "Task", "Resource": ` +
					`"arn:aws:lambda:us-east-1:000000000000:function:fail", ` +
					`"Retry": [{"ErrorEquals": ["States.ALL"]}], "End": true}`),
				RoleArn: aws.String(sfnRole),
			},
			status: sfntypes.TestExecutionStatusRetriable, err: "ValueError",
		},
		{
			in: sfn.TestStateInput{
				Definition: aws.String(`{"Type": "Task", "Resource": ` +
					`"arn:aws:lambda:us-east-1:000000000000:function:fail", ` +
					`"Catch": [{"ErrorEquals": ["States.ALL"], "ResultPath": "$.error", "Next": "Handle"}], "End": true}`),
				RoleArn: aws.String(sfnRole), Input: aws.String(`{"x": 1}`),
			},
			status: sfntypes.TestExecutionStatusCaughtError,
			next:   "Handle",
			err:    "ValueError",
			output: `{"error":{"Cause":"{\"errorType\":\"ValueError\",\"errorMessage\":\"boom\"}","Error":"ValueError"},"x":1}`,
		},
		{
			in:     sfn.TestStateInput{Definition: aws.String(`{"Type": "Fail", "Error": "Bad", "Cause": "why"}`)},
			status: sfntypes.TestExecutionStatusFailed, err: "Bad",
		},
		{
			// A state of a whole definition, by name.
			in: sfn.TestStateInput{
				Definition: aws.String(`{"StartAt": "A", "States": {"A": {"Type": "Pass", "Next": "B"}, ` +
					`"B": {"Type": "Pass", "Result": "b", "End": true}}}`),
				StateName: aws.String("B"),
			},
			status: sfntypes.TestExecutionStatusSucceeded, output: `"b"`,
		},
	}
	for i, tc := range cases {
		out, err := c.TestState(ctx, &tc.in, noHostPrefix)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}

		if out.Status != tc.status || (tc.output != "" && aws.ToString(out.Output) != tc.output) ||
			aws.ToString(out.NextState) != tc.next || aws.ToString(out.Error) != tc.err {
			t.Fatalf("case %d: %s output %s next %s error %s %s", i, out.Status, aws.ToString(out.Output),
				aws.ToString(out.NextState), aws.ToString(out.Error), aws.ToString(out.Cause))
		}
	}

	for _, in := range []*sfn.TestStateInput{
		{Definition: aws.String(`{"Type": "Pass", "End": true}`), InspectionLevel: sfntypes.InspectionLevelDebug},
		{Definition: aws.String(`{"Type": "Task", "Resource": "arn:aws:lambda:us-east-1:000000000000:function:echo", ` +
			`"End": true}`)},
		{
			Definition: aws.String(`{"StartAt": "A", "States": {"A": {"Type": "Pass", "End": true}}}`),
			StateName:  aws.String("Missing"),
		},
	} {
		_, err := c.TestState(ctx, in, noHostPrefix)
		code(t, err, "ValidationException")
	}

	_, err := c.TestState(
		ctx,
		&sfn.TestStateInput{Definition: aws.String(`{"Type": "Nope", "End": true}`)},
		noHostPrefix,
	)
	code(t, err, "InvalidDefinition")
}

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestStepFunctionsJSONata(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sfnClient(s)
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "echo", "app.echo")
	createFunction(t, functions, "fail", "app.fail")

	for name, definition := range map[string]string{
		"jsonpath-field": `{"QueryLanguage": "JSONata", "StartAt": "A",
			"States": {"A": {"Type": "Pass", "InputPath": "$.x", "End": true}}}`,
		"jsonata-field": `{"StartAt": "A", "States": {"A": {"Type": "Pass", "Output": {}, "End": true}}}`,
		"bad-expression": `{"QueryLanguage": "JSONata", "StartAt": "A",
			"States": {"A": {"Type": "Pass", "Output": "{% $states.input. %}", "End": true}}}`,
		"back-to-jsonpath": `{"QueryLanguage": "JSONata", "StartAt": "A",
			"States": {"A": {"Type": "Pass", "QueryLanguage": "JSONPath", "End": true}}}`,
		"unknown-language": `{"QueryLanguage": "XPath", "StartAt": "A", "States": {"A": {"Type": "Pass", "End": true}}}`,
		"variable-rule": `{"QueryLanguage": "JSONata", "StartAt": "A", "States": {"A": {"Type": "Choice",
			"Choices": [{"Variable": "$.x", "IsPresent": true, "Next": "B"}], "Default": "B"},
			"B": {"Type": "Succeed"}}}`,
	} {
		if _, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
			Name: aws.String(name), Definition: aws.String(definition), RoleArn: aws.String(sfnRole),
		}); err == nil || !strings.Contains(err.Error(), "InvalidDefinition") {
			t.Fatalf("%s: want InvalidDefinition, got %v", name, err)
		}
	}

	definition := `{
  "QueryLanguage": "JSONata",
  "StartAt": "Start",
  "States": {
    "Start": {
      "Type": "Pass",
      "Assign": {"total": "{% $sum($states.input.orders) %}", "who": "{% $states.input.customer %}"},
      "Output": {"count": "{% $count($states.input.orders) %}"},
      "Next": "Each"
    },
    "Each": {
      "Type": "Map",
      "Items": "{% [1, 2, 3] %}",
      "MaxConcurrency": "{% $states.input.count %}",
      "ItemSelector": {"v": "{% $states.context.Map.Item.Value * 10 %}", "who": "{% $who %}"},
      "ItemProcessor": {
        "StartAt": "Inc",
        "States": {"Inc": {"Type": "Pass", "Output": "{% $states.input.v + 1 %}", "End": true}}
      },
      "Assign": {"mapped": "{% $states.result %}"},
      "Next": "Pick"
    },
    "Pick": {
      "Type": "Choice",
      "Choices": [
        {"Condition": "{% $total > 5 %}", "Next": "Echo", "Output": {"big": true}, "Assign": {"size": "big"}}
      ],
      "Default": "Echo",
      "Output": {"big": false},
      "Assign": {"size": "small"}
    },
    "Echo": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:000000000000:function:echo",
      "Arguments": {"size": "{% $size %}", "big": "{% $states.input.big %}"},
      "TimeoutSeconds": "{% 30 %}",
      "Output": "{% $states.result.event %}",
      "Assign": {"echoed": "{% $states.result.event.size %}"},
      "Next": "Call"
    },
    "Call": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:000000000000:function:fail",
      "Catch": [{
        "ErrorEquals": ["States.ALL"],
        "Output": "{% $merge([$states.input, {'error': $states.errorOutput.Error}]) %}",
        "Assign": {"caught": true},
        "Next": "Pause"
      }],
      "End": true
    },
    "Pause": {"Type": "Wait", "Seconds": "{% 0 %}", "Next": "Done"},
    "Done": {
      "Type": "Pass",
      "Output": {
        "total": "{% $total %}", "mapped": "{% $mapped %}", "size": "{% $size %}", "echoed": "{% $echoed %}",
        "caught": "{% $caught %}", "big": "{% $states.input.big %}", "error": "{% $states.input.error %}",
        "partition": "{% $partition() %}", "parsed": "{% $parse('{\"a\": 1}').a %}",
        "range": "{% $range(1, 5, 2) %}", "exec": "{% $states.context.Execution.Name %}",
        "literal": [1, "two"]
      },
      "End": true
    }
  }
}`
	machine := createMachine(t, c, "jsonata", definition, sfntypes.StateMachineTypeStandard)
	for orders, size := range map[string]string{"[1, 2, 3]": "big", "[1]": "small"} {
		exec := start(t, c, machine, `{"customer": "ann", "orders": `+orders+`}`)
		out := finish(t, c, exec)
		if out.Status != sfntypes.ExecutionStatusSucceeded {
			t.Fatalf("%s: %s %s %s", orders, out.Status, aws.ToString(out.Error), aws.ToString(out.Cause))
		}

		var got map[string]any
		if err := json.Unmarshal([]byte(aws.ToString(out.Output)), &got); err != nil {
			t.Fatal(err)
		}

		want := map[string]any{
			"mapped": []any{11.0, 21.0, 31.0}, "size": size, "echoed": size, "caught": true, "big": size == "big",
			"error": "ValueError", "partition": "aws", "parsed": 1.0, "range": []any{1.0, 3.0, 5.0},
			"exec": execName(exec), "literal": []any{1.0, "two"},
		}
		for k, v := range want {
			if got, _ := json.Marshal(got[k]); !bytes.Equal(got, must(json.Marshal(v))) {
				t.Fatalf("%s: %s is %s, want %v (output %s)", orders, k, got, v, aws.ToString(out.Output))
			}
		}
	}

	// An expression with no value fails the state, and a JSONPath machine can
	// have JSONata states that read its variables.
	for input, want := range map[string]string{
		`{"x": 1}`: "States.QueryEvaluationError",
		`{"y": 2}`: "",
	} {
		mixed := createMachine(t, c, "mixed-"+strings.Trim(input, `{}": 0123456789`), `{
  "StartAt": "Set",
  "States": {
    "Set": {"Type": "Pass", "Assign": {"seen.$": "$"}, "Next": "Read"},
    "Read": {"Type": "Pass", "QueryLanguage": "JSONata", "Output": "{% $seen.y * 2 %}", "End": true}
  }
}`, sfntypes.StateMachineTypeStandard)
		out := finish(t, c, start(t, c, mixed, input))
		switch {
		case want == "" && (out.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(out.Output) != "4"):
			t.Fatalf("%s: %s %s %s", input, out.Status, aws.ToString(out.Output), aws.ToString(out.Cause))
		case want != "" && (out.Status != sfntypes.ExecutionStatusFailed || aws.ToString(out.Error) != want):
			t.Fatalf("%s: %s %s %s", input, out.Status, aws.ToString(out.Error), aws.ToString(out.Cause))
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

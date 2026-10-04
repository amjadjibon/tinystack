package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestStepFunctionsVariables(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sfnClient(s)
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "fail", "app.fail")

	for name, definition := range map[string]string{
		"bad-name":   `{"StartAt": "A", "States": {"A": {"Type": "Pass", "Assign": {"1x": 1}, "End": true}}}`,
		"reserved":   `{"StartAt": "A", "States": {"A": {"Type": "Pass", "Assign": {"states": 1}, "End": true}}}`,
		"succeed":    `{"StartAt": "A", "States": {"A": {"Type": "Succeed", "Assign": {"x": 1}}}}`,
		"not-object": `{"StartAt": "A", "States": {"A": {"Type": "Pass", "Assign": [1], "End": true}}}`,
	} {
		if _, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
			Name: aws.String(name), Definition: aws.String(definition), RoleArn: aws.String(sfnRole),
		}); err == nil || !strings.Contains(err.Error(), "InvalidDefinition") {
			t.Fatalf("%s: want InvalidDefinition, got %v", name, err)
		}
	}

	definition := `{
  "StartAt": "Start",
  "States": {
    "Start": {
      "Type": "Pass",
      "Assign": {"a": 1, "customer.$": "$.customer", "orders.$": "$.orders"},
      "Next": "Again"
    },
    "Again": {"Type": "Pass", "Assign": {"a": 2, "b.$": "$a"}, "Next": "Sum"},
    "Sum": {
      "Type": "Map",
      "ItemsPath": "$orders",
      "ItemSelector": {"amount.$": "$$.Map.Item.Value", "who.$": "$customer"},
      "ItemProcessor": {
        "StartAt": "Double",
        "States": {
          "Double": {
            "Type": "Pass",
            "Parameters": {"v.$": "States.MathAdd($.amount, $.amount)", "who.$": "$.who"},
            "Assign": {"inner": 1},
            "End": true
          }
        }
      },
      "Assign": {"results.$": "$"},
      "Next": "Who"
    },
    "Who": {
      "Type": "Choice",
      "Choices": [
        {"Variable": "$customer", "StringEquals": "ann", "Assign": {"greeting": "hi ann"}, "Next": "Call"}
      ],
      "Default": "Call",
      "Assign": {"greeting": "hi stranger"}
    },
    "Call": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:000000000000:function:fail",
      "Catch": [{"ErrorEquals": ["States.ALL"], "Assign": {"failed.$": "$.Error"}, "Next": "Done"}],
      "End": true
    },
    "Done": {
      "Type": "Pass",
      "Parameters": {
        "results.$": "$results", "greeting.$": "$greeting", "a.$": "$a", "b.$": "$b", "failed.$": "$failed",
        "first.$": "$results[0].v", "text.$": "States.Format('{} has {}', $customer, $a)"
      },
      "End": true
    }
  }
}`
	machine := createMachine(t, c, "variables", definition, sfntypes.StateMachineTypeStandard)
	for customer, greeting := range map[string]string{"ann": "hi ann", "bob": "hi stranger"} {
		exec := start(t, c, machine, `{"customer": "`+customer+`", "orders": [1, 2]}`)
		out := finish(t, c, exec)
		if out.Status != sfntypes.ExecutionStatusSucceeded {
			t.Fatalf("%s: %s %s %s", customer, out.Status, aws.ToString(out.Error), aws.ToString(out.Cause))
		}

		var got struct {
			Greeting string
			Failed   string
			Text     string
			Results  []map[string]any
			A, B     float64
			First    float64
		}
		if err := json.Unmarshal([]byte(aws.ToString(out.Output)), &got); err != nil {
			t.Fatal(err)
		}

		// Again reads $a before its own Assign sets it to 2.
		if got.A != 2 || got.B != 1 || got.Greeting != greeting || got.Failed == "" || got.First != 2 ||
			len(got.Results) != 2 || got.Results[1]["who"] != customer || got.Text != customer+" has 2" {
			t.Fatalf("%s: unexpected output %s", customer, aws.ToString(out.Output))
		}

		history, err := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: exec})
		if err != nil {
			t.Fatal(err)
		}

		var assigned map[string]string
		for _, e := range history.Events {
			if d := e.StateExitedEventDetails; d != nil && aws.ToString(d.Name) == "Start" {
				assigned = d.AssignedVariables
			}
		}

		if assigned["a"] != "1" || assigned["customer"] != `"`+customer+`"` {
			t.Fatalf("%s: Start assigned %v", customer, assigned)
		}
	}

	// A Map iteration's variables stay inside it.
	scoped := createMachine(t, c, "scoped", `{
  "StartAt": "Each",
  "States": {
    "Each": {
      "Type": "Map",
      "ItemProcessor": {
        "StartAt": "Set", "States": {"Set": {"Type": "Pass", "Assign": {"inner": 1}, "End": true}}
      },
      "Next": "Read"
    },
    "Read": {"Type": "Pass", "Parameters": {"inner.$": "$inner"}, "End": true}
  }
}`, sfntypes.StateMachineTypeStandard)
	out := finish(t, c, start(t, c, scoped, `[1]`))
	if out.Status != sfntypes.ExecutionStatusFailed || aws.ToString(out.Error) != "States.Runtime" ||
		!strings.Contains(aws.ToString(out.Cause), "inner") {
		t.Fatalf("scoped: %s %s %s", out.Status, aws.ToString(out.Error), aws.ToString(out.Cause))
	}
}

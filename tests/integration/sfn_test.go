package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const sfnRole = "arn:aws:iam::000000000000:role/states"

func sfnClient(s *httptest.Server) *sfn.Client {
	return sfn.New(sfn.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func createMachine(t *testing.T, c *sfn.Client, name, definition string, kind sfntypes.StateMachineType) *string {
	t.Helper()
	out, err := c.CreateStateMachine(context.Background(), &sfn.CreateStateMachineInput{
		Name: aws.String(name), Definition: aws.String(definition), RoleArn: aws.String(sfnRole), Type: kind,
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.StateMachineArn
}

// finish waits for an execution to stop running and describes it.
func finish(t *testing.T, c *sfn.Client, arn *string) *sfn.DescribeExecutionOutput {
	t.Helper()
	for range 200 {
		out, err := c.DescribeExecution(context.Background(), &sfn.DescribeExecutionInput{ExecutionArn: arn})
		if err != nil {
			t.Fatal(err)
		}

		if out.Status != sfntypes.ExecutionStatusRunning {
			return out
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("the execution is still running")
	return nil
}

func start(t *testing.T, c *sfn.Client, machine *string, input string) *string {
	t.Helper()
	out, err := c.StartExecution(context.Background(), &sfn.StartExecutionInput{
		StateMachineArn: machine, Input: aws.String(input),
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.ExecutionArn
}

func TestStepFunctionsStateMachines(t *testing.T) {
	s := server(t)
	c := sfnClient(s)
	ctx := context.Background()
	definition := `{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`

	for _, bad := range []string{
		`{"StartAt":"Missing","States":{"Done":{"Type":"Succeed"}}}`,
		`{"StartAt":"A","States":{"A":{"Type":"Pass"}}}`,
		`{"StartAt":"A","States":{"A":{"Type":"Task","Resource":"arn:aws:states:::glue:startJobRun","End":true}}}`,
		`not json`,
	} {
		_, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
			Name: aws.String("bad"), Definition: aws.String(bad), RoleArn: aws.String(sfnRole),
		})
		code(t, err, "InvalidDefinition")
	}

	validated, err := c.ValidateStateMachineDefinition(ctx, &sfn.ValidateStateMachineDefinitionInput{
		Definition: aws.String(`{"StartAt":"A","States":{"A":{"Type":"Pass","Next":"B"}}}`),
	})
	if err != nil || validated.Result != sfntypes.ValidateStateMachineDefinitionResultCodeFail ||
		aws.ToString(validated.Diagnostics[0].Code) != "MISSING_TRANSITION_TARGET" {
		t.Fatalf("%+v %v", validated, err)
	}

	arn := createMachine(t, c, "orders", definition, "")
	if aws.ToString(arn) != "arn:aws:states:us-east-1:000000000000:stateMachine:orders" {
		t.Fatal(aws.ToString(arn))
	}

	// Creating the same state machine again returns it; a different one conflicts.
	createMachine(t, c, "orders", definition, "")
	_, err = c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
		Name: aws.String("orders"), RoleArn: aws.String(sfnRole),
		Definition: aws.String(`{"StartAt":"X","States":{"X":{"Type":"Succeed"}}}`),
	})
	code(t, err, "StateMachineAlreadyExists")

	updated := `{"StartAt":"X","States":{"X":{"Type":"Pass","End":true}}}`
	if _, err = c.UpdateStateMachine(ctx, &sfn.UpdateStateMachineInput{
		StateMachineArn: arn, Definition: aws.String(updated),
	}); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{StateMachineArn: arn})
	if err != nil || aws.ToString(described.Definition) != updated ||
		described.Type != sfntypes.StateMachineTypeStandard {
		t.Fatalf("%+v %v", described, err)
	}

	if _, err = c.TagResource(ctx, &sfn.TagResourceInput{
		ResourceArn: arn, Tags: []sfntypes.Tag{{Key: aws.String("team"), Value: aws.String("a")}},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sfn.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "a" {
		t.Fatalf("%+v %v", tags, err)
	}

	listed, err := c.ListStateMachines(ctx, &sfn.ListStateMachinesInput{})
	if err != nil || len(listed.StateMachines) != 1 {
		t.Fatalf("%+v %v", listed, err)
	}

	if _, err = c.DeleteStateMachine(ctx, &sfn.DeleteStateMachineInput{StateMachineArn: arn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{StateMachineArn: arn})
	code(t, err, "StateMachineDoesNotExist")
}

func TestStepFunctionsExecutions(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sfnClient(s)
	functions := lambdaClient(s, "test")
	echo := createFunction(t, functions, "echo", "app.echo")
	createFunction(t, functions, "fail", "app.fail")
	tables := client(s, "test", "us-east-1")
	if _, err := tables.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("orders"),
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("id"), KeyType: ddbtypes.KeyTypeHash},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	}); err != nil {
		t.Fatal(err)
	}

	definition := `{
  "StartAt": "Prepare",
  "States": {
    "Prepare": {
      "Type": "Pass",
      "Parameters": {"id.$": "$.order", "greeting.$": "States.Format('order {} for {}', $.order, $.customer)",
        "items.$": "States.Array(1, 2, 3)", "execution.$": "$$.Execution.Name"},
      "ResultPath": "$.prepared",
      "Next": "Kind"
    },
    "Kind": {
      "Type": "Choice",
      "Choices": [
        {"And": [{"Variable": "$.total", "NumericGreaterThan": 100}, {"Variable": "$.vip", "IsPresent": true}],
         "Next": "Big"},
        {"Variable": "$.customer", "StringMatches": "a*", "Next": "Small"}
      ],
      "Default": "Unknown"
    },
    "Unknown": {"Type": "Fail", "Error": "Order.Unknown", "Cause": "no rule matched"},
    "Big": {"Type": "Pass", "Result": "big", "ResultPath": "$.size", "Next": "Save"},
    "Small": {"Type": "Pass", "Result": "small", "ResultPath": "$.size", "Next": "Save"},
    "Save": {
      "Type": "Task",
      "Resource": "arn:aws:states:::dynamodb:putItem",
      "Parameters": {"TableName": "orders", "Item": {"id": {"S.$": "$.prepared.id"}, "size": {"S.$": "$.size"}}},
      "ResultPath": null,
      "Next": "Load"
    },
    "Load": {
      "Type": "Task",
      "Resource": "arn:aws:states:::aws-sdk:dynamodb:getItem",
      "Parameters": {"TableName": "orders", "Key": {"id": {"S.$": "$.prepared.id"}}},
      "ResultSelector": {"size.$": "$.Item.size.S"},
      "ResultPath": "$.loaded",
      "Next": "Fan"
    },
    "Fan": {
      "Type": "Parallel",
      "ResultPath": "$.parallel",
      "Next": "Each",
      "Branches": [
        {"StartAt": "Echo", "States": {"Echo": {"Type": "Task", "Resource": "` + aws.ToString(echo.FunctionArn) + `",
          "Parameters": {"from": "direct"}, "ResultSelector": {"from.$": "$.event.from"}, "End": true}}},
        {"StartAt": "Invoke", "States": {"Invoke": {"Type": "Task", "Resource": "arn:aws:states:::lambda:invoke",
          "Parameters": {"FunctionName": "echo", "Payload": {"from": "invoke"}},
          "OutputPath": "$.Payload.event", "End": true}}}
      ]
    },
    "Each": {
      "Type": "Map",
      "ItemsPath": "$.prepared.items",
      "MaxConcurrency": 2,
      "ItemSelector": {"n.$": "$$.Map.Item.Value", "i.$": "$$.Map.Item.Index"},
      "ItemProcessor": {"StartAt": "Double", "States": {"Double": {"Type": "Pass",
        "Parameters": {"sum.$": "States.MathAdd($.n, $.n)"}, "End": true}}},
      "ResultPath": "$.doubled",
      "Next": "Flaky"
    },
    "Flaky": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:000000000000:function:fail",
      "Retry": [{"ErrorEquals": ["ValueError"], "IntervalSeconds": 0, "MaxAttempts": 2}],
      "Catch": [{"ErrorEquals": ["States.ALL"], "ResultPath": "$.caught", "Next": "Wait"}],
      "End": true
    },
    "Wait": {"Type": "Wait", "Seconds": 0, "Next": "Done"},
    "Done": {"Type": "Succeed", "OutputPath": "$"}
  }
}`
	machine := createMachine(t, c, "orders", definition, "")
	exec := start(t, c, machine, `{"order": "o-1", "customer": "ana", "total": 5}`)
	done := finish(t, c, exec)
	if done.Status != sfntypes.ExecutionStatusSucceeded {
		t.Fatalf("%+v %s %s", done.Status, aws.ToString(done.Error), aws.ToString(done.Cause))
	}

	var out struct {
		Prepared struct {
			Greeting  string
			Execution string
		}
		Caught   struct{ Error, Cause string }
		Size     string
		Loaded   struct{ Size string }
		Parallel []map[string]any
		Doubled  []struct{ Sum float64 }
	}
	if err := json.Unmarshal([]byte(aws.ToString(done.Output)), &out); err != nil {
		t.Fatal(err)
	}

	if out.Prepared.Greeting != "order o-1 for ana" || out.Size != "small" || out.Loaded.Size != "small" ||
		out.Parallel[0]["from"] != "direct" || out.Parallel[1]["from"] != "invoke" || len(out.Doubled) != 3 ||
		out.Doubled[2].Sum != 6 || out.Caught.Error != "ValueError" || !strings.Contains(out.Caught.Cause, "boom") ||
		out.Prepared.Execution == "" {
		t.Fatalf("%s", aws.ToString(done.Output))
	}

	history, err := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: exec})
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[sfntypes.HistoryEventType]int{}
	for _, e := range history.Events {
		kinds[e.Type]++
	}

	if kinds[sfntypes.HistoryEventTypeExecutionStarted] != 1 ||
		kinds[sfntypes.HistoryEventTypeExecutionSucceeded] != 1 ||
		kinds[sfntypes.HistoryEventTypeLambdaFunctionFailed] != 3 ||
		kinds[sfntypes.HistoryEventTypeMapIterationSucceeded] != 3 ||
		kinds[sfntypes.HistoryEventTypeTaskSucceeded] < 3 ||
		kinds[sfntypes.HistoryEventTypeChoiceStateEntered] != 1 {
		t.Fatalf("%v", kinds)
	}

	// Fail states fail the execution.
	failed := finish(t, c, start(t, c, machine, `{"order": "o-2", "customer": "zed", "total": 5}`))
	if failed.Status != sfntypes.ExecutionStatusFailed || aws.ToString(failed.Error) != "Order.Unknown" {
		t.Fatalf("%+v", failed)
	}

	// A name is unique, but starting a running execution again returns it.
	_, err = c.StartExecution(ctx, &sfn.StartExecutionInput{StateMachineArn: machine, Name: aws.String(execName(exec))})
	code(t, err, "ExecutionAlreadyExists")

	listed, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{
		StateMachineArn: machine, StatusFilter: sfntypes.ExecutionStatusFailed,
	})
	if err != nil || len(listed.Executions) != 1 {
		t.Fatalf("%+v %v", listed, err)
	}
}

// noHostPrefix keeps the SDK from adding an operation's host prefix.
func noHostPrefix(o *sfn.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("NoHostPrefix",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
				middleware.InitializeOutput, middleware.Metadata, error,
			) {
				return next.HandleInitialize(smithyhttp.DisableEndpointHostPrefix(ctx, true), in)
			}), middleware.After)
	})
}

func execName(arn *string) string {
	parts := strings.Split(aws.ToString(arn), ":")
	return parts[len(parts)-1]
}

func TestStepFunctionsCallbacksAndControl(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sfnClient(s)
	queues := sqsClient(s, "test")
	queue, err := queues.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("approvals")})
	if err != nil {
		t.Fatal(err)
	}

	// A token callback through SQS.
	callback := createMachine(t, c, "approve", `{"StartAt":"Ask","States":{"Ask":{
  "Type":"Task","Resource":"arn:aws:states:::sqs:sendMessage.waitForTaskToken","HeartbeatSeconds":30,
  "Parameters":{"QueueUrl":"`+aws.ToString(queue.QueueUrl)+`","MessageBody":{"token.$":"$$.Task.Token","id.$":"$.id"}},
  "End":true}}}`, "")
	exec := start(t, c, callback, `{"id":"r-1"}`)
	var token string
	for range 100 {
		got, receiveErr := queues.ReceiveMessage(
			ctx,
			&sqs.ReceiveMessageInput{QueueUrl: queue.QueueUrl, WaitTimeSeconds: 1},
		)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		if len(got.Messages) > 0 {
			var body map[string]string
			if err = json.Unmarshal(
				[]byte(aws.ToString(got.Messages[0].Body)),
				&body,
			); err != nil ||
				body["id"] != "r-1" {
				t.Fatalf("%s %v", aws.ToString(got.Messages[0].Body), err)
			}

			token = body["token"]
			break
		}
	}

	if _, err = c.SendTaskHeartbeat(ctx, &sfn.SendTaskHeartbeatInput{TaskToken: aws.String(token)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.SendTaskSuccess(ctx, &sfn.SendTaskSuccessInput{
		TaskToken: aws.String(token), Output: aws.String(`{"approved":true}`),
	}); err != nil {
		t.Fatal(err)
	}

	if done := finish(t, c, exec); aws.ToString(done.Output) != `{"approved":true}` {
		t.Fatalf("%+v", done)
	}

	_, err = c.SendTaskSuccess(ctx, &sfn.SendTaskSuccessInput{TaskToken: aws.String(token), Output: aws.String(`{}`)})
	code(t, err, "TaskDoesNotExist")

	// Activities hand tasks to workers.
	act, err := c.CreateActivity(ctx, &sfn.CreateActivityInput{Name: aws.String("review")})
	if err != nil {
		t.Fatal(err)
	}

	worker := createMachine(t, c, "review", `{"StartAt":"Review","States":{"Review":{
  "Type":"Task","Resource":"`+aws.ToString(act.ActivityArn)+`","End":true}}}`, "")
	exec = start(t, c, worker, `{"doc":"d-1"}`)
	task, err := c.GetActivityTask(
		ctx,
		&sfn.GetActivityTaskInput{ActivityArn: act.ActivityArn, WorkerName: aws.String("w1")},
	)
	if err != nil || aws.ToString(task.Input) != `{"doc":"d-1"}` {
		t.Fatalf("%+v %v", task, err)
	}

	if _, err = c.SendTaskFailure(ctx, &sfn.SendTaskFailureInput{
		TaskToken: task.TaskToken, Error: aws.String("Review.Rejected"), Cause: aws.String("typos"),
	}); err != nil {
		t.Fatal(err)
	}

	if done := finish(t, c, exec); aws.ToString(done.Error) != "Review.Rejected" {
		t.Fatalf("%+v", done)
	}

	// StopExecution aborts, and TimeoutSeconds times out.
	slow := createMachine(t, c, "slow", `{"StartAt":"Nap","TimeoutSeconds":1,
  "States":{"Nap":{"Type":"Wait","SecondsPath":"$.seconds","End":true}}}`, "")
	exec = start(t, c, slow, `{"seconds":30}`)
	if _, err = c.StopExecution(ctx, &sfn.StopExecutionInput{
		ExecutionArn: exec, Error: aws.String("Manual"), Cause: aws.String("impatient"),
	}); err != nil {
		t.Fatal(err)
	}

	if done := finish(
		t,
		c,
		exec,
	); done.Status != sfntypes.ExecutionStatusAborted ||
		aws.ToString(done.Error) != "Manual" {
		t.Fatalf("%+v", done)
	}

	if done := finish(t, c, start(t, c, slow, `{"seconds":30}`)); done.Status != sfntypes.ExecutionStatusTimedOut {
		t.Fatalf("%+v", done)
	}

	// Express state machines run synchronously, and a parent waits for a
	// child with .sync.
	express := createMachine(t, c, "shout", `{"StartAt":"Up","States":{"Up":{"Type":"Pass",
  "Parameters":{"text.$":"States.Format('{}!', $.text)"},"End":true}}}`, sfntypes.StateMachineTypeExpress)
	// The SDK sends StartSyncExecution to a "sync-" host prefix, which a
	// local endpoint does not have.
	synced, err := c.StartSyncExecution(
		ctx,
		&sfn.StartSyncExecutionInput{StateMachineArn: express, Input: aws.String(`{"text":"hi"}`)},
		noHostPrefix,
	)
	if err != nil || synced.Status != sfntypes.SyncExecutionStatusSucceeded ||
		aws.ToString(synced.Output) != `{"text":"hi!"}` {
		t.Fatalf("%+v %v", synced, err)
	}

	child := createMachine(t, c, "child", `{"StartAt":"Up","States":{"Up":{"Type":"Pass",
  "Parameters":{"text.$":"States.Format('{}!', $.text)"},"End":true}}}`, "")
	parent := createMachine(t, c, "parent", `{"StartAt":"Run","States":{"Run":{"Type":"Task",
  "Resource":"arn:aws:states:::states:startExecution.sync:2",
  "Parameters":{"StateMachineArn":"`+aws.ToString(child)+`","Input":{"text":"yo"}},
  "OutputPath":"$.Output","End":true}}}`, "")
	if done := finish(t, c, start(t, c, parent, `{}`)); aws.ToString(done.Output) != `{"text":"yo!"}` {
		t.Fatalf("%+v %s", done, aws.ToString(done.Cause))
	}
}

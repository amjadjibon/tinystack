package integration_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func TestCloudWatchStepFunctionsMetrics(t *testing.T) {
	ctx := t.Context()
	s := server(t)
	c, cw := sfnClient(s), cloudwatchClient(s)
	const namespace = "AWS/States"
	express := createMachine(t, c, "metrics-ok", `{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`,
		sfntypes.StateMachineTypeExpress)
	ok, err := c.StartSyncExecution(ctx, &sfn.StartSyncExecutionInput{StateMachineArn: express}, noHostPrefix)
	if err != nil || ok.Status != sfntypes.SyncExecutionStatusSucceeded {
		t.Fatalf("successful execution: %+v %v", ok, err)
	}

	standard := createMachine(t, c, "metrics-failed", `{"StartAt":"Fail","States":{"Fail":{"Type":"Fail"}}}`,
		sfntypes.StateMachineTypeStandard)
	failed, err := c.StartExecution(ctx, &sfn.StartExecutionInput{StateMachineArn: standard})
	if err != nil {
		t.Fatal(err)
	}

	if e := finish(t, c, failed.ExecutionArn); e.Status != sfntypes.ExecutionStatusFailed {
		t.Fatalf("failed execution: %+v", e)
	}

	okDims := dimensions("StateMachineArn", *express)
	badDims := dimensions("StateMachineArn", *standard)
	expectBuiltin(t, cw, namespace, "ExecutionsSucceeded", "Count", okDims, 1)
	expectBuiltin(t, cw, namespace, "ExecutionsFailed", "Count", badDims, 1)
	started := builtinMetric(t, cw, namespace, "ExecutionsStarted", nil, func(b builtin) bool { return b.count == 4 })
	if started.sum != 2 {
		t.Fatalf("start metrics: %+v", started)
	}

	builtinMetric(t, cw, namespace, "ExecutionTime", badDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})
	checkBuiltinP99(t, cw, namespace, "ExecutionTime", okDims)
}

func TestCloudWatchEventBridgeMetrics(t *testing.T) {
	ctx := t.Context()
	s := server(t)
	c, cw := eventsClient(s, "test"), cloudwatchClient(s)
	putRule(t, c, "metrics-orders", orderPattern)
	_, queue := queueARN(t, sqsClient(s, "test"), "metrics-queue")
	targets, err := c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("metrics-orders"), Targets: []ebtypes.Target{
			{Id: new("ok"), Arn: &queue},
			{Id: new("missing"), Arn: new(queue + "-missing")},
		},
	})
	if err != nil || targets.FailedEntryCount != 0 {
		t.Fatalf("put targets: %+v %v", targets, err)
	}

	out, err := c.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebtypes.PutEventsRequestEntry{
			{Source: new("shop"), DetailType: new("order"), Detail: new("{}")},
			{Source: new("shop"), DetailType: new("nonmatch"), Detail: new("{}")},
		},
	})
	if err != nil || out.FailedEntryCount != 0 {
		t.Fatalf("put events: %+v %v", out, err)
	}

	const namespace = "AWS/Events"
	rule := dimensions("RuleName", "metrics-orders")
	expectBuiltin(t, cw, namespace, "MatchedEvents", "Count", rule, 1)
	expectBuiltin(t, cw, namespace, "MatchedEvents", "Count", dimensions("EventBusName", "default"), 1)
	expectBuiltin(t, cw, namespace, "Invocations", "Count", rule, 2)
	expectBuiltin(t, cw, namespace, "FailedInvocations", "Count", rule, 1)
}

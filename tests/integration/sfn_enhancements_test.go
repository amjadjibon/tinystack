package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func gzipBytes(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestStepFunctionsJSONPathFiltersDescentAndInlineMaxConcurrencyPath(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)

	// Non-definite paths must be rejected on reference paths (ResultPath, MaxConcurrencyPath, Distributed ItemsPath).
	for _, bad := range []string{
		`{"StartAt":"P","States":{"P":{"Type":"Pass","ResultPath":"$..a","End":true}}}`,
		`{"StartAt":"P","States":{"P":{"Type":"Pass","ResultPath":"$.items[?(@.x == 1)]","End":true}}}`,
		`{"StartAt":"M","States":{"M":{"Type":"Map","MaxConcurrencyPath":"$.items[?(@.x == 1)]",` +
			`"ItemProcessor":{"StartAt":"P","States":{"P":{"Type":"Pass","End":true}}},"End":true}}}`,
		`{"StartAt":"M","States":{"M":{"Type":"Map","MaxConcurrency":1,"MaxConcurrencyPath":"$.limit",` +
			`"ItemProcessor":{"StartAt":"P","States":{"P":{"Type":"Pass","End":true}}},"End":true}}}`,
		distributedDefinition(`"ItemsPath":"$.items[?(@.x == 1)]",`, "STANDARD", passMapProcessor),
	} {
		_, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
			Name: new("bad-jsonpath"), RoleArn: new(sfnRole), Definition: new(bad),
		})
		code(t, err, "InvalidDefinition")
	}

	// Query paths: recursive descent, filters with comparisons, boolean ops, parens, existence checks,
	// null/bool/string/num literals, and Inline Map MaxConcurrencyPath.
	def := `{
  "StartAt": "Query",
  "States": {
    "Query": {
      "Type": "Pass",
      "Parameters": {
        "allPrices.$": "$..price",
        "bracketNames.$": "$..['name']",
        "allValues.$": "$.nested..*",
        "cheapOrFiction.$": "$.books[?((@.price < 10 || @.category == 'fiction') && !(@.sold == true))]",
        "withIsbn.$": "$.books[?(@.isbn)]",
        "withoutIsbn.$": "$.books[?(!@.isbn)]",
        "nullAuthor.$": "$.books[?(@.author == null)]",
        "underBudget.$": "$.books[?(@.price <= $.budget && @.price >= 5)]",
        "descFiltered.$": "$..[?(@.price != 25 && @.price > 10)]",
        "scalarFilter.$": "$.nums[?(@ > 2 && @ < 5)]"
      },
      "ResultPath": "$.queried",
      "Next": "InlineMap"
    },
    "InlineMap": {
      "Type": "Map",
      "ItemsPath": "$.books[?(@.price < 15)]",
      "MaxConcurrencyPath": "$.concurrency",
      "ItemProcessor": {
        "StartAt": "Pick",
        "States": {
          "Pick": {"Type": "Pass", "OutputPath": "$.name", "End": true}
        }
      },
      "ResultPath": "$.selectedNames",
      "End": true
    }
  }
}`
	machine := createMachine(t, c, "jsonpath-advanced", def, sfntypes.StateMachineTypeStandard)
	input := `{
  "budget": 12,
  "concurrency": 2,
  "nums": [1, 2, 3, 4, 5],
  "nested": {"a": 1, "b": {"c": 2}},
  "books": [
    {"name": "A", "price": 8, "category": "tech", "isbn": "111", "sold": false, "author": "alice"},
    {"name": "B", "price": 12, "category": "fiction", "sold": false, "author": null},
    {"name": "C", "price": 25, "category": "fiction", "isbn": false, "sold": true, "author": "bob"}
  ]
}`
	done := finish(t, c, start(t, c, machine, input))
	if done.Status != sfntypes.ExecutionStatusSucceeded {
		t.Fatalf("status=%s err=%s cause=%s", done.Status, aws.ToString(done.Error), aws.ToString(done.Cause))
	}

	var out struct {
		Queried struct {
			AllPrices      []float64        `json:"allPrices"`
			BracketNames   []string         `json:"bracketNames"`
			AllValues      []any            `json:"allValues"`
			CheapOrFiction []map[string]any `json:"cheapOrFiction"`
			WithIsbn       []map[string]any `json:"withIsbn"`
			WithoutIsbn    []map[string]any `json:"withoutIsbn"`
			NullAuthor     []map[string]any `json:"nullAuthor"`
			UnderBudget    []map[string]any `json:"underBudget"`
			DescFiltered   []map[string]any `json:"descFiltered"`
			ScalarFilter   []float64        `json:"scalarFilter"`
		} `json:"queried"`
		SelectedNames []string `json:"selectedNames"`
	}
	if err := json.Unmarshal([]byte(aws.ToString(done.Output)), &out); err != nil {
		t.Fatal(err)
	}

	if len(out.Queried.AllPrices) != 3 || out.Queried.AllPrices[0] != 8 || out.Queried.AllPrices[2] != 25 {
		t.Fatalf("allPrices = %v", out.Queried.AllPrices)
	}

	if len(out.Queried.BracketNames) != 3 || out.Queried.BracketNames[1] != "B" {
		t.Fatalf("bracketNames = %v", out.Queried.BracketNames)
	}

	if len(out.Queried.AllValues) != 3 {
		t.Fatalf("allValues = %v", out.Queried.AllValues)
	}

	// Book A (price 8, sold false) and Book B (fiction, sold false). Book C has sold: true.
	if len(out.Queried.CheapOrFiction) != 2 {
		t.Fatalf("cheapOrFiction = %v", out.Queried.CheapOrFiction)
	}

	// Existence check: Book A ("111") and Book C (false) both have isbn key; Book B does not.
	if len(out.Queried.WithIsbn) != 2 || len(out.Queried.WithoutIsbn) != 1 {
		t.Fatalf("isbn checks: with=%v without=%v", out.Queried.WithIsbn, out.Queried.WithoutIsbn)
	}

	if len(out.Queried.NullAuthor) != 1 || out.Queried.NullAuthor[0]["name"] != "B" {
		t.Fatalf("nullAuthor = %v", out.Queried.NullAuthor)
	}

	if len(out.Queried.UnderBudget) != 2 {
		t.Fatalf("underBudget = %v", out.Queried.UnderBudget)
	}

	if len(out.Queried.DescFiltered) != 1 || out.Queried.DescFiltered[0]["name"] != "B" {
		t.Fatalf("descFiltered = %v", out.Queried.DescFiltered)
	}

	if len(out.Queried.ScalarFilter) != 2 || out.Queried.ScalarFilter[0] != 3 || out.Queried.ScalarFilter[1] != 4 {
		t.Fatalf("scalarFilter = %v", out.Queried.ScalarFilter)
	}

	if len(out.SelectedNames) != 2 || out.SelectedNames[0] != "A" || out.SelectedNames[1] != "B" {
		t.Fatalf("selectedNames = %v", out.SelectedNames)
	}

	// Invalid runtime MaxConcurrencyPath value fails with States.Runtime.
	badExec := finish(t, c, start(t, c, machine, `{
  "budget": 12, "concurrency": -1, "nums": [], "nested": {}, "books": []
}`))
	if badExec.Status != sfntypes.ExecutionStatusFailed || aws.ToString(badExec.Error) != "States.Runtime" {
		t.Fatalf("expected States.Runtime for negative MaxConcurrencyPath: %+v", badExec)
	}
}

func TestStepFunctionsAPIParamsAndKMSEncryption(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)
	kmsSvc := kmsClient(srv, "test", "us-east-1")
	functions := lambdaClient(srv, "test")
	createFunction(t, functions, "sfn-param-fn", "app.fail")

	keyOut, err := kmsSvc.CreateKey(ctx, &kms.CreateKeyInput{})
	if err != nil {
		t.Fatal(err)
	}

	keyARN := aws.ToString(keyOut.KeyMetadata.Arn)

	// Invalid encryption configs are rejected explicitly.
	for _, enc := range []*sfntypes.EncryptionConfiguration{
		{Type: sfntypes.EncryptionTypeCustomerManagedKmsKey},
		{
			Type:                         sfntypes.EncryptionTypeCustomerManagedKmsKey,
			KmsKeyId:                     new(keyARN),
			KmsDataKeyReusePeriodSeconds: new(int32(30)),
		},
		{
			Type:                         sfntypes.EncryptionTypeCustomerManagedKmsKey,
			KmsKeyId:                     new(keyARN),
			KmsDataKeyReusePeriodSeconds: new(int32(901)),
		},
		{
			Type:     sfntypes.EncryptionTypeAwsOwnedKey,
			KmsKeyId: new(keyARN),
		},
	} {
		_, err = c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
			Name:                    new("bad-enc"),
			Definition:              new(`{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`),
			RoleArn:                 new(sfnRole),
			EncryptionConfiguration: enc,
		})
		if err == nil {
			t.Fatalf("accepted invalid encryption config: %+v", enc)
		}
	}

	// Nonexistent KMS key fails with KmsAccessDeniedException.
	_, err = c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
		Name:       new("missing-kms"),
		Definition: new(`{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`),
		RoleArn:    new(sfnRole),
		EncryptionConfiguration: &sfntypes.EncryptionConfiguration{
			Type:     sfntypes.EncryptionTypeCustomerManagedKmsKey,
			KmsKeyId: new("arn:aws:kms:us-east-1:000000000000:key/does-not-exist"),
		},
	})
	code(t, err, "KmsAccessDeniedException")

	// Create encrypted Standard state machine + encrypted Activity.
	smDef := `{"StartAt":"Call","States":{"Call":{"Type":"Task",` +
		`"Resource":"arn:aws:lambda:us-east-1:000000000000:function:sfn-param-fn","End":true}}}`
	created, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
		Name:       new("encrypted-sm"),
		Definition: new(smDef),
		RoleArn:    new(sfnRole),
		EncryptionConfiguration: &sfntypes.EncryptionConfiguration{
			Type:                         sfntypes.EncryptionTypeCustomerManagedKmsKey,
			KmsKeyId:                     new(keyARN),
			KmsDataKeyReusePeriodSeconds: new(int32(120)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	descSM, err := c.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{
		StateMachineArn: created.StateMachineArn,
	})
	if err != nil || descSM.EncryptionConfiguration == nil ||
		descSM.EncryptionConfiguration.Type != sfntypes.EncryptionTypeCustomerManagedKmsKey ||
		aws.ToString(descSM.EncryptionConfiguration.KmsKeyId) != keyARN ||
		aws.ToInt32(descSM.EncryptionConfiguration.KmsDataKeyReusePeriodSeconds) != 120 ||
		aws.ToString(descSM.Definition) == "" {
		t.Fatalf("DescribeStateMachine: %+v %v", descSM, err)
	}

	// DescribeStateMachine with METADATA_ONLY omits Definition.
	metaSM, err := c.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{
		StateMachineArn: created.StateMachineArn,
		IncludedData:    sfntypes.IncludedDataMetadataOnly,
	})
	if err != nil || metaSM.Definition != nil || metaSM.EncryptionConfiguration == nil {
		t.Fatalf("DescribeStateMachine METADATA_ONLY: %+v %v", metaSM, err)
	}

	// CreateActivity with KMS key and verify DescribeActivity returns EncryptionConfiguration.
	act, err := c.CreateActivity(ctx, &sfn.CreateActivityInput{
		Name: new("encrypted-act"),
		EncryptionConfiguration: &sfntypes.EncryptionConfiguration{
			Type:     sfntypes.EncryptionTypeCustomerManagedKmsKey,
			KmsKeyId: new(keyARN),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	descAct, err := c.DescribeActivity(ctx, &sfn.DescribeActivityInput{ActivityArn: act.ActivityArn})
	if err != nil || descAct.EncryptionConfiguration == nil ||
		descAct.EncryptionConfiguration.Type != sfntypes.EncryptionTypeCustomerManagedKmsKey ||
		aws.ToInt32(descAct.EncryptionConfiguration.KmsDataKeyReusePeriodSeconds) != 300 {
		t.Fatalf("DescribeActivity: %+v %v", descAct, err)
	}

	// StartExecution with TraceHeader; fail, fix, redrive; verify traceHeader, includedData, redriveFilter.
	trace := "Root=1-5f84c7a1-abcdef1234567890"
	started1, err := c.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: created.StateMachineArn,
		Name:            new("exec-redriven"),
		Input:           new(`{"n":1}`),
		TraceHeader:     new(trace),
	})
	if err != nil {
		t.Fatal(err)
	}

	finish(t, c, started1.ExecutionArn)
	if _, err = functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("sfn-param-fn"), Handler: new("app.echo"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: started1.ExecutionArn}); err != nil {
		t.Fatal(err)
	}

	exec1Done := finish(t, c, started1.ExecutionArn)
	if exec1Done.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(exec1Done.TraceHeader) != trace {
		t.Fatalf("exec1Done: %+v", exec1Done)
	}

	// DescribeExecution with METADATA_ONLY omits Input/Output and keeps TraceHeader.
	metaExec, err := c.DescribeExecution(ctx, &sfn.DescribeExecutionInput{
		ExecutionArn: started1.ExecutionArn,
		IncludedData: sfntypes.IncludedDataMetadataOnly,
	})
	if err != nil || metaExec.Input != nil || metaExec.Output != nil ||
		metaExec.InputDetails != nil || metaExec.OutputDetails != nil ||
		aws.ToString(metaExec.TraceHeader) != trace {
		t.Fatalf("DescribeExecution METADATA_ONLY: %+v %v", metaExec, err)
	}

	// DescribeStateMachineForExecution with METADATA_ONLY omits Definition and returns EncryptionConfiguration.
	smForExec, err := c.DescribeStateMachineForExecution(ctx, &sfn.DescribeStateMachineForExecutionInput{
		ExecutionArn: started1.ExecutionArn,
		IncludedData: sfntypes.IncludedDataMetadataOnly,
	})
	if err != nil || smForExec.Definition != nil || smForExec.EncryptionConfiguration == nil ||
		smForExec.EncryptionConfiguration.Type != sfntypes.EncryptionTypeCustomerManagedKmsKey {
		t.Fatalf("DescribeStateMachineForExecution METADATA_ONLY: %+v %v", smForExec, err)
	}

	// Start a second execution that is NOT redriven, then verify ListExecutions.RedriveFilter.
	started2, err := c.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: created.StateMachineArn, Name: new("exec-fresh"), Input: new(`{"n":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	finish(t, c, started2.ExecutionArn)

	redrivenList, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{
		StateMachineArn: created.StateMachineArn, RedriveFilter: sfntypes.ExecutionRedriveFilterRedriven,
	})
	if err != nil || len(redrivenList.Executions) != 1 ||
		aws.ToString(redrivenList.Executions[0].Name) != "exec-redriven" {
		t.Fatalf("REDRIVEN list: %+v %v", redrivenList, err)
	}

	notRedrivenList, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{
		StateMachineArn: created.StateMachineArn, RedriveFilter: sfntypes.ExecutionRedriveFilterNotRedriven,
	})
	if err != nil || len(notRedrivenList.Executions) != 1 ||
		aws.ToString(notRedrivenList.Executions[0].Name) != "exec-fresh" {
		t.Fatalf("NOT_REDRIVEN list: %+v %v", notRedrivenList, err)
	}

	// Express state machine: StartSyncExecution traceHeader + includedData METADATA_ONLY.
	express := createMachine(t, c, "sync-params", `{"StartAt":"P","States":{"P":{"Type":"Pass","End":true}}}`,
		sfntypes.StateMachineTypeExpress)
	syncFull, err := c.StartSyncExecution(ctx, &sfn.StartSyncExecutionInput{
		StateMachineArn: express, Input: new(`{"k":"v"}`), TraceHeader: new(trace),
	}, noHostPrefix)
	if err != nil || aws.ToString(syncFull.TraceHeader) != trace || aws.ToString(syncFull.Output) != `{"k":"v"}` {
		t.Fatalf("syncFull: %+v %v", syncFull, err)
	}

	syncMeta, err := c.StartSyncExecution(ctx, &sfn.StartSyncExecutionInput{
		StateMachineArn: express,
		Input:           new(`{"k":"v"}`),
		TraceHeader:     new(trace),
		IncludedData:    sfntypes.IncludedDataMetadataOnly,
	}, noHostPrefix)
	if err != nil || syncMeta.Input != nil || syncMeta.Output != nil ||
		syncMeta.InputDetails != nil || syncMeta.OutputDetails != nil ||
		aws.ToString(syncMeta.TraceHeader) != trace {
		t.Fatalf("syncMeta: %+v %v", syncMeta, err)
	}

	// Disabling the KMS key causes StartExecution to fail with KmsInvalidStateException.
	if _, err = kmsSvc.DisableKey(ctx, &kms.DisableKeyInput{KeyId: new(keyARN)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: created.StateMachineArn, Name: new("blocked-by-kms"),
	})
	code(t, err, "KmsInvalidStateException")

	// UpdateStateMachine with ONLY EncryptionConfiguration back to AWS_OWNED_KEY succeeds and unblocks StartExecution.
	if _, err = c.UpdateStateMachine(ctx, &sfn.UpdateStateMachineInput{
		StateMachineArn: created.StateMachineArn,
		EncryptionConfiguration: &sfntypes.EncryptionConfiguration{
			Type: sfntypes.EncryptionTypeAwsOwnedKey,
		},
	}); err != nil {
		t.Fatal(err)
	}

	unblocked, err := c.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: created.StateMachineArn, Name: new("unblocked"),
	})
	if err != nil {
		t.Fatal(err)
	}

	finish(t, c, unblocked.ExecutionArn)
}

func TestStepFunctionsTestStateMockContextAndInspection(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)

	// Task with Mock.Result (no RoleArn needed), Context override, TRACE inspection, RevealSecrets.
	taskDef := `{
  "Type": "Task",
  "Resource": "arn:aws:lambda:us-east-1:000000000000:function:never-called",
  "InputPath": "$.payload",
  "Parameters": {"x.$": "$.x", "exec.$": "$$.Execution.Name"},
  "ResultSelector": {"doubled.$": "$.val"},
  "Assign": {"saved.$": "$.doubled"},
  "ResultPath": "$.taskOut",
  "OutputPath": "$",
  "Next": "After"
}`
	out, err := c.TestState(ctx, &sfn.TestStateInput{
		Definition:      new(taskDef),
		Input:           new(`{"payload":{"x":7},"keep":true}`),
		Context:         new(`{"Execution":{"Name":"custom-exec"}}`),
		InspectionLevel: sfntypes.InspectionLevelTrace,
		RevealSecrets:   true,
		Mock:            &sfntypes.MockInput{Result: new(`{"val":14}`)},
	}, noHostPrefix)
	if err != nil {
		t.Fatal(err)
	}

	if out.Status != sfntypes.TestExecutionStatusSucceeded || aws.ToString(out.NextState) != "After" ||
		out.InspectionData == nil ||
		aws.ToString(out.InspectionData.AfterInputPath) != `{"x":7}` ||
		aws.ToString(out.InspectionData.AfterParameters) != `{"exec":"custom-exec","x":7}` ||
		aws.ToString(out.InspectionData.Result) != `{"val":14}` ||
		aws.ToString(out.InspectionData.AfterResultSelector) != `{"doubled":14}` ||
		aws.ToString(out.InspectionData.Variables) != `{"saved":14}` ||
		out.InspectionData.AfterResultPath == nil {
		t.Fatalf("mock TRACE output: %+v insp=%+v", out, out.InspectionData)
	}

	// Mock.ErrorOutput with Retry + RetrierRetryCount exhausting MaxAttempts -> falls through to Catch with CatchIndex.
	retryCatchDef := `{
  "Type": "Task",
  "Resource": "arn:aws:lambda:us-east-1:000000000000:function:never-called",
  "Retry": [{"ErrorEquals": ["Custom.Boom"], "IntervalSeconds": 2, "MaxAttempts": 2}],
  "Catch": [{"ErrorEquals": ["Custom.Boom"], "ResultPath": "$.err", "Next": "Recover"}],
  "End": true
}`
	retriable, err := c.TestState(ctx, &sfn.TestStateInput{
		Definition:      new(retryCatchDef),
		InspectionLevel: sfntypes.InspectionLevelDebug,
		Mock: &sfntypes.MockInput{
			ErrorOutput: &sfntypes.MockErrorOutput{Error: new("Custom.Boom"), Cause: new("transient")},
		},
		StateConfiguration: &sfntypes.TestStateConfiguration{RetrierRetryCount: new(int32(0))},
	}, noHostPrefix)
	if err != nil || retriable.Status != sfntypes.TestExecutionStatusRetriable ||
		retriable.InspectionData == nil || retriable.InspectionData.ErrorDetails == nil ||
		aws.ToInt32(retriable.InspectionData.ErrorDetails.RetryIndex) != 0 ||
		aws.ToInt32(retriable.InspectionData.ErrorDetails.RetryBackoffIntervalSeconds) != 2 {
		t.Fatalf("retriable: %+v insp=%+v", retriable, retriable.InspectionData)
	}

	caught, err := c.TestState(ctx, &sfn.TestStateInput{
		Definition:      new(retryCatchDef),
		InspectionLevel: sfntypes.InspectionLevelDebug,
		Mock: &sfntypes.MockInput{
			ErrorOutput: &sfntypes.MockErrorOutput{Error: new("Custom.Boom"), Cause: new("exhausted")},
		},
		StateConfiguration: &sfntypes.TestStateConfiguration{RetrierRetryCount: new(int32(2))},
	}, noHostPrefix)
	if err != nil || caught.Status != sfntypes.TestExecutionStatusCaughtError ||
		aws.ToString(caught.NextState) != "Recover" ||
		caught.InspectionData == nil || caught.InspectionData.ErrorDetails == nil ||
		aws.ToInt32(caught.InspectionData.ErrorDetails.CatchIndex) != 0 {
		t.Fatalf("caught after retry exhaustion: %+v insp=%+v", caught, caught.InspectionData)
	}

	// JSONata state with DEBUG inspection populates AfterArguments and Result.
	jsonataDef := `{
  "QueryLanguage": "JSONata",
  "Type": "Task",
  "Resource": "arn:aws:lambda:us-east-1:000000000000:function:never-called",
  "Arguments": {"sum": "{% $states.input.a + $states.input.b %}"},
  "Output": {"total": "{% $states.result.out %}"},
  "End": true
}`
	jOut, err := c.TestState(ctx, &sfn.TestStateInput{
		Definition:      new(jsonataDef),
		Input:           new(`{"a":3,"b":4}`),
		InspectionLevel: sfntypes.InspectionLevelDebug,
		Mock:            &sfntypes.MockInput{Result: new(`{"out":7}`)},
	}, noHostPrefix)
	if err != nil || jOut.Status != sfntypes.TestExecutionStatusSucceeded ||
		aws.ToString(jOut.Output) != `{"total":7}` ||
		jOut.InspectionData == nil || aws.ToString(jOut.InspectionData.AfterArguments) != `{"sum":7}` {
		t.Fatalf("jsonata DEBUG: %+v insp=%+v", jOut, jOut.InspectionData)
	}

	// Distributed Map with MapItemReaderData + ItemsPointer + Mock.Result.
	mapDef := `{
  "Type": "Map",
  "ItemReader": {
    "Resource": "arn:aws:states:::s3:getObject",
    "Parameters": {"Bucket": "b", "Key": "k.json"},
    "ReaderConfig": {"InputType": "JSON", "ItemsPointer": "/orders/items"}
  },
  "ToleratedFailureCount": 1,
  "ItemProcessor": {
    "ProcessorConfig": {"Mode": "DISTRIBUTED", "ExecutionType": "STANDARD"},
    "StartAt": "P", "States": {"P": {"Type": "Pass", "End": true}}
  },
  "End": true
}`
	mapOut, err := c.TestState(ctx, &sfn.TestStateInput{
		Definition:      new(mapDef),
		InspectionLevel: sfntypes.InspectionLevelTrace,
		Mock:            &sfntypes.MockInput{Result: new(`[10, 20]`)},
		StateConfiguration: &sfntypes.TestStateConfiguration{
			MapItemReaderData: new(`{"orders":{"items":[1,2]}}`),
		},
	}, noHostPrefix)
	if err != nil || mapOut.Status != sfntypes.TestExecutionStatusSucceeded ||
		mapOut.InspectionData == nil || aws.ToString(mapOut.InspectionData.AfterItemsPointer) != `[1,2]` {
		t.Fatalf("map TestState: %+v insp=%+v", mapOut, mapOut.InspectionData)
	}

	// Validation errors: mock on Pass, revealSecrets with INFO.
	_, err = c.TestState(ctx, &sfn.TestStateInput{
		Definition: new(`{"Type": "Pass", "End": true}`),
		Mock:       &sfntypes.MockInput{Result: new(`{}`)},
	}, noHostPrefix)
	code(t, err, "ValidationException")

	_, err = c.TestState(ctx, &sfn.TestStateInput{
		Definition:      new(`{"Type": "Pass", "End": true}`),
		InspectionLevel: sfntypes.InspectionLevelInfo,
		RevealSecrets:   true,
	}, noHostPrefix)
	code(t, err, "ValidationException")
}

func TestStepFunctionsDistributedMapRunRedrive(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)
	functions := lambdaClient(srv, "test")
	flakyOut := createFunction(t, functions, "map-flaky", "app.fail")
	flakyChildOut := createFunction(t, functions, "map-flaky-child", "app.fail")

	// Child workflow routes items with mode=="flaky" to a flaky Lambda Task state,
	// and other items directly to Done.
	childDef := `"StartAt":"Route","States":{
  "Route":{
    "Type":"Choice",
    "Choices":[
      {"Variable":"$.mode","StringEquals":"flaky","Next":"CallFlaky"},
      {"Variable":"$.mode","StringEquals":"flaky-child","Next":"CallFlakyChild"}
    ],
    "Default":"Done"
  },
  "CallFlaky":{
    "Type":"Task",
    "Resource":"` + aws.ToString(flakyOut.FunctionArn) + `",
    "ResultSelector":{"ok.$":"$.event.id"},
    "End":true
  },
  "CallFlakyChild":{
    "Type":"Task",
    "Resource":"` + aws.ToString(flakyChildOut.FunctionArn) + `",
    "ResultSelector":{"ok.$":"$.event.id"},
    "End":true
  },
  "Done":{"Type":"Pass","Parameters":{"ok.$":"$.id"},"End":true}
}`
	machine := createMachine(
		t,
		c,
		"dist-redrive",
		distributedDefinition(`"MaxConcurrency":1,"Label":"Gate",`, "STANDARD", childDef),
		sfntypes.StateMachineTypeStandard,
	)

	parent := start(t, c, machine, `[{"id":"1","mode":"ok"},{"id":"2","mode":"flaky"},{"id":"3","mode":"ok"}]`)
	failed := finish(t, c, parent)
	if failed.Status != sfntypes.ExecutionStatusFailed ||
		failed.RedriveStatus != sfntypes.ExecutionRedriveStatusRedrivable {
		t.Fatalf("expected failed redrivable parent: %+v", failed)
	}

	mapARN := waitMapRun(t, c, parent)
	mrBefore, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
	if err != nil || mrBefore.Status != sfntypes.MapRunStatusFailed ||
		mrBefore.ItemCounts.Succeeded != 1 || mrBefore.ItemCounts.Failed != 1 || mrBefore.ItemCounts.Pending != 1 {
		t.Fatalf("mrBefore: %+v %v", mrBefore, err)
	}

	// Fix the flaky Lambda, then redrive the parent execution.
	if _, err = functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("map-flaky"), Handler: new("app.echo"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: parent}); err != nil {
		t.Fatal(err)
	}

	succeeded := finish(t, c, parent)
	if succeeded.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToString(succeeded.Output) != `[{"ok":"1"},{"ok":"2"},{"ok":"3"}]` ||
		aws.ToInt32(succeeded.RedriveCount) != 1 {
		t.Fatalf("redriven parent: %+v output=%s", succeeded, aws.ToString(succeeded.Output))
	}

	mrAfter, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
	if err != nil || mrAfter.Status != sfntypes.MapRunStatusSucceeded ||
		aws.ToInt32(mrAfter.RedriveCount) != 1 || mrAfter.RedriveDate == nil ||
		mrAfter.ItemCounts.Succeeded != 3 || mrAfter.ItemCounts.Failed != 0 ||
		aws.ToInt64(mrAfter.ItemCounts.PendingRedrive) != 0 {
		t.Fatalf("mrAfter: %+v %v", mrAfter, err)
	}

	// Verify item 1 was NOT re-run (RedriveCount == 0), while item 2 WAS redriven in place (RedriveCount == 1).
	children, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{MapRunArn: mapARN})
	if err != nil || len(children.Executions) != 3 {
		t.Fatalf("children: %+v %v", children, err)
	}

	redrivenChildren := 0
	for _, ch := range children.Executions {
		if ch.Status != sfntypes.ExecutionStatusSucceeded {
			t.Fatalf("child not succeeded: %+v", ch)
		}

		if aws.ToInt32(ch.RedriveCount) > 0 {
			redrivenChildren++
		}
	}

	if redrivenChildren != 1 {
		t.Fatalf("expected exactly 1 redriven child execution, got %d", redrivenChildren)
	}

	// Verify parent history recorded MapRunRedriven.
	hist, err := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: parent})
	if err != nil {
		t.Fatal(err)
	}

	sawMapRunRedriven := false
	for _, ev := range hist.Events {
		if ev.Type == sfntypes.HistoryEventTypeMapRunRedriven {
			sawMapRunRedriven = true
		}
	}

	if !sawMapRunRedriven {
		t.Fatal("missing MapRunRedriven event in parent history")
	}

	// Test individual Standard Map Run child redrive directly, followed by parent redrive preserving it.
	parent2 := start(t, c, machine, `[{"id":"1","mode":"ok"},{"id":"4","mode":"flaky-child"}]`)
	finish(t, c, parent2)
	mapARN2 := waitMapRun(t, c, parent2)
	failedChildren, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{
		MapRunArn: mapARN2, StatusFilter: sfntypes.ExecutionStatusFailed,
	})
	if err != nil || len(failedChildren.Executions) != 1 {
		t.Fatalf("failedChildren: %+v %v", failedChildren, err)
	}

	if _, err = functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("map-flaky-child"), Handler: new("app.echo"),
	}); err != nil {
		t.Fatal(err)
	}

	childARN := failedChildren.Executions[0].ExecutionArn
	if _, err = c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: childARN}); err != nil {
		t.Fatal(err)
	}

	childDone := finish(t, c, childARN)
	if childDone.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToInt32(childDone.RedriveCount) != 1 ||
		aws.ToString(childDone.Output) != `{"ok":"4"}` {
		t.Fatalf("redriven child: %+v", childDone)
	}

	// Redriving parent2 now preserves both item 1 and the directly-redriven item 4.
	if _, err = c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: parent2}); err != nil {
		t.Fatal(err)
	}

	parent2Done := finish(t, c, parent2)
	if parent2Done.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToString(parent2Done.Output) != `[{"ok":"1"},{"ok":"4"}]` {
		t.Fatalf("parent2Done: %+v output=%s", parent2Done, aws.ToString(parent2Done.Output))
	}
}

func TestStepFunctionsDistributedItemReaderEnhancements(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)
	objects := s3Client(srv, "test", "us-east-1")
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("reader-enh")}); err != nil {
		t.Fatal(err)
	}

	put := func(key string, body []byte) {
		if _, err := objects.PutObject(ctx, &s3.PutObjectInput{
			Bucket: new("reader-enh"), Key: new(key), Body: bytes.NewReader(body),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. ItemsPointer (including RFC 6901 ~1 and ~0 escaping) on gzipped JSON.
	put("nested.json.gz", gzipBytes(t, `{"a/b":{"c~d":[{"id":1},{"id":2}]}}`))
	ptrDef := distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
  "Parameters":{"Bucket":"reader-enh","Key":"nested.json.gz"},
  "ReaderConfig":{"InputType":"JSON","ItemsPointer":"/a~1b/c~0d"}},`, "STANDARD", passMapProcessor)
	mPtr := createMachine(t, c, "reader-pointer-gz", ptrDef, "")
	out := finish(t, c, start(t, c, mPtr, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(out.Output) != `[{"id":1},{"id":2}]` {
		t.Fatalf("ItemsPointer+gz: %+v output=%s", out, aws.ToString(out.Output))
	}

	// 2. Transparent gzip decompression on JSONL (without .gz extension, via magic bytes) and CSV (.gz).
	put("lines-gz-magic", gzipBytes(t, "{\"v\":10}\n{\"v\":20}\n"))
	jsonlDef := distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
  "Parameters":{"Bucket":"reader-enh","Key":"lines-gz-magic"},
  "ReaderConfig":{"InputType":"JSONL"}},`, "STANDARD", passMapProcessor)
	mJSONL := createMachine(t, c, "reader-jsonl-gz", jsonlDef, "")
	out = finish(t, c, start(t, c, mJSONL, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(out.Output) != `[{"v":10},{"v":20}]` {
		t.Fatalf("JSONL gz: %+v output=%s", out, aws.ToString(out.Output))
	}

	put("rows.csv.gz", gzipBytes(t, "k,v\na,1\nb,2\n"))
	csvDef := distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
  "Parameters":{"Bucket":"reader-enh","Key":"rows.csv.gz"},
  "ReaderConfig":{"InputType":"CSV","CSVHeaderLocation":"FIRST_ROW"}},`, "STANDARD", passMapProcessor)
	mCSV := createMachine(t, c, "reader-csv-gz", csvDef, "")
	out = finish(t, c, start(t, c, mCSV, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToString(out.Output) != `[{"k":"a","v":"1"},{"k":"b","v":"2"}]` {
		t.Fatalf("CSV gz: %+v output=%s", out, aws.ToString(out.Output))
	}

	// 3. S3 Inventory JSON MANIFEST pointing to gzipped CSV data files.
	put("inv/data1.csv.gz", gzipBytes(t, "src-bkt,obj-1,100\nsrc-bkt,obj-2,200\n"))
	put("inv/manifest.json", []byte(`{
  "sourceBucket": "src-bkt",
  "destinationBucket": "arn:aws:s3:::reader-enh",
  "fileFormat": "CSV",
  "fileSchema": "Bucket, Key, Size",
  "files": [{"key": "inv/data1.csv.gz"}]
}`))
	manifestDef := distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
  "Parameters":{"Bucket":"reader-enh","Key":"inv/manifest.json"},
  "ReaderConfig":{"InputType":"MANIFEST"}},`, "STANDARD", passMapProcessor)
	mManifest := createMachine(t, c, "reader-manifest-json", manifestDef, "")
	out = finish(t, c, start(t, c, mManifest, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToString(out.Output) != `[{"Bucket":"src-bkt","Key":"obj-1","Size":"100"},`+
			`{"Bucket":"src-bkt","Key":"obj-2","Size":"200"}]` {
		t.Fatalf("JSON MANIFEST: %+v output=%s", out, aws.ToString(out.Output))
	}

	// 4. CSV MANIFEST.
	put("inv/manifest.csv", []byte("src-bkt,file-a\nsrc-bkt,file-b\n"))
	csvManifestDef := distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
  "Parameters":{"Bucket":"reader-enh","Key":"inv/manifest.csv"},
  "ReaderConfig":{"InputType":"MANIFEST"}},`, "STANDARD", passMapProcessor)
	mCSVManifest := createMachine(t, c, "reader-manifest-csv", csvManifestDef, "")
	out = finish(t, c, start(t, c, mCSVManifest, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded ||
		aws.ToString(out.Output) != `[{"Bucket":"src-bkt","Key":"file-a"},{"Bucket":"src-bkt","Key":"file-b"}]` {
		t.Fatalf("CSV MANIFEST: %+v output=%s", out, aws.ToString(out.Output))
	}

	// 5. LOAD_AND_FLATTEN on s3:listObjectsV2 across multiple JSON/gzip objects.
	put("parts/01.json", []byte(`[1,2]`))
	put("parts/02.json.gz", gzipBytes(t, `[3,4]`))
	flattenDef := distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:listObjectsV2",
  "Parameters":{"Bucket":"reader-enh","Prefix":"parts/"},
  "ReaderConfig":{"InputType":"JSON","Transformation":"LOAD_AND_FLATTEN"}},`, "STANDARD", passMapProcessor)
	mFlatten := createMachine(t, c, "reader-load-flatten", flattenDef, "")
	out = finish(t, c, start(t, c, mFlatten, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(out.Output) != `[1,2,3,4]` {
		t.Fatalf("LOAD_AND_FLATTEN: %+v output=%s", out, aws.ToString(out.Output))
	}
}

func TestStepFunctionsResetCancelsRunningExecutions(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)
	q := sqsClient(srv, "test")
	queueURL := createQueue(t, q, "reset-cb-queue", nil)

	// 1. Long-running Wait execution.
	waitSM := createMachine(t, c, "reset-wait",
		`{"StartAt":"W","States":{"W":{"Type":"Wait","Seconds":120,"End":true}}}`, "")
	waitExec := start(t, c, waitSM, `{}`)

	// 2. Activity execution + active GetActivityTask poller.
	act, err := c.CreateActivity(ctx, &sfn.CreateActivityInput{Name: new("reset-act")})
	if err != nil {
		t.Fatal(err)
	}

	actSM := createMachine(t, c, "reset-act-sm",
		`{"StartAt":"A","States":{"A":{"Type":"Task","Resource":"`+aws.ToString(act.ActivityArn)+`","End":true}}}`, "")
	start(t, c, actSM, `{"a":1}`)

	// Poll a second idle activity so a GetActivityTask waiter is blocked when reset runs.
	idleAct, err := c.CreateActivity(ctx, &sfn.CreateActivityInput{Name: new("reset-idle-act")})
	if err != nil {
		t.Fatal(err)
	}

	pollerDone := make(chan error, 1)
	go func() {
		_, pollErr := c.GetActivityTask(ctx, &sfn.GetActivityTaskInput{ActivityArn: idleAct.ActivityArn})
		pollerDone <- pollErr
	}()

	// 3. Callback (.waitForTaskToken) execution.
	cbSM := createMachine(t, c, "reset-cb", `{"StartAt":"Ask","States":{"Ask":{
  "Type":"Task","Resource":"arn:aws:states:::sqs:sendMessage.waitForTaskToken",
  "Parameters":{"QueueUrl":"`+queueURL+`","MessageBody":{"token.$":"$$.Task.Token"}},
  "End":true}}}`, "")
	start(t, c, cbSM, `{}`)

	// Wait until the Wait execution has entered WaitStateEntered and callback sent its token.
	waitFor(t, "wait execution running", func() bool {
		hist, histErr := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: waitExec})
		if histErr != nil {
			return false
		}

		for _, ev := range hist.Events {
			if ev.Type == sfntypes.HistoryEventTypeWaitStateEntered {
				return true
			}
		}

		return false
	})

	status, body := post(t, srv, "/_tinystack/reset", nil)
	if status != http.StatusOK {
		t.Fatalf("reset status=%d body=%s", status, body)
	}

	// The blocked GetActivityTask poller must wake up promptly on reset.
	select {
	case pollErr := <-pollerDone:
		if pollErr != nil {
			t.Fatalf("poller error: %v", pollErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetActivityTask poller did not wake up on reset")
	}

	listed, err := c.ListStateMachines(ctx, &sfn.ListStateMachinesInput{})
	if err != nil || len(listed.StateMachines) != 0 {
		t.Fatalf("state machines remained after reset: %+v %v", listed, err)
	}

	_, err = c.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: waitExec})
	code(t, err, "ExecutionDoesNotExist")

	// New executions work cleanly after reset.
	fresh := createMachine(t, c, "after-reset", `{"StartAt":"Done","States":{"Done":{"Type":"Succeed"}}}`, "")
	done := finish(t, c, start(t, c, fresh, `{"ok":true}`))
	if done.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(done.Output) != `{"ok":true}` {
		t.Fatalf("post-reset execution failed: %+v", done)
	}
}

func TestCloudWatchStepFunctionsExpandedMetrics(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	c := sfnClient(srv)
	cw := cloudwatchClient(srv)
	functions := lambdaClient(srv, "test")
	q := sqsClient(srv, "test")
	const namespace = "AWS/States"

	fnOut := createFunction(t, functions, "sfn-metrics-fn", "app.fail")
	fnARN := aws.ToString(fnOut.FunctionArn)
	queueURL := createQueue(t, q, "sfn-metrics-q", nil)

	// 1. Lambda + Service Integration + Redrive metrics.
	workflowDef := `{
  "StartAt": "Send",
  "States": {
    "Send": {
      "Type": "Task",
      "Resource": "arn:aws:states:::sqs:sendMessage",
      "Parameters": {"QueueUrl": "` + queueURL + `", "MessageBody": "hi"},
      "Next": "Call"
    },
    "Call": {
      "Type": "Task",
      "Resource": "` + fnARN + `",
      "End": true
    }
  }
}`
	smARN := createMachine(t, c, "expanded-metrics-sm", workflowDef, sfntypes.StateMachineTypeStandard)
	exec := start(t, c, smARN, `{}`)
	if failed := finish(t, c, exec); failed.Status != sfntypes.ExecutionStatusFailed {
		t.Fatalf("expected failed execution: %+v", failed)
	}

	if _, err := functions.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: new("sfn-metrics-fn"), Handler: new("app.echo"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: exec}); err != nil {
		t.Fatal(err)
	}

	if ok := finish(t, c, exec); ok.Status != sfntypes.ExecutionStatusSucceeded {
		t.Fatalf("expected redriven execution to succeed: %+v", ok)
	}

	smDims := dimensions("StateMachineArn", aws.ToString(smARN))
	expectBuiltin(t, cw, namespace, "ExecutionsRedriven", "Count", smDims, 1)
	expectBuiltin(t, cw, namespace, "ExecutionsSucceeded", "Count", smDims, 1)

	fnDims := dimensions("LambdaFunctionArn", fnARN)
	expectBuiltin(t, cw, namespace, "LambdaFunctionsScheduled", "Count", fnDims, 2)
	expectBuiltin(t, cw, namespace, "LambdaFunctionsStarted", "Count", fnDims, 2)
	expectBuiltin(t, cw, namespace, "LambdaFunctionsFailed", "Count", fnDims, 1)
	expectBuiltin(t, cw, namespace, "LambdaFunctionsSucceeded", "Count", fnDims, 1)
	builtinMetric(t, cw, namespace, "LambdaFunctionRunTime", fnDims, func(b builtin) bool {
		return b.count == 2 && b.unit == "Milliseconds"
	})
	builtinMetric(t, cw, namespace, "LambdaFunctionTime", fnDims, func(b builtin) bool {
		return b.count == 2 && b.unit == "Milliseconds"
	})

	svcDims := dimensions("ServiceIntegrationResourceArn", "arn:aws:states:::sqs:sendMessage")
	expectBuiltin(t, cw, namespace, "ServiceIntegrationsScheduled", "Count", svcDims, 1)
	expectBuiltin(t, cw, namespace, "ServiceIntegrationsStarted", "Count", svcDims, 1)
	expectBuiltin(t, cw, namespace, "ServiceIntegrationsSucceeded", "Count", svcDims, 1)
	builtinMetric(t, cw, namespace, "ServiceIntegrationRunTime", svcDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})
	builtinMetric(t, cw, namespace, "ServiceIntegrationTime", svcDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})

	// 2. Activity metrics.
	act, err := c.CreateActivity(ctx, &sfn.CreateActivityInput{Name: new("metrics-act")})
	if err != nil {
		t.Fatal(err)
	}

	actSM := createMachine(t, c, "metrics-act-sm", `{"StartAt":"Do","States":{"Do":{"Type":"Task",`+
		`"Resource":"`+aws.ToString(act.ActivityArn)+`","End":true}}}`, "")
	actExec := start(t, c, actSM, `{"x":1}`)
	task, err := c.GetActivityTask(ctx, &sfn.GetActivityTaskInput{
		ActivityArn: act.ActivityArn, WorkerName: new("w1"),
	})
	if err != nil || task.TaskToken == nil {
		t.Fatalf("GetActivityTask: %+v %v", task, err)
	}

	if _, err = c.SendTaskSuccess(ctx, &sfn.SendTaskSuccessInput{
		TaskToken: task.TaskToken, Output: new(`{"done":true}`),
	}); err != nil {
		t.Fatal(err)
	}

	finish(t, c, actExec)
	actDims := dimensions("ActivityArn", aws.ToString(act.ActivityArn))
	expectBuiltin(t, cw, namespace, "ActivitiesScheduled", "Count", actDims, 1)
	expectBuiltin(t, cw, namespace, "ActivitiesStarted", "Count", actDims, 1)
	expectBuiltin(t, cw, namespace, "ActivitiesSucceeded", "Count", actDims, 1)
	builtinMetric(t, cw, namespace, "ActivityScheduleTime", actDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})
	builtinMetric(t, cw, namespace, "ActivityRunTime", actDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})
	builtinMetric(t, cw, namespace, "ActivityTime", actDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})

	// 3. Map Run metrics.
	mapSM := createMachine(t, c, "metrics-map-sm", distributedDefinition(``, "STANDARD", passMapProcessor), "")
	finish(t, c, start(t, c, mapSM, `[1,2]`))
	mapDims := dimensions("StateMachineArn", aws.ToString(mapSM))
	expectBuiltin(t, cw, namespace, "MapRunStarted", "Count", mapDims, 1)
	expectBuiltin(t, cw, namespace, "MapRunSucceeded", "Count", mapDims, 1)
	builtinMetric(t, cw, namespace, "MapRunTime", mapDims, func(b builtin) bool {
		return b.count == 1 && b.unit == "Milliseconds"
	})
}

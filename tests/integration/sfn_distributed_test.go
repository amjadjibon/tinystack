package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
)

func distributedDefinition(fields, kind, processor string) string {
	return fmt.Sprintf(`{"StartAt":"Map","States":{"Map":{"Type":"Map",%s
 "ItemProcessor":{"ProcessorConfig":{"Mode":"DISTRIBUTED","ExecutionType":%q},%s},"End":true}}}`,
		fields, kind, processor)
}

const passMapProcessor = `"StartAt":"Pass","States":{"Pass":{"Type":"Pass","End":true}}`

func waitMapRun(t *testing.T, c *sfn.Client, parent *string) *string {
	t.Helper()
	for range 200 {
		out, err := c.ListMapRuns(context.Background(), &sfn.ListMapRunsInput{ExecutionArn: parent})
		if err != nil {
			t.Fatal(err)
		}

		if len(out.MapRuns) != 0 {
			return out.MapRuns[0].MapRunArn
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("Map Run was not created")
	return nil
}

func TestStepFunctionsDistributedReaders(t *testing.T) {
	srv := server(t)
	c := sfnClient(srv)
	objects := s3Client(srv, "test", "us-east-1")
	ctx := context.Background()
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("map-input")}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, inputType, body, config, want string }{
		{"json", "JSON", `[1,2,3]`, ``, `[1,2,3]`},
		{"jsonl", "JSONL", "{\"x\":1}\n{\"x\":2}\n", ``, `[{"x":1},{"x":2}]`},
		{"csv", "CSV", "name,count\na,1\nb,2\n", `,"CSVHeaderLocation":"FIRST_ROW"`, `[{"count":"1","name":"a"},{"c` +
			`ount":"2","name":"b"}]`},
		{"given", "CSV", "a|1\nb|2\n", `,"CSVHeaderLocation":"GIVEN","CSVHeaders":["name","count"],"CSVDe` +
			`limiter":"PIPE"`, `[{"count":"1","name":"a"},{"count":"2","name":` +
			`"b"}]`},
	} {
		{
			if _, err := objects.PutObject(ctx, &s3.PutObjectInput{
				Bucket: new("map-input"), Key: new(tc.name), Body: strings.NewReader(tc.body),
			}); err != nil {
				t.Fatal(err)
			}

			fields := fmt.Sprintf(`"Label":"Reader","ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
    "Parameters":{"Bucket":"map-input","Key":%q},"ReaderConfig":{"InputType":%q%s}},`, tc.name, tc.inputType, tc.config)
			machine := createMachine(
				t,
				c,
				"reader-"+tc.name,
				distributedDefinition(fields, "STANDARD", passMapProcessor),
				"",
			)
			parent := start(t, c, machine, `{}`)
			out := finish(t, c, parent)
			if out.Status != sfntypes.ExecutionStatusSucceeded || aws.ToString(out.Output) != tc.want {
				t.Fatalf("%+v output=%s", out, aws.ToString(out.Output))
			}

			mapARN := waitMapRun(t, c, parent)
			mr, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
			if err != nil || mr.Status != sfntypes.MapRunStatusSucceeded ||
				mr.ItemCounts.Succeeded != mr.ItemCounts.Total {
				t.Fatalf("%+v %v", mr, err)
			}

			children, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{MapRunArn: mapARN, MaxResults: 1})
			if err != nil || len(children.Executions) != 1 || children.NextToken == nil {
				t.Fatalf("%+v %v", children, err)
			}

			child := children.Executions[0]
			described, err := c.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: child.ExecutionArn})
			if err != nil || aws.ToString(described.MapRunArn) != *mapARN {
				t.Fatalf("%+v %v", described, err)
			}

			history, err := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: child.ExecutionArn})
			if err != nil || len(history.Events) != 4 {
				t.Fatalf("%+v %v", history, err)
			}

			parentHistory, err := c.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{ExecutionArn: parent})
			if err != nil {
				t.Fatal(err)
			}

			kinds := map[sfntypes.HistoryEventType]int{}
			for _, event := range parentHistory.Events {
				kinds[event.Type]++
			}

			if kinds[sfntypes.HistoryEventTypeMapRunStarted] != 1 ||
				kinds[sfntypes.HistoryEventTypeMapRunSucceeded] != 1 ||
				kinds[sfntypes.HistoryEventTypePassStateEntered] != 0 {
				t.Fatalf("parent history: %v", kinds)
			}
		}
	}

	fields := `"ItemReader":{"Resource":"arn:aws:states:::s3:listObjectsV2","Par` +
		`ameters":{"Bucket":"map-input","Prefix":"json"},"ReaderConfig":{"` +
		`MaxItems":1}},`
	machine := createMachine(t, c, "list-reader", distributedDefinition(fields, "STANDARD", passMapProcessor), "")
	out := finish(t, c, start(t, c, machine, `{}`))
	if out.Status != sfntypes.ExecutionStatusSucceeded || !strings.Contains(aws.ToString(out.Output), `"Key":"json"`) {
		t.Fatalf("%+v", out)
	}
}

func TestStepFunctionsDistributedBatchAndExpress(t *testing.T) {
	srv := server(t)
	c := sfnClient(srv)
	ctx := context.Background()
	machine := createMachine(t, c, "batches", distributedDefinition(`"ItemsPath":"$.items",
 "ItemSelector":{"value.$":"$$.Map.Item.Value"},
 "ItemBatcher":{"MaxItemsPerBatchPath":"$.size","BatchInput":{"greeting":"hi"}},`, "EXPRESS", passMapProcessor), "")
	parent := start(t, c, machine, `{"items":[1,2,3],"size":2}`)
	out := finish(t, c, parent)
	if aws.ToString(
		out.Output,
	) != `[{"BatchInput":{"greeting":"hi"},"Items":[{"value":1},{"value":2}`+
		`]},{"BatchInput":{"greeting":"hi"},"Items":[{"value":3}]}]` {
		t.Fatalf("%+v output=%s", out, aws.ToString(out.Output))
	}

	mapARN := waitMapRun(t, c, parent)
	mr, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
	if err != nil || mr.ItemCounts.Total != 3 || mr.ExecutionCounts.Total != 2 {
		t.Fatalf("%+v %v", mr, err)
	}

	children, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{MapRunArn: mapARN})
	if err != nil || len(children.Executions) != 0 {
		t.Fatalf("%+v %v", children, err)
	}

	definition := `{"QueryLanguage":"JSONata","StartAt":"Map","States":{"Map":{"Type":"Map",
 "Items":"{% $states.input.items %}","ItemSelector":{"v":"{% $states.context.Map.Item.Value * 2 %}"},
 "MaxConcurrency":"{% 2 %}","ItemProcessor":{"ProcessorConfig":{"Mode":"DISTRIBUTED","ExecutionType":"STANDARD"},
 "StartAt":"Pass","States":{"Pass":{"Type":"Pass","End":true}}},"Output":"{% $states.result %}","End":true}}}`
	machine = createMachine(t, c, "jsonata-map", definition, "")
	out = finish(t, c, start(t, c, machine, `{"items":[1,2]}`))
	if aws.ToString(out.Output) != `[{"v":2},{"v":4}]` {
		t.Fatalf("%+v", out)
	}
}

func TestStepFunctionsDistributedFailuresAndWriter(t *testing.T) {
	srv := server(t)
	c := sfnClient(srv)
	objects := s3Client(srv, "test", "us-east-1")
	ctx := context.Background()
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("map-results")}); err != nil {
		t.Fatal(err)
	}

	processor := `"StartAt":"Fail","States":{"Fail":{"Type":"Fail","Error":"BadItem"}}`
	for _, tc := range []struct {
		name, fields string
		status       sfntypes.ExecutionStatus
	}{
		{"tolerated", `"ToleratedFailureCount":3,`, sfntypes.ExecutionStatusSucceeded},
		{"percentage", `"ToleratedFailurePercentagePath":"$.tolerance",`, sfntypes.ExecutionStatusSucceeded},
		{"threshold", `"ToleratedFailureCount":1,"MaxConcurrency":1,`, sfntypes.ExecutionStatusFailed},
	} {
		machine := createMachine(t, c, tc.name, distributedDefinition(`"ItemsPath":"$.items",`+tc.fields+`
  "ResultWriter":{"Resource":"arn:aws:states:::s3:putObject",
 "Parameters":{"Bucket":"map-results","Prefix":"out"}},`, "STANDARD", processor), "")
		parent := start(t, c, machine, `{"items":[1,2,3],"tolerance":100}`)
		out := finish(t, c, parent)
		if out.Status != tc.status {
			t.Fatalf("%+v", out)
		}

		if tc.status == sfntypes.ExecutionStatusFailed &&
			aws.ToString(out.Error) != "States.ExceedToleratedFailureThreshold" {
			t.Fatalf("%+v", out)
		}

		mapARN := waitMapRun(t, c, parent)
		listed, err := objects.ListObjectsV2(
			ctx,
			&s3.ListObjectsV2Input{Bucket: new("map-results"), Prefix: new("out/")},
		)
		if err != nil {
			t.Fatal(err)
		}

		found := false
		for _, object := range listed.Contents {
			if !strings.HasSuffix(*object.Key, "/manifest.json") {
				continue
			}

			obj, getErr := objects.GetObject(ctx, &s3.GetObjectInput{Bucket: new("map-results"), Key: object.Key})
			if getErr != nil {
				t.Fatal(getErr)
			}

			body, readErr := io.ReadAll(obj.Body)
			obj.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}

			if strings.Contains(string(body), *mapARN) && strings.Contains(string(body), "FAILED_0.json") {
				found = true
			}
		}

		if !found {
			t.Fatal("manifest did not reference failed results")
		}

		_, err = c.RedriveExecution(ctx, &sfn.RedriveExecutionInput{ExecutionArn: parent})
		code(t, err, "ExecutionNotRedrivable")
	}

	machine := createMachine(
		t,
		c,
		"success-writer",
		distributedDefinition(`"ResultWriter":{"Resource":"arn:aws:states:::s3:putObject",
 "Parameters":{"Bucket":"map-results","Prefix":"success"},
 "WriterConfig":{"Transformation":"FLATTEN","OutputType":"JSONL"}},`, "STANDARD", passMapProcessor),
		"",
	)
	out := finish(t, c, start(t, c, machine, `[[1,2],[3]]`))
	if out.Status != sfntypes.ExecutionStatusSucceeded {
		t.Fatalf("%+v", out)
	}

	var result struct{ ResultWriterDetails struct{ Bucket, Key string } }
	if err := json.Unmarshal([]byte(*out.Output), &result); err != nil {
		t.Fatal(err)
	}

	key := strings.TrimSuffix(result.ResultWriterDetails.Key, "manifest.json") + "SUCCEEDED_0.json"
	object, err := objects.GetObject(ctx, &s3.GetObjectInput{Bucket: new("map-results"), Key: new(key)})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || string(body) != "1\n2\n3\n" {
		t.Fatalf("%s %v", body, err)
	}
}

func TestStepFunctionsDistributedUpdateAndStop(t *testing.T) {
	srv := server(t)
	c := sfnClient(srv)
	ctx := context.Background()
	objects := s3Client(srv, "test", "us-east-1")
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("abort-results")}); err != nil {
		t.Fatal(err)
	}

	processor := `"StartAt":"Wait","States":{"Wait":{"Type":"Wait","Seconds":1,"End":true}}`
	machine := createMachine(
		t,
		c,
		"update-map",
		distributedDefinition(`"MaxConcurrencyPath":"$.concurrency","ItemsPath":"$.items",
 "ResultWriter":{"Resource":"arn:aws:states:::s3:putObject",
 "Parameters":{"Bucket":"abort-results","Prefix":"out"}},`, "STANDARD", processor),
		"",
	)
	parent := start(t, c, machine, `{"items":[1,2,3,4],"concurrency":1}`)
	mapARN := waitMapRun(t, c, parent)
	for range 100 {
		mr, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
		if err != nil {
			t.Fatal(err)
		}

		if mr.ExecutionCounts.Running == 1 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if _, err := c.UpdateMapRun(ctx, &sfn.UpdateMapRunInput{
		MapRunArn: mapARN, MaxConcurrency: new(int32(3)), ToleratedFailurePercentage: new(float32(100)),
		ToleratedFailureCount: new(int64(4)),
	}); err != nil {
		t.Fatal(err)
	}

	for range 100 {
		mr, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
		if err != nil {
			t.Fatal(err)
		}

		if mr.ExecutionCounts.Running == 3 {
			if mr.MaxConcurrency != 3 || mr.ToleratedFailureCount != 4 || mr.ToleratedFailurePercentage != 100 {
				t.Fatalf("%+v", mr)
			}

			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if _, err := c.StopExecution(ctx, &sfn.StopExecutionInput{ExecutionArn: parent}); err != nil {
		t.Fatal(err)
	}

	out := finish(t, c, parent)
	if out.Status != sfntypes.ExecutionStatusAborted {
		t.Fatalf("%+v", out)
	}

	mr, err := c.DescribeMapRun(ctx, &sfn.DescribeMapRunInput{MapRunArn: mapARN})
	if err != nil || mr.Status != sfntypes.MapRunStatusAborted || mr.ExecutionCounts.Running != 0 ||
		mr.ExecutionCounts.Aborted != 3 {
		t.Fatalf("%+v %v", mr, err)
	}

	children, err := c.ListExecutions(ctx, &sfn.ListExecutionsInput{MapRunArn: mapARN})
	if err != nil {
		t.Fatal(err)
	}

	for _, child := range children.Executions {
		if child.Status != sfntypes.ExecutionStatusAborted {
			t.Fatalf("%+v", child)
		}
	}

	listed, err := objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("abort-results")})
	if err != nil {
		t.Fatal(err)
	}

	pending := false
	for _, object := range listed.Contents {
		if strings.HasSuffix(aws.ToString(object.Key), "/PENDING_0.json") {
			pending = true
		}
	}

	if !pending {
		t.Fatal("aborted Map Run did not export pending child inputs")
	}
}

func TestStepFunctionsDistributedValidation(t *testing.T) {
	srv := server(t)
	c := sfnClient(srv)
	ctx := context.Background()
	for _, fields := range []string{
		`"Label":"bad label",`,
		`"MaxConcurrency":1,"MaxConcurrencyPath":"$.limit",`,
		`"ToleratedFailurePercentage":101,`,
		`"ToleratedFailureCount":null,`,
		`"ToleratedFailureCount":"",`,
		`"QueryLanguage":"JSONata","ToleratedFailureCount":"invalid",`,
		`"ItemBatcher":{"MaxItemsPerBatch":0},`,
		`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject","Paramet` +
			`ers":{"Bucket":"b","Key":"k"},"ReaderConfig":{"InputType":"PARQUE` +
			`T"}},`,
		`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject","Paramet` +
			`ers":{"Bucket":"b","Key":"k"},"ReaderConfig":{"InputType":"CSV"}}` +
			`,`,
		`"ResultWriter":{"Parameters":{"Bucket":"b","Prefix":"x"}},`,
		`"ResultWriter":{"Resource":"wrong","Parameters":{"Bucket":"b","Prefix":"x"}},`,
	} {
		_, err := c.CreateStateMachine(ctx, &sfn.CreateStateMachineInput{
			Name: new(
				"invalid-map",
			),
			RoleArn:    new(sfnRole),
			Definition: new(distributedDefinition(fields, "STANDARD", passMapProcessor)),
		})
		code(t, err, "InvalidDefinition")
	}

	machine := createMachine(
		t,
		c,
		"reader-failure",
		distributedDefinition(`"ItemReader":{"Resource":"arn:aws:states:::s3:getObject",
 "Parameters":{"Bucket":"missing","Key":"x"},"ReaderConfig":{"InputType":"JSON"}},`, "STANDARD", passMapProcessor),
		"",
	)
	out := finish(t, c, start(t, c, machine, `{}`))
	if aws.ToString(out.Error) != "States.ItemReaderFailed" {
		t.Fatalf("%+v", out)
	}

	machine = createMachine(
		t,
		c,
		"preview-writer",
		distributedDefinition(
			`"ResultWriter":{"WriterConfig":{"Transformation":"FLATTEN"}},`,
			"STANDARD",
			passMapProcessor,
		),
		"",
	)
	out = finish(t, c, start(t, c, machine, `[[1],[2]]`))
	if aws.ToString(out.Output) != `[1,2]` {
		t.Fatalf("%+v", out)
	}
}

func TestStepFunctionsDistributedPersistence(t *testing.T) {
	dir := t.TempDir()
	srv, stop := persistent(t, dir, &fakeRuntime{})
	c := sfnClient(srv)
	processor := `"StartAt":"Wait","States":{"Wait":{"Type":"Wait","Seconds":60,"End":true}}`
	machine := createMachine(
		t,
		c,
		"persistent-map",
		distributedDefinition(`"MaxConcurrency":1,`, "STANDARD", processor),
		"",
	)
	parent := start(t, c, machine, `[1,2]`)
	mapARN := waitMapRun(t, c, parent)
	stop()
	srv, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = sfnClient(srv)
	out := finish(t, c, parent)
	if out.Status != sfntypes.ExecutionStatusAborted {
		t.Fatalf("%+v", out)
	}

	mr, err := c.DescribeMapRun(context.Background(), &sfn.DescribeMapRunInput{MapRunArn: mapARN})
	if err != nil || mr.Status != sfntypes.MapRunStatusAborted || mr.ItemCounts.Running != 0 {
		t.Fatalf("%+v %v", mr, err)
	}

	children, err := c.ListExecutions(context.Background(), &sfn.ListExecutionsInput{MapRunArn: mapARN})
	if err != nil {
		t.Fatal(err)
	}

	for _, child := range children.Executions {
		if child.Status != sfntypes.ExecutionStatusAborted {
			t.Fatalf("%+v", child)
		}
	}
}

func TestStepFunctionsDistributedAdditionalPaths(t *testing.T) {
	srv := server(t)
	c := sfnClient(srv)
	ctx := context.Background()
	machine := createMachine(t, c, "byte-batches", distributedDefinition(`"ItemsPath":"$.items",
 "ItemBatcher":{"MaxInputBytesPerBatchPath":"$.bytes"},`, "STANDARD", passMapProcessor), "")
	out := finish(t, c, start(t, c, machine, `{"items":[1,2,3],"bytes":13}`))
	if aws.ToString(out.Output) != `[{"Items":[1]},{"Items":[2]},{"Items":[3]}]` {
		t.Fatalf("%+v", out)
	}

	machine = createMachine(t, c, "object-map", distributedDefinition(`"ItemSelector":{
 "key.$":"$$.Map.Item.Key","value.$":"$$.Map.Item.Value","source.$":"$$.Map.Item.Source"},`,
		"STANDARD", passMapProcessor), "")
	out = finish(t, c, start(t, c, machine, `{"a":1,"b":2}`))
	if aws.ToString(out.Output) != `[{"key":"a","source":"STATE_DATA","value":1},`+
		`{"key":"b","source":"STATE_DATA","value":2}]` {
		t.Fatalf("%+v", out)
	}

	machine = createMachine(t, c, "express-metadata", distributedDefinition(`"ResultWriter":{
 "WriterConfig":{"Transformation":"NONE"}},`, "EXPRESS", passMapProcessor), "")
	parent := start(t, c, machine, `[1]`)
	out = finish(t, c, parent)
	var children []struct{ ExecutionArn string }
	if err := json.Unmarshal([]byte(aws.ToString(out.Output)), &children); err != nil {
		t.Fatal(err)
	}

	if len(children) != 1 {
		t.Fatalf("%+v", out)
	}

	_, err := c.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: new(children[0].ExecutionArn)})
	code(t, err, "ExecutionDoesNotExist")

	large, err := json.Marshal(strings.Repeat("x", 140<<10))
	if err != nil {
		t.Fatal(err)
	}

	processor := `"StartAt":"Pass","States":{"Pass":{"Type":"Pass","Result":` + string(large) + `,"End":true}}`
	machine = createMachine(t, c, "map-output-limit", distributedDefinition(``, "STANDARD", processor), "")
	out = finish(t, c, start(t, c, machine, `[1,2]`))
	if aws.ToString(out.Error) != "States.DataLimitExceeded" {
		t.Fatalf("%+v", out)
	}

	machine = createMachine(t, c, "writer-failure", distributedDefinition(`"ResultWriter":{
 "Resource":"arn:aws:states:::s3:putObject","Parameters":{"Bucket":"missing","Prefix":"out"}},`,
		"STANDARD", passMapProcessor), "")
	out = finish(t, c, start(t, c, machine, `[1]`))
	if aws.ToString(out.Error) != "States.ResultWriterFailed" {
		t.Fatalf("%+v", out)
	}
}

package integration_test

import (
	"net/http"
	"testing"
)

func TestConsolePipesLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	callStatus := func(service, operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation, account, region, input), status)
	}

	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callStatus("pipes", operation, input, http.StatusOK)["output"].(map[string]any)
	}

	callStatus("sqs", "CreateQueue", map[string]any{"QueueName": "pipe-source"}, http.StatusOK)
	const queue = "arn:aws:sqs:eu-west-1:000000000000:pipe-source"
	const arn = "arn:aws:pipes:eu-west-1:000000000000:pipe/console-pipe"
	call("CreatePipe", map[string]any{
		"Name": "console-pipe", "Source": queue, "Target": queue,
		"RoleArn": "arn:aws:iam::000000000000:role/pipes", "DesiredState": "STOPPED",
		"Tags": map[string]string{"env": "dev"},
	})
	name := map[string]any{"Name": "console-pipe"}
	if out := call("DescribePipe", name); out["CurrentState"] != "STOPPED" || out["Source"] != queue {
		t.Fatal(out)
	}

	call("UpdatePipe", map[string]any{
		"Name": "console-pipe", "Description": "updated",
		"SourceParameters": map[string]any{
			"SqsQueueParameters": map[string]any{"BatchSize": 2},
		},
	})
	if out := call("DescribePipe", name); out["Description"] != "updated" {
		t.Fatal(out)
	}

	call("StartPipe", name)
	if out := call("DescribePipe", name); out["DesiredState"] != "RUNNING" {
		t.Fatal(out)
	}

	call("StopPipe", name)
	call("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]string{"team": "core"}})
	tags := call("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any)
	if len(tags) != 2 {
		t.Fatal(tags)
	}

	call("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})
	if out := call("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any); len(out) != 1 {
		t.Fatal(out)
	}

	if out := call("ListPipes", map[string]any{"Limit": 100})["Pipes"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	call("DeletePipe", name)
	callStatus("pipes", "DescribePipe", name, http.StatusNotFound)
}

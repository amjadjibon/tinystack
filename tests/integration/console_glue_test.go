package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleGlueLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	callStatus := func(operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, "glue", operation, account, region, input), status)
	}

	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callStatus(operation, input, http.StatusOK)["output"].(map[string]any)
	}

	name := map[string]any{"Name": "console-db"}
	call("CreateDatabase", map[string]any{"DatabaseInput": name})
	call(
		"UpdateDatabase",
		map[string]any{
			"Name":          "console-db",
			"DatabaseInput": map[string]any{"Name": "console-db", "Description": "updated"},
		},
	)
	if out := call("GetDatabase", name)["Database"].(map[string]any); out["Description"] != "updated" {
		t.Fatal(out)
	}

	if out := call("GetDatabases", map[string]any{"MaxResults": 1})["DatabaseList"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	table := map[string]any{
		"Name":      "events",
		"TableType": "EXTERNAL_TABLE",
		"StorageDescriptor": map[string]any{
			"Columns":  []any{map[string]any{"Name": "id", "Type": "string"}},
			"Location": "s3://data/",
		},
		"PartitionKeys": []any{map[string]any{"Name": "day", "Type": "string"}},
	}

	call("CreateTable", map[string]any{"DatabaseName": "console-db", "TableInput": table})
	table["Description"] = "updated table"
	call("UpdateTable", map[string]any{"DatabaseName": "console-db", "TableInput": table})
	tableKey := map[string]any{"DatabaseName": "console-db", "Name": "events"}
	if out := call("GetTable", tableKey)["Table"].(map[string]any); out["Description"] != "updated table" {
		t.Fatal(out)
	}

	call("GetTables", map[string]any{"DatabaseName": "console-db"})
	partition := map[string]any{
		"DatabaseName":   "console-db",
		"TableName":      "events",
		"PartitionInput": map[string]any{"Values": []string{"2026"}},
	}

	call("CreatePartition", partition)
	partitionKey := map[string]any{
		"DatabaseName":    "console-db",
		"TableName":       "events",
		"PartitionValues": []string{"2026"},
	}

	call("GetPartition", partitionKey)
	partitions := call("GetPartitions", map[string]any{"DatabaseName": "console-db", "TableName": "events"})
	if out := partitions["Partitions"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	arn := "arn:aws:glue:" + region + ":" + account + ":database/console-db"
	call("TagResource", map[string]any{"ResourceArn": arn, "TagsToAdd": map[string]string{"env": "dev"}})
	if out := call("GetTags", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any); out["env"] != "dev" {
		t.Fatal(out)
	}

	call("UntagResource", map[string]any{"ResourceArn": arn, "TagsToRemove": []string{"env"}})
	if out := call("GetTags", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any); len(out) != 0 {
		t.Fatal(out)
	}

	call(
		"CreateWorkflow",
		map[string]any{"Name": "console-workflow", "DefaultRunProperties": map[string]string{"env": "dev"}},
	)
	call("UpdateWorkflow", map[string]any{"Name": "console-workflow", "Description": "updated"})
	call("GetWorkflow", map[string]any{"Name": "console-workflow"})
	call("ListWorkflows", map[string]any{})
	for _, op := range []string{"GetCrawlers", "GetJobs", "GetTriggers"} {
		call(op, map[string]any{})
	}

	consoleResponse(t, srv, consoleLambdaRequest(t, srv, "iam", "CreateRole", account, region, map[string]any{
		"RoleName": "console-glue-role",
		"AssumeRolePolicyDocument": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Principal":{"Service":"glue.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
	}), http.StatusOK)
	consoleResponse(t, srv, consoleLambdaRequest(t, srv, "s3", "CreateBucket", account, region, map[string]any{
		"Bucket":                    "console-glue-data",
		"CreateBucketConfiguration": map[string]any{"LocationConstraint": region},
	}), http.StatusOK)
	job := map[string]any{"Name": "console-job", "Role": "console-glue-role", "Command": map[string]any{
		"Name": "pythonshell", "ScriptLocation": "s3://console-glue-data/missing.py",
	}}
	call("CreateJob", job)
	call("UpdateJob", map[string]any{"JobName": "console-job", "JobUpdate": map[string]any{
		"Role": job["Role"], "Command": job["Command"], "Description": "updated",
	}})
	call("GetJob", map[string]any{"JobName": "console-job"})
	call("StartJobRun", map[string]any{"JobName": "console-job"})
	if out := call("GetJobRuns", map[string]any{"JobName": "console-job"})["JobRuns"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	call(
		"CreateTrigger",
		map[string]any{
			"Name":    "console-trigger",
			"Type":    "ON_DEMAND",
			"Actions": []any{map[string]any{"JobName": "console-job"}},
		},
	)
	call(
		"UpdateTrigger",
		map[string]any{
			"Name": "console-trigger",
			"TriggerUpdate": map[string]any{
				"Description": "updated",
				"Actions":     []any{map[string]any{"JobName": "console-job"}},
			},
		},
	)
	call("GetTrigger", map[string]any{"Name": "console-trigger"})
	call("DeleteTrigger", map[string]any{"Name": "console-trigger"})
	call("DeleteJob", map[string]any{"JobName": "console-job"})
	crawler := map[string]any{
		"Name":         "console-crawler",
		"Role":         "console-glue-role",
		"DatabaseName": "console-db",
		"Targets": map[string]any{
			"S3Targets": []any{map[string]any{"Path": "s3://console-glue-data/"}},
		},
	}

	call("CreateCrawler", crawler)
	crawler["Description"] = "updated"
	call("UpdateCrawler", crawler)
	call("GetCrawler", map[string]any{"Name": "console-crawler"})
	call("DeleteCrawler", map[string]any{"Name": "console-crawler"})

	call("DeleteWorkflow", map[string]any{"Name": "console-workflow"})
	call("DeletePartition", partitionKey)
	call("DeleteTable", tableKey)
	call("DeleteDatabase", name)
	callStatus("GetDatabase", name, http.StatusBadRequest)
	callStatus("CreateConnection", map[string]any{}, http.StatusBadRequest)
}

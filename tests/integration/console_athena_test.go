package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleAthenaLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}

	athena := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return call("athena", operation, input)
	}

	call("s3", "CreateBucket", map[string]any{
		"Bucket":                    "console-athena-results",
		"CreateBucketConfiguration": map[string]any{"LocationConstraint": region},
	})
	athena("CreateWorkGroup", map[string]any{
		"Name": "console-queries",
		"Configuration": map[string]any{"ResultConfiguration": map[string]any{
			"OutputLocation": "s3://console-athena-results/queries/",
		}},
	})
	athena("UpdateWorkGroup", map[string]any{"WorkGroup": "console-queries", "Description": "Console queries"})
	const arn = "arn:aws:athena:eu-west-1:000000000000:workgroup/console-queries"
	athena("TagResource", map[string]any{
		"ResourceARN": arn, "Tags": []map[string]any{{"Key": "team", "Value": "console"}},
	})
	if tags := athena("ListTagsForResource", map[string]any{"ResourceARN": arn})["Tags"].([]any); len(tags) != 1 {
		t.Fatal(tags)
	}

	id := athena("StartQueryExecution", map[string]any{
		"QueryString": "SELECT 42 AS answer", "WorkGroup": "console-queries",
	})["QueryExecutionId"].(string)
	execution := athena("GetQueryExecution", map[string]any{"QueryExecutionId": id})["QueryExecution"].(map[string]any)
	status := execution["Status"].(map[string]any)
	if status["State"] != "SUCCEEDED" {
		t.Fatal(execution)
	}

	submitted, ok := status["SubmissionDateTime"].(float64)
	if !ok || submitted < 1e9 || submitted > 1e11 {
		t.Fatalf("expected epoch seconds in console JSON, got %v", status)
	}

	rows := athena("GetQueryResults", map[string]any{"QueryExecutionId": id, "MaxResults": 1})
	if rows["NextToken"] == nil {
		t.Fatal(rows)
	}

	next := athena("GetQueryResults", map[string]any{"QueryExecutionId": id, "NextToken": rows["NextToken"]})
	if len(next["ResultSet"].(map[string]any)["Rows"].([]any)) != 1 {
		t.Fatal(next)
	}

	athena("StopQueryExecution", map[string]any{"QueryExecutionId": id})
	athena("ListQueryExecutions", map[string]any{"WorkGroup": "console-queries"})
	athena("BatchGetQueryExecution", map[string]any{"QueryExecutionIds": []string{id}})
	saved := athena("CreateNamedQuery", map[string]any{
		"Name": "answer", "Database": "default", "QueryString": "SELECT 42", "WorkGroup": "console-queries",
	})["NamedQueryId"].(string)
	athena("ListNamedQueries", map[string]any{"WorkGroup": "console-queries"})
	athena("BatchGetNamedQuery", map[string]any{"NamedQueryIds": []string{saved}})
	athena("DeleteNamedQuery", map[string]any{"NamedQueryId": saved})
	athena("ListDataCatalogs", map[string]any{})
	athena("GetDataCatalog", map[string]any{"Name": "AwsDataCatalog"})
	athena("ListDatabases", map[string]any{"CatalogName": "AwsDataCatalog"})
	athena("UntagResource", map[string]any{"ResourceARN": arn, "TagKeys": []string{"team"}})
	athena("DeleteWorkGroup", map[string]any{"WorkGroup": "console-queries"})
}

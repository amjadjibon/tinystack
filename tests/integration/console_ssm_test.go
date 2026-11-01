package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleSSMParameterLifecycle(t *testing.T) {
	srv := server(t)
	const account, region, name = "000000000000", "eu-west-1", "/console/db/password"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "ssm", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	call("PutParameter", map[string]any{
		"Name": name, "Value": "private-original", "Type": "SecureString",
		"Tier": "Standard", "Description": "database", "DataType": "text",
	})
	metadata := call("DescribeParameters", map[string]any{"ParameterFilters": []any{
		map[string]any{"Key": "Path", "Option": "Recursive", "Values": []string{"/console"}},
		map[string]any{"Key": "Type", "Values": []string{"SecureString"}},
	}})["Parameters"].([]any)
	if len(metadata) != 1 || metadata[0].(map[string]any)["Value"] != nil {
		t.Fatal("metadata list differs or exposed a value")
	}
	if call("GetParameter", map[string]any{"Name": name})["Parameter"].(map[string]any)["Value"] == "private-original" {
		t.Fatal("SecureString read exposed plaintext without decryption")
	}
	decrypted := call("GetParameter", map[string]any{"Name": name, "WithDecryption": true})
	if decrypted["Parameter"].(map[string]any)["Value"] != "private-original" {
		t.Fatal("decryption failed")
	}
	call("PutParameter", map[string]any{"Name": name, "Value": "private-new", "Overwrite": true})
	call("LabelParameterVersion", map[string]any{"Name": name, "ParameterVersion": 1, "Labels": []string{"stable"}})
	history := call("GetParameterHistory", map[string]any{"Name": name, "WithDecryption": false})["Parameters"].([]any)
	if len(history) != 2 || history[0].(map[string]any)["Labels"].([]any)[0] != "stable" {
		t.Fatal("history or version label missing")
	}
	historical := call("GetParameter", map[string]any{"Name": name + ":1", "WithDecryption": true})
	if historical["Parameter"].(map[string]any)["Value"] != "private-original" {
		t.Fatal("historical value differs")
	}
	call("UnlabelParameterVersion", map[string]any{"Name": name, "ParameterVersion": 1, "Labels": []string{"stable"}})
	tagInput := map[string]any{"ResourceType": "Parameter", "ResourceId": name}
	call("AddTagsToResource", map[string]any{
		"ResourceType": "Parameter", "ResourceId": name,
		"Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})
	if len(call("ListTagsForResource", tagInput)["TagList"].([]any)) != 1 {
		t.Fatal("tag missing")
	}
	call(
		"RemoveTagsFromResource",
		map[string]any{"ResourceType": "Parameter", "ResourceId": name, "TagKeys": []string{"team"}},
	)
	for _, other := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(t, srv, consoleLambdaRequest(t, srv, "ssm", "GetParameter", other.account, other.region,
			map[string]any{"Name": name, "WithDecryption": true}), http.StatusNotFound)
	}
	// Document and command operations are outside the console's Parameter Store scope.
	consoleResponse(t, srv, consoleLambdaRequest(t, srv, "ssm", "ListDocuments", account, region,
		map[string]any{}), http.StatusBadRequest)
	call("DeleteParameter", map[string]any{"Name": name})
	call("PutParameter", map[string]any{"Name": "/console/a", "Value": "a", "Type": "String"})
	call("PutParameter", map[string]any{"Name": "/console/b", "Value": "a,b", "Type": "StringList"})
	deleted := call("DeleteParameters", map[string]any{"Names": []string{"/console/a", "/console/b"}})
	if len(deleted["DeletedParameters"].([]any)) != 2 {
		t.Fatal("bulk deletion failed")
	}
}

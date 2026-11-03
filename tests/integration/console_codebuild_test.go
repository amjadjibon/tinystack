package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleCodeBuildLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(op string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "codebuild", op, account, region, input)

		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}

	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK ||
		!consolePage(caps, "codebuild") ||
		caps["codebuild_runtime"] != "records" {
		t.Fatal(caps)
	}

	input := map[string]any{
		"Name":        "console-build",
		"ServiceRole": "arn:aws:iam::" + account + ":role/build",
		"Source": map[string]any{
			"Type":      "NO_SOURCE",
			"Buildspec": "version: 0.2\nphases:\n  build:\n    commands: [echo hello]\n",
		},
		"Artifacts": map[string]any{"Type": "NO_ARTIFACTS"},
		"Environment": map[string]any{
			"Type":        "LINUX_CONTAINER",
			"ComputeType": "BUILD_GENERAL1_SMALL",
			"Image":       "alpine:3.21",
			"EnvironmentVariables": []any{
				map[string]any{"Name": "TOKEN", "Value": "my-secret", "Type": "SECRETS_MANAGER"},
			},
		},
		"Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	}

	call("CreateProject", input)
	if len(call("ListProjects", map[string]any{})["Projects"].([]any)) != 1 {
		t.Fatal("missing project")
	}

	readProject := func() map[string]any {
		t.Helper()
		out := call("BatchGetProjects", map[string]any{"Names": []string{"console-build"}})

		return out["Projects"].([]any)[0].(map[string]any)
	}

	p := readProject()
	env := p["Environment"].(map[string]any)["EnvironmentVariables"].([]any)[0].(map[string]any)
	if env["Value"] != "my-secret" || env["Type"] != "SECRETS_MANAGER" {
		t.Fatal(env)
	}

	call(
		"UpdateProject",
		map[string]any{"Name": "console-build", "Description": "updated", "TimeoutInMinutes": 10, "Tags": []any{}},
	)
	p = readProject()
	if p["Description"] != "updated" || p["TimeoutInMinutes"] != float64(10) {
		t.Fatal(p)
	}

	for _, scope := range []struct{ account, region string }{{account, "us-west-2"}, {"111111111111", region}} {
		req := consoleLambdaRequest(t, srv, "codebuild", "ListProjects", scope.account, scope.region, map[string]any{})
		out := consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
		if len(out["Projects"].([]any)) != 0 {
			t.Fatal(out)
		}
	}

	for _, op := range []string{"ListBuilds", "ListBuildsForProject"} {
		in := map[string]any{}
		if op == "ListBuildsForProject" {
			in["ProjectName"] = "console-build"
		}
		call(op, in)
	}

	rejected := consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"codebuild",
			"StartBuild",
			account,
			region,
			map[string]any{"ProjectName": "console-build"},
		),
		http.StatusBadRequest,
	)
	if rejected["code"] != "InvalidInputException" {
		t.Fatal(rejected)
	}

	call("BatchGetBuilds", map[string]any{"Ids": []string{"missing"}})
	call("BatchDeleteBuilds", map[string]any{"Ids": []string{"missing"}})
	denied := consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "codebuild", "RetryBuild", account, region, map[string]any{}),
		http.StatusBadRequest,
	)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}

	call("DeleteProject", map[string]any{"Name": "console-build"})
	if len(call("ListProjects", map[string]any{})["Projects"].([]any)) != 0 {
		t.Fatal("project retained")
	}
}

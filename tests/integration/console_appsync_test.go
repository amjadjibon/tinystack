package integration_test

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"
)

func TestConsoleAppSyncLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, "appsync", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["appsync"] != true {
		t.Fatal(capabilities)
	}

	api := call("CreateGraphqlApi", map[string]any{
		"Name": "console-api", "AuthenticationType": "API_KEY",
	})["GraphqlApi"].(map[string]any)
	id := api["ApiId"]
	if api["Uris"].(map[string]any)["GRAPHQL"] == "" {
		t.Fatal(api)
	}

	call("UpdateGraphqlApi", map[string]any{
		"ApiId":                             id,
		"Name":                              "updated",
		"AuthenticationType":                "API_KEY",
		"XrayEnabled":                       true,
		"AdditionalAuthenticationProviders": []any{map[string]any{"AuthenticationType": "AWS_IAM"}},
		"LogConfig": map[string]any{
			"FieldLogLevel":         "ERROR",
			"CloudWatchLogsRoleArn": "arn:aws:iam::000000000000:role/logs",
		},
	})
	updated := call("GetGraphqlApi", map[string]any{"ApiId": id})["GraphqlApi"].(map[string]any)
	if out := updated; out["Name"] != "updated" ||
		out["XrayEnabled"] != true {
		t.Fatal(out)
	}

	call("CreateGraphqlApi", map[string]any{"Name": "second", "AuthenticationType": "AWS_IAM"})
	first := call("ListGraphqlApis", map[string]any{"MaxResults": 1})
	if len(first["GraphqlApis"].([]any)) != 1 || first["NextToken"] == nil {
		t.Fatal(first)
	}

	if out := call(
		"ListGraphqlApis",
		map[string]any{"MaxResults": 1, "NextToken": first["NextToken"]},
	); len(
		out["GraphqlApis"].([]any),
	) != 1 {
		t.Fatal(out)
	}

	call(
		"TagResource",
		map[string]any{"ResourceArn": api["Arn"], "Tags": map[string]any{"team": "dev", "remove": "me"}},
	)
	call("UntagResource", map[string]any{"ResourceArn": api["Arn"], "TagKeys": []any{"remove"}})
	tagged := call("GetGraphqlApi", map[string]any{"ApiId": id})["GraphqlApi"].(map[string]any)["Tags"].(map[string]any)
	if tagged["team"] != "dev" || tagged["remove"] != nil {
		t.Fatal(tagged)
	}

	definition := base64.StdEncoding.EncodeToString([]byte("type Query { hello: String } schema { query: Query }"))
	call("StartSchemaCreation", map[string]any{"ApiId": id, "Definition": definition})
	if out := call("GetSchemaCreationStatus", map[string]any{"ApiId": id}); out["Status"] != "SUCCESS" {
		t.Fatal(out)
	}

	if out := call(
		"GetIntrospectionSchema",
		map[string]any{"ApiId": id, "Format": "SDL", "IncludeDirectives": true},
	); out["Schema"] == nil {
		t.Fatal(out)
	}

	if out := call("ListTypes", map[string]any{"ApiId": id, "Format": "SDL"}); len(out["Types"].([]any)) == 0 {
		t.Fatal(out)
	}

	source := map[string]any{"ApiId": id, "Name": "local", "Type": "NONE"}
	call("CreateDataSource", source)
	source["Description"] = "changed"
	call("UpdateDataSource", source)
	if out := call("ListDataSources", map[string]any{"ApiId": id})["DataSources"].([]any); len(out) != 1 ||
		out[0].(map[string]any)["Description"] != "changed" {
		t.Fatal(out)
	}

	fn := map[string]any{
		"ApiId":                   id,
		"Name":                    "step",
		"DataSourceName":          "local",
		"FunctionVersion":         "2018-05-29",
		"RequestMappingTemplate":  `{"version":"2018-05-29","payload":"hello"}`,
		"ResponseMappingTemplate": "$util.toJson($ctx.result)",
	}

	function := call("CreateFunction", fn)["FunctionConfiguration"].(map[string]any)
	fn["FunctionId"] = function["FunctionId"]
	fn["Description"] = "updated"
	call("UpdateFunction", fn)
	if out := call("ListFunctions", map[string]any{"ApiId": id})["Functions"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	res := map[string]any{
		"ApiId":                   id,
		"TypeName":                "Query",
		"FieldName":               "hello",
		"Kind":                    "UNIT",
		"DataSourceName":          "local",
		"RequestMappingTemplate":  `{"version":"2018-05-29","payload":"hello"}`,
		"ResponseMappingTemplate": "$util.toJson($ctx.result)",
	}

	call("CreateResolver", res)
	delete(res, "DataSourceName")
	res["Kind"] = "PIPELINE"
	res["PipelineConfig"] = map[string]any{"Functions": []any{function["FunctionId"]}}
	res["RequestMappingTemplate"] = "{}"
	res["ResponseMappingTemplate"] = "$util.toJson($ctx.prev.result)"
	call("UpdateResolver", res)
	if out := call("ListResolvers", map[string]any{"ApiId": id, "TypeName": "Query"})["Resolvers"].([]any); len(
		out,
	) != 1 ||
		out[0].(map[string]any)["Kind"] != "PIPELINE" {
		t.Fatal(out)
	}

	key := call("CreateApiKey", map[string]any{
		"ApiId": id, "Description": "console", "Expires": time.Now().Add(7 * 24 * time.Hour).Unix(),
	})["ApiKey"].(map[string]any)
	if out := call("ListApiKeys", map[string]any{"ApiId": id})["ApiKeys"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	// Switch a pipeline to the supported JavaScript runtime and preserve its ordered functions.
	delete(res, "RequestMappingTemplate")
	delete(res, "ResponseMappingTemplate")
	res["Runtime"] = map[string]any{"Name": "APPSYNC_JS", "RuntimeVersion": "1.0.0"}
	res["Code"] = "export function request(ctx) { return {}; } export function response(ctx) { return ctx.prev.result; }"
	call("UpdateResolver", res)
	js := call("ListResolvers", map[string]any{"ApiId": id, "TypeName": "Query"})["Resolvers"].([]any)[0].(map[string]any)
	if js["Runtime"].(map[string]any)["Name"] != "APPSYNC_JS" || js["Code"] != res["Code"] {
		t.Fatal(js)
	}

	// Failed schema creation is reported without replacing the active SDL.
	call(
		"StartSchemaCreation",
		map[string]any{"ApiId": id, "Definition": base64.StdEncoding.EncodeToString([]byte("invalid schema"))},
	)
	if out := call("GetSchemaCreationStatus", map[string]any{"ApiId": id}); out["Status"] != "FAILED" {
		t.Fatal(out)
	}

	if out := call("GetIntrospectionSchema", map[string]any{"ApiId": id, "Format": "SDL"}); out["Schema"] == nil {
		t.Fatal(out)
	}

	// Configuration members used by each other supported data source round-trip.
	for _, spec := range []struct {
		value              map[string]any
		name, kind, config string
	}{
		{
			map[string]any{"LambdaFunctionArn": "arn:aws:lambda:eu-west-1:000000000000:function:resolver"},
			"lambda", "AWS_LAMBDA", "LambdaConfig",
		},
		{
			map[string]any{"TableName": "items", "AwsRegion": region, "UseCallerCredentials": true},
			"dynamo", "AMAZON_DYNAMODB", "DynamodbConfig",
		},
		{map[string]any{"Endpoint": "http://127.0.0.1:4700"}, "http", "HTTP", "HttpConfig"},
		{
			map[string]any{"EventBusArn": "arn:aws:events:eu-west-1:000000000000:event-bus/default"},
			"events", "AMAZON_EVENTBRIDGE", "EventBridgeConfig",
		},
	} {
		input := map[string]any{"ApiId": id, "Name": spec.name, "Type": spec.kind, spec.config: spec.value}
		created := call("CreateDataSource", input)["DataSource"].(map[string]any)
		if created["Type"] != spec.kind || created[spec.config] == nil {
			t.Fatal(created)
		}
		call("DeleteDataSource", map[string]any{"ApiId": id, "Name": spec.name})
	}

	call("DeleteApiKey", map[string]any{"ApiId": id, "Id": key["Id"]})
	call("DeleteResolver", map[string]any{"ApiId": id, "TypeName": "Query", "FieldName": "hello"})
	call("DeleteFunction", map[string]any{"ApiId": id, "FunctionId": function["FunctionId"]})
	call("DeleteDataSource", map[string]any{"ApiId": id, "Name": "local"})
	call("DeleteGraphqlApi", map[string]any{"ApiId": id})
	denied := consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "appsync", "CreateApi", account, region, map[string]any{}),
		http.StatusBadRequest,
	)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}
}

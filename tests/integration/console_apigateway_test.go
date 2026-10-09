package integration_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	httpapi "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
)

func TestConsoleHTTPAPIDeploymentsAndInvocation(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "console")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, r.URL.Path+"?"+r.URL.RawQuery+"|"+r.Header.Get("X-Test"))
	}))
	defer backend.Close()
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "apigatewayv2", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	created := write(
		"CreateApi",
		map[string]any{"Name": "console-http", "ProtocolType": "HTTP", "Tags": map[string]string{"team": "console"}},
	)
	id := created["ApiId"].(string)
	scoped := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		input["ApiId"] = id
		return write(operation, input)
	}
	integration := scoped("CreateIntegration", map[string]any{
		"IntegrationType":      "HTTP_PROXY",
		"IntegrationUri":       backend.URL + "/old",
		"IntegrationMethod":    "GET",
		"PayloadFormatVersion": "1.0",
	})["IntegrationId"].(string)
	route := scoped("CreateRoute", map[string]any{
		"RouteKey":            "GET /pets",
		"Target":              "integrations/" + integration,
		"AuthorizationType":   "NONE",
		"AuthorizerId":        "",
		"AuthorizationScopes": []string{},
	})["RouteId"].(string)
	scoped("CreateStage", map[string]any{"StageName": "$default", "AutoDeploy": true})
	deployed := scoped("CreateDeployment", map[string]any{"Description": "first snapshot"})["DeploymentId"].(string)
	scoped("CreateStage", map[string]any{"StageName": "dev", "DeploymentId": deployed, "AutoDeploy": false})
	invoke := func(stage string) map[string]any {
		t.Helper()
		return scoped(
			"InvokeApi",
			map[string]any{
				"Stage":   stage,
				"Method":  "GET",
				"Path":    "/pets?q=hello%20world",
				"Headers": map[string]string{"X-Test": "yes"},
			},
		)
	}
	assertBody := func(stage, path string) {
		t.Helper()
		out := invoke(stage)
		if out["StatusCode"] != float64(http.StatusCreated) || out["Body"] != path+"?q=hello%20world|yes" {
			t.Fatalf("%s: %+v", stage, out)
		}
		if out["Headers"].(map[string]any)["X-Backend"].([]any)[0] != "console" {
			t.Fatal(out)
		}
	}
	assertBody("$default", "/old")
	assertBody("dev", "/old")
	scoped(
		"UpdateIntegration",
		map[string]any{"IntegrationId": integration, "IntegrationUri": backend.URL + "/new", "Description": "updated"},
	)
	assertBody("$default", "/new")
	assertBody("dev", "/old")
	scoped("CreateDeployment", map[string]any{"StageName": "dev", "Description": "publish changes"})
	assertBody("dev", "/new")
	// The default stage must not silently select a named stage from the path.
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"apigatewayv2",
			"InvokeApi",
			account,
			region,
			map[string]any{"ApiId": id, "Stage": "$default", "Method": "GET", "Path": "/dev/pets"},
		),
		http.StatusBadRequest,
	)
	authorizer := scoped("CreateAuthorizer", map[string]any{
		"Name":           "jwt",
		"AuthorizerType": "JWT",
		"IdentitySource": []string{"$request.header.Authorization"},
		"JwtConfiguration": map[string]any{
			"Issuer":   "https://issuer.example.com",
			"Audience": []string{"console"},
		},
	})["AuthorizerId"].(string)
	scoped("UpdateAuthorizer", map[string]any{"AuthorizerId": authorizer, "Name": "jwt-edited"})
	scoped(
		"UpdateRoute",
		map[string]any{
			"RouteId":             route,
			"AuthorizationType":   "JWT",
			"AuthorizerId":        authorizer,
			"AuthorizationScopes": []string{"read"},
		},
	)
	if invoke("$default")["StatusCode"] != float64(http.StatusUnauthorized) {
		t.Fatal("missing JWT was accepted")
	}
	scoped(
		"UpdateRoute",
		map[string]any{
			"RouteId":             route,
			"AuthorizationType":   "AWS_IAM",
			"AuthorizerId":        "",
			"AuthorizationScopes": []string{},
		},
	)
	if invoke("$default")["StatusCode"] != float64(http.StatusForbidden) {
		t.Fatal("test request impersonated an IAM caller")
	}
	scoped(
		"UpdateRoute",
		map[string]any{
			"RouteId":             route,
			"AuthorizationType":   "NONE",
			"AuthorizerId":        "",
			"AuthorizationScopes": []string{},
		},
	)
	scoped("DeleteAuthorizer", map[string]any{"AuthorizerId": authorizer})
	arn := "arn:aws:apigateway:" + region + "::/apis/" + id
	write("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]string{"owner": "dev"}})
	write("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})
	write("TagResource", map[string]any{"ResourceArn": arn + "/stages/dev", "Tags": map[string]string{"env": "dev"}})
	options := apigwClient(srv).Options()
	options.Region = region
	sdk := httpapi.New(options)
	read, err := sdk.GetApi(context.Background(), &httpapi.GetApiInput{ApiId: new(id)})
	if err != nil || read.Tags["owner"] != "dev" || len(read.Tags) != 1 {
		t.Fatalf("SDK API: %+v %v", read, err)
	}
	stages, err := sdk.GetStages(context.Background(), &httpapi.GetStagesInput{ApiId: new(id)})
	if err != nil || len(stages.Items) != 2 {
		t.Fatalf("SDK stages: %+v %v", stages, err)
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(
				t,
				srv,
				"apigatewayv2",
				"InvokeApi",
				scope.account,
				scope.region,
				map[string]any{"ApiId": id, "Stage": "dev", "Method": "GET", "Path": "/pets"},
			),
			http.StatusNotFound,
		)
	}
	scoped("DeleteStage", map[string]any{"StageName": "dev"})
	scoped("DeleteDeployment", map[string]any{"DeploymentId": deployed})
	scoped("DeleteRoute", map[string]any{"RouteId": route})
	scoped("DeleteIntegration", map[string]any{"IntegrationId": integration})
	scoped("DeleteApi", map[string]any{})
	_, err = sdk.GetApi(context.Background(), &httpapi.GetApiInput{ApiId: new(id)})
	code(t, err, "NotFoundException")
}

func TestConsoleRESTAPIResourcesAndPreservedConfiguration(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+"|"+string(data))
	}))
	defer backend.Close()
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "apigateway", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	created := write(
		"CreateRestApi",
		map[string]any{
			"Name":                  "console-rest",
			"EndpointConfiguration": map[string]any{"Types": []string{"REGIONAL"}},
			"Tags":                  map[string]string{"team": "console"},
		},
	)
	id := created["Id"].(string)
	scoped := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		input["RestApiId"] = id
		return write(operation, input)
	}
	resources := scoped("GetResources", map[string]any{"Embed": []string{"methods"}})["Items"].([]any)
	root := resources[0].(map[string]any)["Id"].(string)
	resource := scoped("CreateResource", map[string]any{"ParentId": root, "PathPart": "pets"})["Id"].(string)
	method := func(operation string, input map[string]any) {
		t.Helper()
		input["ResourceId"] = resource
		input["HttpMethod"] = "POST"
		scoped(operation, input)
	}
	method("PutMethod", map[string]any{"AuthorizationType": "NONE", "ApiKeyRequired": false})
	method(
		"PutIntegration",
		map[string]any{
			"Type":                  "HTTP_PROXY",
			"IntegrationHttpMethod": "POST",
			"Uri":                   backend.URL + "/old",
			"TimeoutInMillis":       29000,
		},
	)
	options := restClient(srv).Options()
	options.Region = region
	sdk := restapi.New(options)
	ctx := context.Background()
	// A settings edit must retain fields that are not exposed in the form.
	_, err := sdk.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
		RestApiId: new(id), ResourceId: new(resource), HttpMethod: new("POST"), StatusCode: new("202"),
		ResponseParameters: map[string]bool{"method.response.header.X-Keep": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sdk.PutIntegrationResponse(
		ctx,
		&restapi.PutIntegrationResponseInput{
			RestApiId:          new(id),
			ResourceId:         new(resource),
			HttpMethod:         new("POST"),
			StatusCode:         new("202"),
			ResponseParameters: map[string]string{"method.response.header.X-Keep": "'yes'"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	method(
		"UpdateMethod",
		map[string]any{
			"PatchOperations": []any{map[string]any{"Op": "replace", "Path": "/operationName", "Value": "createPet"}},
		},
	)
	deployment := scoped("CreateDeployment", map[string]any{"StageName": "dev"})["Id"].(string)
	invoke := func() map[string]any {
		t.Helper()
		return write(
			"InvokeApi",
			map[string]any{"ApiId": id, "Stage": "dev", "Method": "POST", "Path": "/pets", "Body": "hello"},
		)
	}
	if out := invoke(); out["StatusCode"] != float64(http.StatusAccepted) || out["Body"] != "POST /old|hello" {
		t.Fatal(out)
	}
	method(
		"UpdateMethod",
		map[string]any{
			"PatchOperations": []any{
				map[string]any{"Op": "replace", "Path": "/authorizationType", "Value": "NONE"},
				map[string]any{"Op": "replace", "Path": "/apiKeyRequired", "Value": "false"},
			},
		},
	)
	method(
		"UpdateIntegration",
		map[string]any{
			"PatchOperations": []any{map[string]any{"Op": "replace", "Path": "/uri", "Value": backend.URL + "/new"}},
		},
	)
	got, err := sdk.GetMethod(
		ctx,
		&restapi.GetMethodInput{RestApiId: new(id), ResourceId: new(resource), HttpMethod: new("POST")},
	)
	if err != nil || aws.ToString(got.OperationName) != "createPet" ||
		got.MethodIntegration.IntegrationResponses["202"].ResponseParameters["method.response.header.X-Keep"] != "'yes'" {
		t.Fatalf("preserved settings: %+v %v", got, err)
	}
	if invoke()["Body"] != "POST /old|hello" {
		t.Fatal("REST draft change went live before deployment")
	}
	scoped("CreateDeployment", map[string]any{"StageName": "dev", "Description": "updated"})
	if invoke()["Body"] != "POST /new|hello" {
		t.Fatal("REST deployment did not publish changes")
	}
	// REST GetStages uses the SDK Item field and accepts no pagination members.
	if stages := scoped("GetStages", map[string]any{}); len(stages["Item"].([]any)) != 1 {
		t.Fatal(stages)
	}
	scoped(
		"UpdateStage",
		map[string]any{
			"StageName": "dev",
			"PatchOperations": []any{
				map[string]any{"Op": "replace", "Path": "/variables/env", "Value": "test"},
				map[string]any{"Op": "remove", "Path": "/accessLogSettings"},
			},
		},
	)
	arn := "arn:aws:apigateway:" + region + "::/restapis/" + id
	write("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]string{"owner": "dev"}})
	write("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})
	write("TagResource", map[string]any{"ResourceArn": arn + "/stages/dev", "Tags": map[string]string{"env": "dev"}})
	scoped(
		"UpdateRestApi",
		map[string]any{
			"PatchOperations": []any{map[string]any{"Op": "replace", "Path": "/description", "Value": "edited"}},
		},
	)
	authorizer := scoped("CreateAuthorizer", map[string]any{
		"Name":           "cognito",
		"Type":           "COGNITO_USER_POOLS",
		"ProviderARNs":   []string{"arn:aws:cognito-idp:eu-west-1:000000000000:userpool/eu-west-1_console"},
		"IdentitySource": "method.request.header.Authorization",
	})["Id"].(string)
	scoped(
		"UpdateAuthorizer",
		map[string]any{
			"AuthorizerId":    authorizer,
			"PatchOperations": []any{map[string]any{"Op": "replace", "Path": "/name", "Value": "cognito-edited"}},
		},
	)
	scoped("DeleteAuthorizer", map[string]any{"AuthorizerId": authorizer})
	if tags := write("GetTags", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any); len(tags) != 1 ||
		tags["owner"] != "dev" {
		t.Fatal(tags)
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(
				t,
				srv,
				"apigateway",
				"InvokeApi",
				scope.account,
				scope.region,
				map[string]any{"ApiId": id, "Stage": "dev", "Method": "POST", "Path": "/pets"},
			),
			http.StatusNotFound,
		)
	}
	// Named-stage invocation cannot skip stage lookup or target another scope.
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"apigateway",
			"InvokeApi",
			account,
			region,
			map[string]any{"ApiId": id, "Stage": "missing", "Method": "POST", "Path": "/pets"},
		),
		http.StatusNotFound,
	)
	scoped("DeleteDeployment", map[string]any{"DeploymentId": deployment})
	method("DeleteMethod", map[string]any{})
	scoped("DeleteResource", map[string]any{"ResourceId": resource})
	scoped("DeleteStage", map[string]any{"StageName": "dev"})
	scoped("DeleteRestApi", map[string]any{})
	_, err = sdk.GetRestApi(ctx, &restapi.GetRestApiInput{RestApiId: new(id)})
	code(t, err, "NotFoundException")
}

func TestConsoleAPIRequestValidationAndBinaryResponses(t *testing.T) {
	srv := server(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/binary":
			_, _ = w.Write([]byte{0xff, 0, 0xfe})
		case "/large":
			_, _ = io.WriteString(w, strings.Repeat("a", (1<<20)+1))
		}
	}))
	defer backend.Close()
	const account, region = "000000000000", "us-east-1"
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "apigatewayv2", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	id := write("CreateApi", map[string]any{"Name": "binary", "ProtocolType": "HTTP"})["ApiId"].(string)
	integration := write("CreateIntegration", map[string]any{
		"ApiId":                id,
		"IntegrationType":      "HTTP_PROXY",
		"IntegrationUri":       backend.URL + "/{proxy}",
		"IntegrationMethod":    "ANY",
		"PayloadFormatVersion": "1.0",
	})["IntegrationId"].(string)
	write(
		"CreateRoute",
		map[string]any{"ApiId": id, "RouteKey": "ANY /{proxy+}", "Target": "integrations/" + integration},
	)
	write("CreateStage", map[string]any{"ApiId": id, "StageName": "$default", "AutoDeploy": true})
	invoke := func(path string) map[string]any {
		t.Helper()
		return write("InvokeApi", map[string]any{"ApiId": id, "Stage": "$default", "Method": "GET", "Path": path})
	}
	binary := invoke("/binary")
	if binary["IsBase64Encoded"] != true || binary["Body"] != base64.StdEncoding.EncodeToString([]byte{0xff, 0, 0xfe}) {
		t.Fatal(binary)
	}
	large := invoke("/large")
	if large["BodyTruncated"] != true || len(large["Body"].(string)) != 1<<20 {
		t.Fatalf("truncated=%v length=%d", large["BodyTruncated"], len(large["Body"].(string)))
	}
	for _, bad := range []map[string]any{
		{"Unknown": true},
		{"Method": "ANY"},
		{"Path": "https://example.com"},
		{"Path": "//example.com"},
		{"Path": "/pets#fragment"},
		{"Path": "/%zz"},
		{"Path": "/?a=%zz"},
		{"Headers": map[string]string{"Bad Header": "value"}},
		{"Headers": map[string]string{"X-Test": "bad\r\nInjected: yes"}},
		{"Headers": map[string]string{"Authorization": "AWS4-HMAC-SHA256 fake"}},
		{"Path": "/?X-Amz-Signature=fake"},
	} {
		input := map[string]any{"ApiId": id, "Stage": "$default", "Method": "GET", "Path": "/binary"}
		for key, value := range bad {
			input[key] = value
		}
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(t, srv, "apigatewayv2", "InvokeApi", account, region, input),
			http.StatusBadRequest,
		)
	}
	// Console actions retain their existing 2 MiB JSON envelope limit.
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(
			t,
			srv,
			"apigatewayv2",
			"InvokeApi",
			account,
			region,
			map[string]any{
				"ApiId":  id,
				"Stage":  "$default",
				"Method": "POST",
				"Path":   "/binary",
				"Body":   strings.Repeat("x", 2<<20),
			},
		),
		http.StatusBadRequest,
	)
}

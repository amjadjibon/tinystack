package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	rest "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	httpapi "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
)

func TestAPIGatewayOpenAPI(t *testing.T) {
	s := server(t)
	c := restClient(s)
	ctx := context.Background()
	fn := createFunction(t, lambdaClient(s, "test"), "openapi", "app.respond")
	uri := *lambdaURI(fn.FunctionArn)
	for _, version := range []string{`"swagger":"2.0"`, `"openapi":"3.0.1"`} {
		t.Run(version, func(t *testing.T) {
			subCtx := context.Background()
			spec := fmt.Sprintf(`{%s,"info":{"title":"imported","version":"1.0"},"paths":{"/pets":{"get":{
    "responses":{"200":{"description":"ok"}},"x-amazon-apigateway-integration":{
    "type":"aws_proxy","httpMethod":"POST","uri":%q}}}}}`, version, uri)
			a, err := c.ImportRestApi(subCtx, &rest.ImportRestApiInput{Body: []byte(spec)})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.CreateDeployment(subCtx, &rest.CreateDeploymentInput{RestApiId: a.Id, StageName: new("prod")})
			if err != nil {
				t.Fatal(err)
			}

			status, _, body := call(t, s, "GET", *a.Id, "/prod/pets", nil, "")
			if status != 201 {
				t.Fatalf("invoke: %d %s", status, body)
			}

			for _, accept := range []string{"application/json", "application/yaml"} {
				exported, exportErr := c.GetExport(subCtx, &rest.GetExportInput{
					RestApiId: a.Id, StageName: new("prod"), ExportType: new("oas30"),
					Accepts: new(accept), Parameters: map[string]string{"extensions": "apigateway"},
				})
				if exportErr != nil {
					t.Fatal(exportErr)
				}

				copyAPI, importErr := c.ImportRestApi(subCtx, &rest.ImportRestApiInput{Body: exported.Body})
				if importErr != nil {
					t.Fatal(importErr)
				}

				resources, getErr := c.GetResources(
					subCtx,
					&rest.GetResourcesInput{RestApiId: copyAPI.Id, Embed: []string{"methods"}},
				)
				if getErr != nil || len(resources.Items) != 2 {
					t.Fatalf("roundtrip: %+v %v", resources, getErr)
				}
			}

			changed := strings.ReplaceAll(spec, "/pets", "/dogs")
			for _, mode := range []resttypes.PutMode{resttypes.PutModeMerge, resttypes.PutModeOverwrite} {
				_, err = c.PutRestApi(subCtx, &rest.PutRestApiInput{RestApiId: a.Id, Body: []byte(changed), Mode: mode})
				if err != nil {
					t.Fatal(err)
				}

				resources, getErr := c.GetResources(subCtx, &rest.GetResourcesInput{RestApiId: a.Id})
				want := 3
				if mode == resttypes.PutModeOverwrite {
					want = 2
				}

				if getErr != nil || len(resources.Items) != want {
					t.Fatalf("%s: %+v %v", mode, resources, getErr)
				}
			}
		})
	}

	for _, spec := range []string{
		`broken`, `{"openapi":"3.0.1","info":{"title":"bad"},"paths":{"/x":{"trace":{}}}}`,
		`{"openapi":"3.0.1","info":{"title":"bad"},"paths":{},"components":{"schemas":{"Pet":{}}}}`,
	} {
		if _, err := c.ImportRestApi(ctx, &rest.ImportRestApiInput{Body: []byte(spec)}); err == nil {
			t.Fatal("accepted invalid spec")
		}
	}
}

func TestHTTPAPIOpenAPI(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := context.Background()
	spec := `{"openapi":"3.0.1","info":{"title":"http-import","version":"1.0"},
 "x-amazon-apigateway-cors":{"allowOrigins":["https://example.com"],"allowMethods":["GET"]},
 "components":{"securitySchemes":{"jwt":{"type":"oauth2","x-amazon-apigateway-authorizer":{
 "type":"jwt","identitySource":["$request.header.Authorization"],
 "jwtConfiguration":{"issuer":"https://issuer.example.com","audience":["test"]}}}}},
 "paths":{"/pets":{"get":{"security":[{"jwt":[]}],"responses":{"200":{"description":"OK"}},
 "x-amazon-apigateway-integration":{"type":"http_proxy","httpMethod":"GET","uri":"https://example.com",
 "payloadFormatVersion":"1.0"}}}}}`
	var input map[string]any
	if err := json.Unmarshal([]byte(spec), &input); err != nil {
		t.Fatal(err)
	}

	components := input["components"].(map[string]any)
	operation := input["paths"].(map[string]any)["/pets"].(map[string]any)["get"].(map[string]any)
	components["x-amazon-apigateway-integrations"] = map[string]any{
		"backend": operation["x-amazon-apigateway-integration"],
	}

	operation["x-amazon-apigateway-integration"] = map[string]any{
		"$ref": "#/components/x-amazon-apigateway-integrations/backend",
	}

	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	spec = string(encoded)
	a, err := c.ImportApi(ctx, &httpapi.ImportApiInput{Body: new(spec)})
	if err != nil {
		t.Fatal(err)
	}

	if a.CorsConfiguration == nil || len(a.CorsConfiguration.AllowOrigins) != 1 {
		t.Fatalf("CORS: %+v", a)
	}

	authors, err := c.GetAuthorizers(ctx, &httpapi.GetAuthorizersInput{ApiId: a.ApiId})
	if err != nil || len(authors.Items) != 1 {
		t.Fatalf("authorizers: %+v %v", authors, err)
	}

	routes, err := c.GetRoutes(ctx, &httpapi.GetRoutesInput{ApiId: a.ApiId})
	if err != nil || len(routes.Items) != 1 || string(routes.Items[0].AuthorizationType) != "JWT" {
		t.Fatalf("routes: %+v %v", routes, err)
	}

	if _, err = c.CreateStage(
		ctx,
		&httpapi.CreateStageInput{ApiId: a.ApiId, StageName: new("prod"), AutoDeploy: new(true)},
	); err != nil {
		t.Fatal(err)
	}

	exported, err := c.ExportApi(ctx, &httpapi.ExportApiInput{
		ApiId: a.ApiId, Specification: new("OAS30"), OutputType: new("JSON"), StageName: new("prod"),
		IncludeExtensions: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	var root map[string]any
	if err = json.Unmarshal(exported.Body, &root); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ReimportApi(
		ctx,
		&httpapi.ReimportApiInput{ApiId: a.ApiId, Body: new(string(exported.Body))},
	); err != nil {
		t.Fatal(err)
	}
}

func TestAPIGatewayOpenAPIWarningsRollback(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	spec := []byte(`{"openapi":"3.0.1","info":{"title":"warnings","version":"1"},"paths":{"/pets":{"get":{}}}}`)
	if _, err := c.ImportRestApi(ctx, &rest.ImportRestApiInput{Body: spec, FailOnWarnings: true}); err == nil {
		t.Fatal("expected import warnings failure")
	}

	all, err := c.GetRestApis(ctx, &rest.GetRestApisInput{})
	if err != nil || len(all.Items) != 0 {
		t.Fatalf("failed import leaked API: %+v %v", all, err)
	}

	a, err := c.ImportRestApi(ctx, &rest.ImportRestApiInput{Body: spec})
	if err != nil || len(a.Warnings) != 1 {
		t.Fatalf("warnings: %+v %v", a, err)
	}

	invalid := strings.ReplaceAll(
		string(spec),
		`"get":{}`,
		`"get":{"x-amazon-apigateway-integration":{"type":"invalid"}}`,
	)
	if _, err = c.PutRestApi(
		ctx,
		&rest.PutRestApiInput{RestApiId: a.Id, Body: []byte(invalid), Mode: resttypes.PutModeOverwrite},
	); err == nil {
		t.Fatal("expected invalid integration")
	}

	resources, err := c.GetResources(ctx, &rest.GetResourcesInput{RestApiId: a.Id})
	if err != nil || len(resources.Items) != 2 {
		t.Fatalf("failed overwrite changed resources: %+v %v", resources, err)
	}
}

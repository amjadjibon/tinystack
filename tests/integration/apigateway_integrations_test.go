package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rest "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	mappedJSON = "application/json"
	mappedText = "text/plain"
)

func newMappedAPI(t *testing.T, c *rest.Client) *rest.CreateRestApiOutput {
	t.Helper()
	a, err := c.CreateRestApi(context.Background(), &rest.CreateRestApiInput{Name: new("mapped")})
	if err != nil {
		t.Fatal(err)
	}

	return a
}

func deployMappedAPI(t *testing.T, c *rest.Client, id *string, stage string) {
	t.Helper()
	_, err := c.CreateDeployment(context.Background(), &rest.CreateDeploymentInput{
		RestApiId: id, StageName: new(stage), Variables: map[string]string{"tenant": "demo"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func putMappedResponse(t *testing.T, c *rest.Client, response *rest.PutIntegrationResponseInput) {
	t.Helper()
	headers := map[string]bool{}
	for name := range response.ResponseParameters {
		headers[name] = false
	}

	ctx := context.Background()
	_, err := c.PutMethodResponse(ctx, &rest.PutMethodResponseInput{
		RestApiId: response.RestApiId, ResourceId: response.ResourceId, HttpMethod: response.HttpMethod,
		StatusCode: response.StatusCode, ResponseParameters: headers,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutIntegrationResponse(ctx, response); err != nil {
		t.Fatal(err)
	}
}

func TestRESTAPIHTTPIntegration(t *testing.T) {
	s := server(t)
	c := restClient(s)
	a := newMappedAPI(t, c)
	ctx := context.Background()
	resource, err := c.CreateResource(ctx, &rest.CreateResourceInput{
		RestApiId: a.Id, ParentId: a.RootResourceId, PathPart: new("{id}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.EscapedPath() != "/upstream/a%20b" {
			http.NotFound(w, r)
			return
		}

		q := r.URL.Query()
		if q.Get("q") != "a / b" || q.Get("tenant") != "demo" || q.Get("fixed") != "yes" || q.Has("ignored") ||
			r.Header.Get("X-Source") != "request" || r.Header.Get("X-Method") != "POST" ||
			r.Header.Get("X-Static") != "literal" || r.Header.Get("X-Ignored") != "" {
			t.Errorf("backend request: %s %v", r.URL, r.Header)
		}

		payload, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}

		w.Header().Set("Content-Type", mappedJSON)
		w.Header().Set("X-Backend", "copied")
		if string(payload) == `{"name":"pet","id":"a b","stage":"demo"}` {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"message":"stored","trace":{"id":"123"}}`)
			return
		}

		w.WriteHeader(http.StatusTeapot)
		_, _ = fmt.Fprintf(w, `{"unmapped":%q}`, payload)
	}))
	t.Cleanup(backend.Close)
	restMethod(t, c, *a.Id, *resource.Id, "POST", &rest.PutIntegrationInput{
		Type: resttypes.IntegrationTypeHttp, Uri: new(backend.URL + "/upstream/{part}?fixed=yes"),
		IntegrationHttpMethod: new("PUT"), PassthroughBehavior: new("WHEN_NO_MATCH"),
		RequestParameters: map[string]string{
			"integration.request.path.part":          "method.request.path.id",
			"integration.request.querystring.q":      "method.request.querystring.term",
			"integration.request.querystring.tenant": "stageVariables.tenant",
			"integration.request.header.X-Source":    "method.request.header.X-Original",
			"integration.request.header.X-Method":    "context.httpMethod",
			"integration.request.header.X-Static":    "'literal'",
		},
		RequestTemplates: map[string]string{
			mappedJSON: `{"name":$input.json('$.name'),"id":"$input.params('id')","stage":"$stageVariables.tenant"}`,
		},
	})
	putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
		RestApiId: a.Id, ResourceId: resource.Id, HttpMethod: new("POST"), StatusCode: new("202"),
		SelectionPattern: new("2[0-9]{2}"), ResponseTemplates: map[string]string{
			mappedJSON: `{"result":$input.json('$.message')}`,
			mappedText: `$input.path('$.message') from $context.resourcePath`,
		},
		ResponseParameters: map[string]string{
			"method.response.header.X-Copy":   "integration.response.header.X-Backend",
			"method.response.header.X-Trace":  "integration.response.body.trace.id",
			"method.response.header.X-Static": "'static'",
		},
	})
	putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
		RestApiId: a.Id, ResourceId: resource.Id, HttpMethod: new("POST"), StatusCode: new("200"),
	})
	deployMappedAPI(t, c, a.Id, "first")

	const endpoint = "/a%20b?term=a+%2F+b&ignored=1"
	headers := map[string]string{
		"Content-Type": "application/json; charset=utf-8", "Accept": mappedText,
		"X-Original": "request", "X-Ignored": "secret",
	}
	status, header, body := call(t, s, "POST", *a.Id, "/first"+endpoint, headers, `{"name":"pet"}`)
	if status != 202 || body != "stored from /{id}" || header.Get("X-Copy") != "copied" ||
		header.Get(
			"X-Trace",
		) != "123" || header.Get("X-Static") != "static" || header.Get("Content-Type") != mappedText {
		t.Fatalf("%d %v %s", status, header, body)
	}

	headers["Content-Type"] = mappedText
	status, _, body = call(t, s, "POST", *a.Id, "/first"+endpoint, headers, "plain")
	if status != 200 || body != `{"unmapped":"plain"}` {
		t.Fatalf("passthrough: %d %s", status, body)
	}

	for _, tc := range []struct {
		stage, behavior string
		templates       bool
		want            int
	}{
		{"only", "WHEN_NO_TEMPLATES", true, 415},
		{"never", "NEVER", true, 415},
		{"empty", "WHEN_NO_TEMPLATES", false, 200},
		{"nothing", "NEVER", false, 415},
	} {
		patches := []resttypes.PatchOperation{replace("/passthroughBehavior", tc.behavior)}
		if tc.stage == "empty" {
			patches = append(patches, resttypes.PatchOperation{
				Op: resttypes.OpRemove, Path: new("/requestTemplates/application~1json"),
			})
		}

		out, updateErr := c.UpdateIntegration(ctx, &rest.UpdateIntegrationInput{
			RestApiId: a.Id, ResourceId: resource.Id, HttpMethod: new("POST"), PatchOperations: patches,
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}

		if out.Type != resttypes.IntegrationTypeHttp || (len(out.RequestTemplates) > 0) != tc.templates {
			t.Fatalf("update integration: %+v", out)
		}

		deployMappedAPI(t, c, a.Id, tc.stage)
		status, _, body = call(t, s, "POST", *a.Id, "/"+tc.stage+endpoint, headers, "plain")
		if status != tc.want {
			t.Fatalf("%s: %d %s", tc.stage, status, body)
		}
	}

	response, err := c.GetIntegrationResponse(ctx, &rest.GetIntegrationResponseInput{
		RestApiId: a.Id, ResourceId: resource.Id, HttpMethod: new("POST"), StatusCode: new("202"),
	})
	if err != nil || response.ResponseTemplates[mappedText] == "" {
		t.Fatalf("get response: %+v %v", response, err)
	}

	_, err = c.UpdateIntegrationResponse(ctx, &rest.UpdateIntegrationResponseInput{
		RestApiId: a.Id, ResourceId: resource.Id, HttpMethod: new("POST"), StatusCode: new("202"),
		PatchOperations: []resttypes.PatchOperation{replace("/responseTemplates/text~1plain", "new response")},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteIntegrationResponse(ctx, &rest.DeleteIntegrationResponseInput{
		RestApiId: a.Id, ResourceId: resource.Id, HttpMethod: new("POST"), StatusCode: new("202"),
	})
	if err != nil {
		t.Fatal(err)
	}

	headers["Content-Type"] = mappedJSON
	status, _, body = call(t, s, "POST", *a.Id, "/first"+endpoint, headers, `{"name":"pet"}`)
	if status != 202 || body != "stored from /{id}" {
		t.Fatalf("original deployment: %d %s", status, body)
	}
}

func TestRESTAPILambdaIntegration(t *testing.T) {
	s := server(t)
	c := restClient(s)
	a := newMappedAPI(t, c)
	functions := lambdaClient(s, "test")
	echo := createFunction(t, functions, "mapped-echo", "app.echo")
	fail := createFunction(t, functions, "mapped-error", "app.fail")
	for _, verb := range []string{"POST", "PUT"} {
		fn := echo
		if verb == "PUT" {
			fn = fail
		}

		restMethod(t, c, *a.Id, *a.RootResourceId, verb, &rest.PutIntegrationInput{
			Type:                  resttypes.IntegrationTypeAws,
			IntegrationHttpMethod: new("POST"),
			Uri:                   lambdaURI(fn.FunctionArn),
			RequestTemplates: map[string]string{
				mappedJSON: `{"value":$input.json('$.id'),"http":"$context.httpMethod"}`,
			},
		})
		putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
			RestApiId: a.Id, ResourceId: a.RootResourceId, HttpMethod: new(verb), StatusCode: new("201"),
			ResponseTemplates: map[string]string{mappedJSON: `$input.json('$.event')`},
		})
		putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
			RestApiId: a.Id, ResourceId: a.RootResourceId, HttpMethod: new(verb), StatusCode: new("422"),
			SelectionPattern: new("bo+m"), ResponseTemplates: map[string]string{
				mappedJSON: `{"error":$input.json('$.errorMessage')}`,
			},
		})
	}

	deployMappedAPI(t, c, a.Id, "prod")
	status, _, body := call(t, s, "POST", *a.Id, "/prod/", nil, `{"id":42}`)
	if status != 201 || body != `{"value":42,"http":"POST"}` {
		t.Fatalf("lambda success: %d %s", status, body)
	}

	status, _, body = call(t, s, "PUT", *a.Id, "/prod/", nil, `{"id":42}`)
	if status != 422 || body != `{"error":"boom"}` {
		t.Fatalf("lambda error: %d %s", status, body)
	}
}

func TestRESTAPIServiceIntegration(t *testing.T) {
	s := server(t)
	c := restClient(s)
	a := newMappedAPI(t, c)
	ctx := context.Background()
	queue := sqsClient(s, "test")
	created, err := queue.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("mapped-queue")})
	if err != nil {
		t.Fatal(err)
	}

	restMethod(t, c, *a.Id, *a.RootResourceId, "POST", &rest.PutIntegrationInput{
		Type: resttypes.IntegrationTypeAws, IntegrationHttpMethod: new("POST"),
		Uri: new("arn:aws:apigateway:us-east-1:sqs:path/000000000000/mapped-queue"),
		RequestParameters: map[string]string{
			"integration.request.header.Content-Type": "'application/x-www-form-urlencoded'",
		},
		RequestTemplates: map[string]string{mappedJSON: `Action=SendMessage&MessageBody=$util.urlEncode($input.body)`},
	})
	restMethod(t, c, *a.Id, *a.RootResourceId, "PUT", &rest.PutIntegrationInput{
		Type: resttypes.IntegrationTypeAws, IntegrationHttpMethod: new("POST"),
		Uri: new("arn:aws:apigateway:us-east-1:sqs:action/SendMessage"),
		RequestTemplates: map[string]string{
			mappedJSON: fmt.Sprintf(`{"QueueUrl":%q,"MessageBody":"action"}`, *created.QueueUrl),
		},
	})
	for _, verb := range []string{"POST", "PUT"} {
		putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
			RestApiId: a.Id, ResourceId: a.RootResourceId, HttpMethod: new(verb), StatusCode: new("200"),
		})
	}

	deployMappedAPI(t, c, a.Id, "prod")
	for _, tc := range []struct{ verb, message string }{{"POST", `{"job":"a&b"}`}, {"PUT", "action"}} {
		status, _, body := call(t, s, tc.verb, *a.Id, "/prod/", nil, tc.message)
		if status != 200 || !strings.Contains(body, `"MessageId"`) {
			t.Fatalf("SQS send: %d %s", status, body)
		}

		received, receiveErr := queue.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: created.QueueUrl})
		if receiveErr != nil || len(received.Messages) != 1 || *received.Messages[0].Body != tc.message {
			t.Fatalf("SQS receive: %+v %v", received, receiveErr)
		}
	}

	_, err = c.UpdateIntegration(ctx, &rest.UpdateIntegrationInput{
		RestApiId:  a.Id,
		ResourceId: a.RootResourceId,
		HttpMethod: new("POST"),
		PatchOperations: []resttypes.PatchOperation{
			replace("/uri", "arn:aws:apigateway:us-east-1:sqs:action/PurgeQueue"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported AWS integration target") {
		t.Fatalf("unsupported action: %v", err)
	}
}

func TestRESTAPIMappedIntegrationOpenAPI(t *testing.T) {
	s := server(t)
	c := restClient(s)
	ctx := context.Background()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/mapped" {
			http.NotFound(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", mappedJSON)
		w.Header().Set("X-Backend", "present")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))
	t.Cleanup(backend.Close)

	def := fmt.Sprintf(`{
		"openapi":"3.0.1","info":{"title":"mapped","version":"1"},"paths":{"/":{"post":{
			"responses":{"202":{"description":"mapped","headers":{"X-Copy":{"schema":{"type":"string"}}}}},
			"x-amazon-apigateway-integration":{
				"type":"http","httpMethod":"POST","uri":%q,"passthroughBehavior":"never",
				"requestTemplates":{"application/json":"{\"input\":$input.json('$')}"},
				"responses":{"2[0-9]{2}":{"statusCode":"202",
					"responseTemplates":{"application/json":"$input.json('$.input')"},
					"responseParameters":{"method.response.header.X-Copy":"integration.response.header.X-Backend"}}}
			}
		}}}}`, backend.URL+"/mapped")
	importAPI := func(body []byte) *rest.ImportRestApiOutput {
		t.Helper()
		out, err := c.ImportRestApi(ctx, &rest.ImportRestApiInput{Body: body, FailOnWarnings: true})
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	a := importAPI([]byte(def))
	deployMappedAPI(t, c, a.Id, "prod")
	for _, format := range []string{"oas30", "swagger"} {
		out, err := c.GetExport(ctx, &rest.GetExportInput{
			RestApiId: a.Id, StageName: new("prod"), ExportType: new(format), Accepts: new("application/yaml"),
			Parameters: map[string]string{"extensions": "integrations"},
		})
		if err != nil {
			t.Fatal(err)
		}

		copyAPI := importAPI(out.Body)
		deployMappedAPI(t, c, copyAPI.Id, "prod")
		status, header, body := call(t, s, "POST", *copyAPI.Id, "/prod/", nil, `{"ok":true}`)
		if status != 202 || header.Get("X-Copy") != "present" || body != `{"ok":true}` {
			t.Fatalf("%s round trip: %d %v %s", format, status, header, body)
		}

		status, _, body = call(
			t,
			s,
			"POST",
			*copyAPI.Id,
			"/prod/",
			map[string]string{"Content-Type": mappedText},
			"oops",
		)
		if status != 415 {
			t.Fatalf("%s passthrough: %d %s", format, status, body)
		}
	}
}

func TestRESTAPIServiceIntegrationRole(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	c := restClient(s)
	a := newMappedAPI(t, c)
	ctx := context.Background()
	db := client(s, "test", "us-east-1")
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: new("mapped-items"), BillingMode: ddbtypes.BillingModePayPerRequest,
		KeySchema: []ddbtypes.KeySchemaElement{{AttributeName: new("id"), KeyType: ddbtypes.KeyTypeHash}},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: new("id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		verb, action, template string
		choice                 roleChoice
	}{
		{
			"POST", "PutItem", `{"TableName":"mapped-items","Item":{"id":{"S":"example"},"value":{"B":"dGVzdA=="}}}`,
			roleAllowed,
		},
		{"GET", "GetItem", `{"TableName":"mapped-items","Key":{"id":{"S":"example"}}}`, roleAllowed},
		{"PUT", "PutItem", `{"TableName":"mapped-items","Item":{"id":{"S":"forbidden"}}}`, roleNoPermission},
	} {
		source := "arn:aws:execute-api:us-east-1:" + deliveryAccount + ":" + *a.Id + "/prod/" + tc.verb + "/"
		resource := "arn:aws:dynamodb:us-east-1:" + deliveryAccount + ":table/mapped-items"
		trust, permission := tc.choice.documentsFor("apigateway.amazonaws.com", source, resource, "dynamodb:"+tc.action)
		role := createDeliveryRole(t, s, "mapped-"+strings.ToLower(tc.verb), trust, permission)
		restMethod(t, c, *a.Id, *a.RootResourceId, tc.verb, &rest.PutIntegrationInput{
			Type: resttypes.IntegrationTypeAws, IntegrationHttpMethod: new("POST"), Credentials: new(role),
			Uri:              new("arn:aws:apigateway:us-east-1:dynamodb:action/" + tc.action),
			RequestTemplates: map[string]string{mappedJSON: tc.template},
		})
		putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
			RestApiId: a.Id, ResourceId: a.RootResourceId, HttpMethod: new(tc.verb), StatusCode: new("200"),
		})
	}

	deployMappedAPI(t, c, a.Id, "prod")
	status, _, body := call(t, s, "POST", *a.Id, "/prod/", nil, "")
	if status != 200 {
		t.Fatalf("PutItem: %d %s", status, body)
	}

	status, _, body = call(t, s, "GET", *a.Id, "/prod/", nil, "")
	if status != 200 || !strings.Contains(body, "dGVzdA==") {
		t.Fatalf("GetItem: %d %s", status, body)
	}

	status, _, body = call(t, s, "PUT", *a.Id, "/prod/", nil, "")
	if status != 500 {
		t.Fatalf("denied PutItem: %d %s", status, body)
	}

	out, err := db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: new("mapped-items"), Key: map[string]ddbtypes.AttributeValue{
			"id": &ddbtypes.AttributeValueMemberS{Value: "forbidden"},
		},
	})
	if err != nil || len(out.Item) != 0 {
		t.Fatalf("denied PutItem wrote an item: %+v %v", out, err)
	}
}

func TestRESTAPIHTTPIntegrationRedirectAndContentHandling(t *testing.T) {
	s := server(t)
	c := restClient(s)
	a := newMappedAPI(t, c)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/mapped" {
			http.NotFound(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "\x00data" {
			t.Errorf("decoded request: %q %v", body, err)
		}

		w.Header().Set("Location", "/redirected")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write(body)
	}))
	t.Cleanup(backend.Close)
	restMethod(t, c, *a.Id, *a.RootResourceId, "POST", &rest.PutIntegrationInput{
		Type: resttypes.IntegrationTypeHttp, Uri: new(backend.URL + "/mapped"), IntegrationHttpMethod: new("POST"),
		ContentHandling: resttypes.ContentHandlingStrategyConvertToBinary,
	})
	putMappedResponse(t, c, &rest.PutIntegrationResponseInput{
		RestApiId:        a.Id,
		ResourceId:       a.RootResourceId,
		HttpMethod:       new("POST"),
		StatusCode:       new("200"),
		SelectionPattern: new("302"),
		ContentHandling:  resttypes.ContentHandlingStrategyConvertToText,
		ResponseParameters: map[string]string{
			"method.response.header.X-Redirect": "integration.response.header.Location",
		},
	})
	deployMappedAPI(t, c, a.Id, "prod")

	const encoded = "AGRhdGE="
	status, header, body := call(t, s, "POST", *a.Id, "/prod/", nil, encoded)
	if status != 200 || body != encoded || header.Get("X-Redirect") != "/redirected" {
		t.Fatalf("redirect and conversion: %d %v %s", status, header, body)
	}
}

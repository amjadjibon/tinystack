package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	apigw "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func createRuntimeHTTPAPI(t *testing.T, c *apigw.Client, name string) *string {
	t.Helper()
	a, err := c.CreateApi(t.Context(), &apigw.CreateApiInput{
		Name: &name, ProtocolType: apigwtypes.ProtocolTypeHttp,
	})
	if err != nil {
		t.Fatal(err)
	}

	return a.ApiId
}

func TestHTTPAPIParameterMapping(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := t.Context()
	id := createRuntimeHTTPAPI(t, c, "parameter-mapping")
	backend := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mapped/123 item" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("X-Backend", "blue")
		w.Header().Set("X-Remove", "drop")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "response", "copy": r.Header.Get("X-Copy"), "drop": r.Header.Get("X-Drop"),
			"append": r.Header.Values("X-Append"), "body": r.Header.Get("X-Body"), "query": r.URL.Query(),
		})
	}))
	defer backend.Close()
	i, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeHttpProxy, IntegrationMethod: new("ANY"),
		IntegrationUri: new(backend.URL + "/original?fixed=yes"), PayloadFormatVersion: new("1.0"),
		RequestParameters: map[string]string{
			"overwrite:path":            "/mapped/${request.path.id}",
			"remove:header.X-Drop":      "",
			"overwrite:header.x-copy":   "${request.header.X-Drop}-${context.stage}-${stageVariables.release}",
			"append:header.X-Append":    "$request.querystring.add",
			"overwrite:header.X-Body":   "$request.body.items[0].name",
			"remove:querystring.drop":   "",
			"append:querystring.keep":   "extra",
			"overwrite:querystring.add": "new",
		},
		ResponseParameters: map[string]map[string]string{
			"201": {
				"overwrite:statuscode": "202", "overwrite:header.X-Backend": "$response.body.name",
				"append:header.X-Extra": "another", "remove:header.X-Remove": "",
				"overwrite:header.X-Copy": "${response.header.X-Backend}-${context.status}",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stored, err := c.GetIntegration(ctx, &apigw.GetIntegrationInput{ApiId: id, IntegrationId: i.IntegrationId})
	if err != nil || stored.RequestParameters["overwrite:path"] != "/mapped/${request.path.id}" ||
		stored.ResponseParameters["201"]["overwrite:statuscode"] != "202" {
		t.Fatalf("GetIntegration = %+v, %v", stored, err)
	}

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: new("POST /items/{id}"), Target: new("integrations/" + *i.IntegrationId),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: new("prod"), AutoDeploy: new(true), StageVariables: map[string]string{"release": "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, headers, body := call(t, s, http.MethodPost, *id, "/prod/items/123%20item?add=a&add=b&keep=base&drop=old",
		map[string]string{"X-Drop": "incoming", "X-Append": "init", "Content-Type": "application/json"},
		`{"items":[{"name":"mapped"}]}`)
	if status != http.StatusAccepted || headers.Get("X-Backend") != "response" ||
		headers.Get("X-Remove") != "" || headers.Get("X-Copy") != "blue-201" || headers.Get("X-Extra") != "another" {
		t.Fatalf("mapped response = %d %v %s", status, headers, body)
	}

	for _, fragment := range []string{
		`"copy":"incoming-prod-v1"`, `"drop":""`, `"body":"mapped"`, `"append":["init","a,b"]`,
		`"query":{"add":["new"],"fixed":["yes"],"keep":["base","extra"]}`,
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected %s in backend response %s", fragment, body)
		}
	}

	for _, test := range []struct{ key, value, message string }{
		{"append:path", "/bad", "Invalid mapping expression specified"},
		{"overwrite:header.Authorization", "x", "reserved"},
		{"overwrite:header.X-Body", "$request.body..name", "Invalid mapping expression specified"},
		{"overwrite:header.X-Body", "${request.querystring.}", "Invalid mapping expression specified"},
	} {
		_, updateErr := c.UpdateIntegration(ctx, &apigw.UpdateIntegrationInput{
			ApiId: id, IntegrationId: i.IntegrationId, RequestParameters: map[string]string{test.key: test.value},
		})
		code(t, updateErr, "BadRequestException")
		if !strings.Contains(updateErr.Error(), test.message) {
			t.Fatalf("validation = %v", updateErr)
		}
	}

	_, err = c.UpdateIntegration(ctx, &apigw.UpdateIntegrationInput{
		ApiId: id, IntegrationId: i.IntegrationId,
		ResponseParameters: map[string]map[string]string{"600": {"overwrite:statuscode": "200"}},
	})
	code(t, err, "BadRequestException")

	_, err = c.UpdateIntegration(ctx, &apigw.UpdateIntegrationInput{
		ApiId: id, IntegrationId: i.IntegrationId, ResponseParameters: map[string]map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	if status, _, body = call(
		t,
		s,
		http.MethodPost,
		*id,
		"/prod/items/123%20item",
		nil,
		"",
	); status != http.StatusCreated {
		t.Fatalf("cleared mapping = %d %s", status, body)
	}
}

func TestHTTPAPILambdaParameterMapping(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := t.Context()
	f := createFunction(t, lambdaClient(s, "test"), "mapped", "app.respond")
	id := createRuntimeHTTPAPI(t, c, "mapped-lambda")
	input := &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeAwsProxy, IntegrationUri: f.FunctionArn,
		PayloadFormatVersion: new("2.0"), RequestParameters: map[string]string{"overwrite:path": "/unsupported"},
	}
	_, err := c.CreateIntegration(ctx, input)
	code(t, err, "BadRequestException")

	input.RequestParameters = map[string]string{
		"overwrite:header.X-Input": "$request.path.id", "append:querystring.test": "$context.routeKey",
	}
	input.ResponseParameters = map[string]map[string]string{
		"201": {"overwrite:statuscode": "203", "overwrite:header.X-Mapped": "$response.header.X-Handler"},
	}
	integration, err := c.CreateIntegration(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: new("GET /item/{id}"), Target: new("integrations/" + *integration.IntegrationId),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateStage(ctx, &apigw.CreateStageInput{ApiId: id, StageName: new("$default"), AutoDeploy: new(true)})
	if err != nil {
		t.Fatal(err)
	}

	status, headers, body := call(t, s, http.MethodGet, *id, "/item/id?test=initial", nil, "")
	if status != http.StatusNonAuthoritativeInfo || headers.Get("X-Mapped") != "respond" ||
		!strings.Contains(body, `"x-input":"id"`) || !strings.Contains(body, `"test":"initial,GET /item/{id}"`) ||
		!strings.Contains(body, `"rawPath":"/item/id"`) {
		t.Fatalf("mapped Lambda = %d %v %s", status, headers, body)
	}
}

func TestHTTPAPIThrottling(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := t.Context()
	id := createRuntimeHTTPAPI(t, c, "throttling")
	backend := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend" {
			http.NotFound(w, r)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	i, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeHttpProxy, IntegrationMethod: new("GET"),
		IntegrationUri: new(backend.URL + "/backend"), PayloadFormatVersion: new("1.0"),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, route := range []string{"GET /a", "GET /b", "GET /c"} {
		_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
			ApiId: id, RouteKey: &route, Target: new("integrations/" + *i.IntegrationId),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: new("invalid"), DefaultRouteSettings: &apigwtypes.RouteSettings{
			ThrottlingRateLimit: new(-0.5),
		},
	})
	code(t, err, "BadRequestException")

	defaults := &apigwtypes.RouteSettings{ThrottlingBurstLimit: new(int32(1)), ThrottlingRateLimit: new(0.0)}
	for _, stage := range []string{"prod", "dev"} {
		_, err = c.CreateStage(ctx, &apigw.CreateStageInput{
			ApiId: id, StageName: &stage, AutoDeploy: new(true), DefaultRouteSettings: defaults,
			RouteSettings: map[string]apigwtypes.RouteSettings{"GET /b": {ThrottlingBurstLimit: new(int32(2))}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	check := func(path string, want int) {
		t.Helper()
		got, _, body := call(t, s, http.MethodGet, *id, path, nil, "")
		if got != want || (want == http.StatusTooManyRequests && body != `{"message":"Too Many Requests"}`) {
			t.Fatalf("%s = %d %s, want %d", path, got, body, want)
		}
	}
	check("/prod/a", http.StatusNoContent)
	check("/prod/a", http.StatusTooManyRequests)
	check("/prod/b", http.StatusNoContent)
	check("/prod/b", http.StatusNoContent)
	check("/prod/b", http.StatusTooManyRequests)
	check("/prod/c", http.StatusNoContent)
	check("/dev/a", http.StatusNoContent)
	check("/dev/a", http.StatusTooManyRequests)

	_, err = c.UpdateStage(ctx, &apigw.UpdateStageInput{
		ApiId: id, StageName: new("prod"), DefaultRouteSettings: &apigwtypes.RouteSettings{
			ThrottlingBurstLimit: new(int32(1)), ThrottlingRateLimit: new(2.0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	check("/prod/a", http.StatusNoContent)
	check("/prod/a", http.StatusTooManyRequests)
	waitFor(t, "route token bucket to refill", func() bool {
		status, _, _ := call(t, s, http.MethodGet, *id, "/prod/a", nil, "")
		return status == http.StatusNoContent
	})
}

type httpAuthorizerRuntime struct {
	fakeRuntime
	invocations atomic.Int64
}

func (r *httpAuthorizerRuntime) Invoke(
	ctx context.Context,
	fn *lambda.Function,
	payload []byte,
) (lambda.Result, error) {
	if fn.Handler != "app.cachedAuthorizer" {
		return r.fakeRuntime.Invoke(ctx, fn, payload)
	}

	var event struct {
		MethodArn, AuthorizationToken string
		IdentitySource                json.RawMessage
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	contextValues := map[string]any{"invocation": r.invocations.Add(1)}
	response := map[string]any{"context": contextValues}
	if event.MethodArn == "" {
		var sources []string
		if err := json.Unmarshal(event.IdentitySource, &sources); err != nil {
			return lambda.Result{}, err
		}

		response["isAuthorized"] = len(sources) == 2 && sources[0] != "deny"
	} else {
		response["principalId"] = event.AuthorizationToken
		response["policyDocument"] = map[string]any{
			"Version": "2012-10-17", "Statement": []any{map[string]string{
				"Effect": "Allow", "Action": "execute-api:Invoke", "Resource": event.MethodArn,
			}},
		}
	}

	data, err := json.Marshal(response)
	return lambda.Result{Payload: data}, err
}

func TestHTTPAPIAuthorizerCache(t *testing.T) {
	for _, format := range []string{"1.0", "2.0"} {
		t.Run(format, func(t *testing.T) {
			runtime := &httpAuthorizerRuntime{}
			s := serverWith(t, runtime)
			c := apigwClient(s)
			ctx := t.Context()
			functions := lambdaClient(s, "test")
			authorizer := createFunction(t, functions, "authorizer-cache", "app.cachedAuthorizer")
			backend := createFunction(t, functions, "authorizer-backend", "app.echo")
			id := createRuntimeHTTPAPI(t, c, "authorizer-cache")

			input := &apigw.CreateAuthorizerInput{
				ApiId: id, Name: new("cache"), AuthorizerType: apigwtypes.AuthorizerTypeRequest,
				AuthorizerUri: authorizer.FunctionArn, AuthorizerPayloadFormatVersion: &format,
				EnableSimpleResponses: new(format == "2.0"), AuthorizerResultTtlInSeconds: new(int32(60)),
				IdentitySource: []string{},
			}
			_, err := c.CreateAuthorizer(ctx, input)
			code(t, err, "BadRequestException")
			input.IdentitySource = []string{"$request.header.X-Token", "$request.querystring.key"}
			auth, err := c.CreateAuthorizer(ctx, input)
			if err != nil {
				t.Fatal(err)
			}

			i, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
				ApiId: id, IntegrationType: apigwtypes.IntegrationTypeAwsProxy, IntegrationUri: backend.FunctionArn,
				PayloadFormatVersion: new("2.0"),
			})
			if err != nil {
				t.Fatal(err)
			}

			for _, route := range []string{"GET /a", "GET /b"} {
				_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
					ApiId: id, RouteKey: &route, Target: new("integrations/" + *i.IntegrationId),
					AuthorizationType: apigwtypes.AuthorizationTypeCustom, AuthorizerId: auth.AuthorizerId,
				})
				if err != nil {
					t.Fatal(err)
				}
			}

			for _, stage := range []string{"dev", "prod"} {
				_, err = c.CreateStage(
					ctx,
					&apigw.CreateStageInput{ApiId: id, StageName: &stage, AutoDeploy: new(true)},
				)
				if err != nil {
					t.Fatal(err)
				}
			}

			invoke := func(path, token string, want int, invocations int64) string {
				t.Helper()
				status, _, body := call(t, s, http.MethodGet, *id, path, map[string]string{"x-token": token}, "")
				if n := runtime.invocations.Load(); status != want || n != invocations {
					t.Fatalf("%s = %d %s with %d invocations, want %d and %d", path, status, body, n, want, invocations)
				}

				return body
			}
			invoke("/prod/a", "user", http.StatusUnauthorized, 0)
			first := invoke("/prod/a?key=one", "user", http.StatusOK, 1)
			repeat := invoke("/prod/a?key=one", "user", http.StatusOK, 1)
			if !strings.Contains(first, `"invocation":1`) || !strings.Contains(repeat, `"invocation":1`) {
				t.Fatalf("authorizer context missing: %s %s", first, repeat)
			}

			otherRoute := http.StatusOK
			if format == "1.0" {
				otherRoute = http.StatusForbidden
			}

			invoke("/prod/b?key=one", "user", otherRoute, 1)
			invoke("/prod/a?key=two", "user", http.StatusOK, 2)
			invoke("/dev/a?key=one", "user", http.StatusOK, 3)
			if _, err = c.ResetAuthorizersCache(ctx, &apigw.ResetAuthorizersCacheInput{
				ApiId: id, StageName: new("prod"),
			}); err != nil {
				t.Fatal(err)
			}

			invoke("/dev/a?key=one", "user", http.StatusOK, 3)
			invoke("/prod/a?key=one", "user", http.StatusOK, 4)
			if format == "2.0" {
				invoke("/prod/a?key=one", "deny", http.StatusForbidden, 5)
				invoke("/prod/a?key=one", "deny", http.StatusForbidden, 5)
			}

			_, err = c.UpdateAuthorizer(ctx, &apigw.UpdateAuthorizerInput{
				ApiId: id, AuthorizerId: auth.AuthorizerId, AuthorizerResultTtlInSeconds: new(int32(1)),
			})
			if err != nil {
				t.Fatal(err)
			}

			n := runtime.invocations.Load() + 1
			invoke("/prod/a?key=one", "user", http.StatusOK, n)
			waitFor(t, "authorizer entry to expire", func() bool {
				status, _, _ := call(
					t,
					s,
					http.MethodGet,
					*id,
					"/prod/a?key=one",
					map[string]string{"X-Token": "user"},
					"",
				)
				return status == http.StatusOK && runtime.invocations.Load() > n
			})
		})
	}
}

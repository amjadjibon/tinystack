package integration_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	management "github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	apigw "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/coder/websocket"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type websocketEvent struct {
	Headers        map[string]string `json:"headers"`
	Query          map[string]string `json:"queryStringParameters"`
	Body           string            `json:"body"`
	RequestContext struct {
		RouteKey     string `json:"routeKey"`
		EventType    string `json:"eventType"`
		ConnectionID string `json:"connectionId"`
		RequestID    string `json:"requestId"`
		APIID        string `json:"apiId"`
		Stage        string `json:"stage"`
		DomainName   string `json:"domainName"`
		Identity     struct {
			SourceIP string `json:"sourceIp"`
		} `json:"identity"`
		ConnectedAt int64 `json:"connectedAt"`
	} `json:"requestContext"`
}

type websocketRuntime struct {
	events chan websocketEvent
	fakeRuntime
}

func (r *websocketRuntime) Invoke(_ context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event websocketEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.events <- event
	status := http.StatusOK
	if event.Headers["x-deny"] == "true" {
		status = http.StatusForbidden
	}

	response, err := json.Marshal(map[string]any{
		"statusCode": status,
		"body":       event.RequestContext.RouteKey + ":" + event.Body,
	})
	return lambda.Result{Payload: response}, err
}

func nextSocketEvent(t *testing.T, events <-chan websocketEvent, kind, route string) websocketEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.RequestContext.EventType != kind || event.RequestContext.RouteKey != route {
			t.Fatalf("event = %+v, want %s %s", event, kind, route)
		}

		return event
	case <-time.After(5 * time.Second):
		t.Fatalf("missing %s %s event", kind, route)
		return websocketEvent{}
	}
}

func TestWebSocketAPI(t *testing.T) {
	runtime := &websocketRuntime{events: make(chan websocketEvent, 32)}
	s := serverWith(t, runtime)
	c := apigwClient(s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fn := createFunction(t, lambdaClient(s, "test"), "websocket", "app.websocket")
	created, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name:         new("chat"),
		ProtocolType: apigwtypes.ProtocolTypeWebsocket, RouteSelectionExpression: new("$request.body.message.action"),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.ApiId
	if !strings.HasPrefix(aws.ToString(created.ApiEndpoint), "ws://") {
		t.Fatal(created.ApiEndpoint)
	}

	integration, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId:           id,
		IntegrationType: apigwtypes.IntegrationTypeAwsProxy, IntegrationUri: fn.FunctionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	routes := map[string]*string{}
	for _, key := range []string{"$connect", "$disconnect", "$default", "echo", "oneway"} {
		route, routeErr := c.CreateRoute(ctx, &apigw.CreateRouteInput{
			ApiId: id, RouteKey: new(key),
			Target: new("integrations/" + aws.ToString(integration.IntegrationId)),
		})
		if routeErr != nil {
			t.Fatal(routeErr)
		}

		routes[key] = route.RouteId
		if key == "$default" || key == "echo" {
			if _, callErr := c.CreateRouteResponse(ctx, &apigw.CreateRouteResponseInput{
				ApiId: id, RouteId: route.RouteId,
				RouteResponseKey: new("$default"),
			}); callErr != nil {
				t.Fatal(callErr)
			}
		}
	}

	if _, callErr := c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: new("chat"),
		AutoDeploy: new(true),
	}); callErr != nil {
		t.Fatal(callErr)
	}

	base := s.URL + "/_aws/execute-api/" + *id + "/chat"
	ws, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"?token=hello",
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Client": {"sdk"}}})
	if err != nil {
		t.Fatal(err)
	}

	defer ws.CloseNow()
	connect := nextSocketEvent(t, runtime.events, "CONNECT", "$connect")
	rc := connect.RequestContext
	if rc.ConnectionID == "" || rc.ConnectedAt == 0 || rc.RequestID == "" || rc.APIID != *id || rc.Stage != "chat" ||
		rc.DomainName == "" || rc.Identity.SourceIP != "127.0.0.1" || connect.Query["token"] != "hello" ||
		connect.Headers["x-client"] != "sdk" {
		t.Fatalf("connect event: %+v", connect)
	}

	manager := management.New(management.Options{
		Region: "us-east-1", BaseEndpoint: new(base),
		Credentials: c.Options().Credentials, Retryer: aws.NopRetryer{},
	})
	details, err := manager.GetConnection(ctx, &management.GetConnectionInput{ConnectionId: new(rc.ConnectionID)})
	if err != nil || details.ConnectedAt == nil || details.Identity == nil ||
		aws.ToString(details.Identity.SourceIp) != "127.0.0.1" || details.LastActiveAt == nil {
		t.Fatalf("details=%+v err=%v", details, err)
	}

	receive := func(want string) {
		t.Helper()
		_, body, readErr := ws.Read(ctx)
		if readErr != nil || string(body) != want {
			t.Fatalf("read %q, %v; want %q", body, readErr, want)
		}
	}

	for _, tc := range []struct{ body, route string }{
		{`{"message":{"action":"echo"}}`, "echo"},
		{`{"message":{"action":"missing"}}`, "$default"},
		{`not-json`, "$default"},
		{`{"message":{"action":"$connect"}}`, "$default"},
	} {
		if writeErr := ws.Write(ctx, websocket.MessageText, []byte(tc.body)); writeErr != nil {
			t.Fatal(writeErr)
		}

		receive(tc.route + ":" + tc.body)
		nextSocketEvent(t, runtime.events, "MESSAGE", tc.route)
	}

	if writeErr := ws.Write(ctx, websocket.MessageText, []byte(`{"message":{"action":"oneway"}}`)); writeErr != nil {
		t.Fatal(writeErr)
	}

	nextSocketEvent(t, runtime.events, "MESSAGE", "oneway")
	if _, callErr := manager.PostToConnection(ctx, &management.PostToConnectionInput{
		ConnectionId: new(rc.ConnectionID),
		Data:         []byte("server push"),
	}); callErr != nil {
		t.Fatal(callErr)
	}

	receive("server push") // Also proves the one-way route produced no response.
	if _, callErr := manager.DeleteConnection(
		ctx,
		&management.DeleteConnectionInput{ConnectionId: new(rc.ConnectionID)},
	); callErr != nil {
		t.Fatal(callErr)
	}

	if _, _, readErr := ws.Read(ctx); readErr == nil {
		t.Fatal("deleted connection remained open")
	}

	nextSocketEvent(t, runtime.events, "DISCONNECT", "$disconnect")
	_, err = manager.GetConnection(ctx, &management.GetConnectionInput{ConnectionId: new(rc.ConnectionID)})
	code(t, err, "GoneException")
	_, err = manager.PostToConnection(
		ctx,
		&management.PostToConnectionInput{ConnectionId: new("missing"), Data: []byte("x")},
	)
	code(t, err, "GoneException")
	_, err = manager.DeleteConnection(ctx, &management.DeleteConnectionInput{ConnectionId: new("missing")})
	code(t, err, "GoneException")

	rejected, response, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1),
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Deny": {"true"}}})
	if rejected != nil {
		rejected.CloseNow()
	}

	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("handshake response=%+v err=%v", response, err)
	}

	nextSocketEvent(t, runtime.events, "CONNECT", "$connect")

	// Host-style endpoints use the same API and stage, without DNS or fixed ports.
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", s.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	hostSocket, _, err := websocket.Dial(ctx, "ws://"+*id+".execute-api.us-east-1.amazonaws.com/chat",
		&websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}

	nextSocketEvent(t, runtime.events, "CONNECT", "$connect")
	hostSocket.CloseNow()
	nextSocketEvent(t, runtime.events, "DISCONNECT", "$disconnect")

	responses, err := c.GetRouteResponses(ctx, &apigw.GetRouteResponsesInput{ApiId: id, RouteId: routes["echo"]})
	if err != nil || len(responses.Items) != 1 {
		t.Fatalf("%+v %v", responses, err)
	}

	responseID := responses.Items[0].RouteResponseId
	if _, callErr := c.GetRouteResponse(ctx, &apigw.GetRouteResponseInput{
		ApiId: id, RouteId: routes["echo"],
		RouteResponseId: responseID,
	}); callErr != nil {
		t.Fatal(callErr)
	}

	if _, callErr := c.UpdateRouteResponse(ctx, &apigw.UpdateRouteResponseInput{
		ApiId: id, RouteId: routes["echo"],
		RouteResponseId: responseID, RouteResponseKey: new("$default"),
	}); callErr != nil {
		t.Fatal(callErr)
	}

	if _, callErr := c.DeleteRouteResponse(ctx, &apigw.DeleteRouteResponseInput{
		ApiId: id, RouteId: routes["echo"],
		RouteResponseId: responseID,
	}); callErr != nil {
		t.Fatal(callErr)
	}
}

func TestWebSocketAPIValidation(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := context.Background()
	for _, selection := range []string{"", "$request.header.action", "$request.body", "$request.body.a[0]"} {
		_, err := c.CreateApi(ctx, &apigw.CreateApiInput{
			Name:         new("invalid"),
			ProtocolType: apigwtypes.ProtocolTypeWebsocket, RouteSelectionExpression: new(selection),
		})
		code(t, err, "BadRequestException")
	}

	api, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name: new("chat"), ProtocolType: apigwtypes.ProtocolTypeWebsocket,
		RouteSelectionExpression: new("$request.body.action"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateApi(
		ctx,
		&apigw.UpdateApiInput{ApiId: api.ApiId, CorsConfiguration: &apigwtypes.Cors{AllowOrigins: []string{"*"}}},
	)
	code(t, err, "BadRequestException")
	keyed, err := c.UpdateApi(ctx, &apigw.UpdateApiInput{
		ApiId: api.ApiId, ApiKeySelectionExpression: new("$context.authorizer.usageIdentifierKey"),
	})
	if err != nil || aws.ToString(keyed.ApiKeySelectionExpression) != "$context.authorizer.usageIdentifierKey" {
		t.Fatalf("%+v %v", keyed, err)
	}

	_, err = c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId:           api.ApiId,
		IntegrationType: apigwtypes.IntegrationTypeHttpProxy, IntegrationUri: new("https://example.com"),
		IntegrationMethod: new("POST"),
	})
	code(t, err, "BadRequestException")
	_, err = c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId:           api.ApiId,
		IntegrationType: apigwtypes.IntegrationTypeMock, PayloadFormatVersion: new("1.0"),
	})
	code(t, err, "BadRequestException")
	_, err = c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId:           api.ApiId,
		IntegrationType: apigwtypes.IntegrationTypeMock, RequestTemplates: map[string]string{"application/json": "{}"},
	})
	code(t, err, "NotImplemented")
	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{ApiId: api.ApiId, RouteKey: new("$invalid")})
	code(t, err, "BadRequestException")
	_, err = c.CreateRoute(
		ctx,
		&apigw.CreateRouteInput{
			ApiId:             api.ApiId,
			RouteKey:          new("echo"),
			AuthorizationType: apigwtypes.AuthorizationTypeAwsIam,
		},
	)
	code(t, err, "BadRequestException")
	_, err = c.CreateIntegrationResponse(ctx, &apigw.CreateIntegrationResponseInput{
		ApiId:         api.ApiId,
		IntegrationId: new("missing"), IntegrationResponseKey: new("$default"),
	})
	code(t, err, "NotImplemented")

	httpAPI, httpErr := c.CreateApi(
		ctx,
		&apigw.CreateApiInput{Name: new("http"), ProtocolType: apigwtypes.ProtocolTypeHttp},
	)
	if httpErr != nil {
		t.Fatal(httpErr)
	}

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId:    httpAPI.ApiId,
		RouteKey: new("GET /echo"), ApiKeyRequired: new(true),
	})
	code(t, err, "BadRequestException")

	if _, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId:    httpAPI.ApiId,
		RouteKey: new("GET /echo"), ApiKeyRequired: new(false),
	}); err != nil {
		t.Fatal(err)
	}

	httpRoute, routeErr := c.CreateRoute(ctx, &apigw.CreateRouteInput{ApiId: httpAPI.ApiId, RouteKey: new("$default")})
	if routeErr != nil {
		t.Fatal(routeErr)
	}

	_, err = c.CreateRouteResponse(ctx, &apigw.CreateRouteResponseInput{
		ApiId:   httpAPI.ApiId,
		RouteId: httpRoute.RouteId, RouteResponseKey: new("$default"),
	})
	code(t, err, "BadRequestException")

	mock, err := c.CreateIntegration(
		ctx,
		&apigw.CreateIntegrationInput{ApiId: api.ApiId, IntegrationType: apigwtypes.IntegrationTypeMock},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, callErr := c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: api.ApiId, RouteKey: new("$connect"),
		Target: new("integrations/" + *mock.IntegrationId),
	}); callErr != nil {
		t.Fatal(callErr)
	}

	if _, callErr := c.CreateStage(
		ctx,
		&apigw.CreateStageInput{ApiId: api.ApiId, StageName: new("v1"), AutoDeploy: new(true)},
	); callErr != nil {
		t.Fatal(callErr)
	}

	socket, _, err := websocket.Dial(
		ctx,
		strings.Replace(s.URL, "http", "ws", 1)+"/_aws/execute-api/"+*api.ApiId+"/v1",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	socket.CloseNow()
}

func TestWebSocketDeploymentAndShutdown(t *testing.T) {
	runtime := &websocketRuntime{events: make(chan websocketEvent, 16)}
	s, shutdown := serverStore(t, testConfig(), runtime, &memory.Store{})
	var once sync.Once
	stop := func() { once.Do(shutdown) }
	t.Cleanup(stop)
	c := apigwClient(s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fn := createFunction(t, lambdaClient(s, "test"), "lifecycle", "app.websocket")
	created, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name:         new("lifecycle"),
		ProtocolType: apigwtypes.ProtocolTypeWebsocket, RouteSelectionExpression: new("$request.body.action"),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.ApiId
	integration, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId:           id,
		IntegrationType: apigwtypes.IntegrationTypeAwsProxy, IntegrationUri: fn.FunctionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	var routeID *string
	for _, key := range []string{"$connect", "$disconnect", "$default"} {
		route, routeErr := c.CreateRoute(ctx, &apigw.CreateRouteInput{
			ApiId: id, RouteKey: new(key),
			Target: new("integrations/" + *integration.IntegrationId),
		})
		if routeErr != nil {
			t.Fatal(routeErr)
		}

		if key == "$default" {
			routeID = route.RouteId
		}
	}

	if _, stageErr := c.CreateStage(
		ctx,
		&apigw.CreateStageInput{ApiId: id, StageName: new("manual")},
	); stageErr != nil {
		t.Fatal(stageErr)
	}

	wsURL := strings.Replace(s.URL, "http", "ws", 1) + "/_aws/execute-api/" + *id + "/manual"
	ws, response, err := websocket.Dial(ctx, wsURL, nil)
	if ws != nil {
		ws.CloseNow()
	}

	if err == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("undeployed stage: %+v %v", response, err)
	}

	if _, deployErr := c.CreateDeployment(
		ctx,
		&apigw.CreateDeploymentInput{ApiId: id, StageName: new("manual")},
	); deployErr != nil {
		t.Fatal(deployErr)
	}

	ws, _, err = websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer ws.CloseNow()
	event := nextSocketEvent(t, runtime.events, "CONNECT", "$connect")
	manager := management.New(management.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL + "/_aws/execute-api/" + *id + "/manual"),
		Credentials: c.Options().Credentials, Retryer: aws.NopRetryer{},
	})
	// Draft two-way settings do not affect a manually deployed stage.
	if _, responseErr := c.CreateRouteResponse(ctx, &apigw.CreateRouteResponseInput{
		ApiId: id, RouteId: routeID,
		RouteResponseKey: new("$default"),
	}); responseErr != nil {
		t.Fatal(responseErr)
	}

	if writeErr := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); writeErr != nil {
		t.Fatal(writeErr)
	}

	nextSocketEvent(t, runtime.events, "MESSAGE", "$default")
	if _, pushErr := manager.PostToConnection(ctx, &management.PostToConnectionInput{
		ConnectionId: new(event.RequestContext.ConnectionID), Data: []byte("barrier"),
	}); pushErr != nil {
		t.Fatal(pushErr)
	}

	_, body, err := ws.Read(ctx)
	if err != nil || string(body) != "barrier" {
		t.Fatalf("draft response leaked: %q %v", body, err)
	}

	// An existing connection observes the newly published deployment.
	if _, deployErr := c.CreateDeployment(
		ctx,
		&apigw.CreateDeploymentInput{ApiId: id, StageName: new("manual")},
	); deployErr != nil {
		t.Fatal(deployErr)
	}

	if writeErr := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); writeErr != nil {
		t.Fatal(writeErr)
	}

	_, body, err = ws.Read(ctx)
	if err != nil || string(body) != "$default:{}" {
		t.Fatalf("deployed response: %q %v", body, err)
	}

	nextSocketEvent(t, runtime.events, "MESSAGE", "$default")
	stop()
	if _, _, readErr := ws.Read(ctx); readErr == nil {
		t.Fatal("shutdown left a live socket")
	}

	nextSocketEvent(t, runtime.events, "DISCONNECT", "$disconnect")
}

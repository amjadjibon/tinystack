package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	apigw "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/coder/websocket"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

type socketAuthorizerRuntime struct {
	authorizations chan websocketEvent
	websocketRuntime
}

func (r *socketAuthorizerRuntime) Invoke(
	ctx context.Context, fn *lambda.Function, payload []byte,
) (lambda.Result, error) {
	if fn.Handler != "app.socketAuth" {
		return r.websocketRuntime.Invoke(ctx, fn, payload)
	}

	var event websocketEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.authorizations <- event
	effect := "Deny"
	if event.Headers["x-token"] == "allowed" && event.Query["client"] == "chat" {
		effect = "Allow"
	}

	out, err := json.Marshal(map[string]any{
		"principalId": "chat-user", "context": map[string]any{"team": "chat"},
		"policyDocument": map[string]any{"Statement": []any{map[string]string{
			"Action": "execute-api:Invoke", "Effect": effect, "Resource": event.MethodARN,
		}}},
	})
	return lambda.Result{Payload: out}, err
}

func TestWebSocketLambdaAuthorizer(t *testing.T) {
	runtime := &socketAuthorizerRuntime{
		websocketRuntime: websocketRuntime{events: make(chan websocketEvent, 8)},
		authorizations:   make(chan websocketEvent, 8),
	}
	s := serverWith(t, runtime)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := apigwClient(s)
	id := createAuthorizedSocketAPI(t, c)
	f := lambdaClient(s, "test")
	authorizer := createFunction(t, f, "socket-auth", "app.socketAuth")
	fn := createFunction(t, f, "socket-integration", "app.websocket")

	input := &apigw.CreateAuthorizerInput{
		ApiId: id, Name: new("connect"), AuthorizerType: apigwtypes.AuthorizerTypeRequest,
		AuthorizerUri:                authorizer.FunctionArn,
		IdentitySource:               []string{"route.request.header.X-Token", "route.request.querystring.client"},
		AuthorizerResultTtlInSeconds: new(int32(1)),
	}
	_, err := c.CreateAuthorizer(ctx, input)
	code(t, err, "BadRequestException")
	input.AuthorizerResultTtlInSeconds = nil
	auth, err := c.CreateAuthorizer(ctx, input)
	mustAuthorize(t, err)
	i, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeAwsProxy, IntegrationUri: fn.FunctionArn,
	})
	mustAuthorize(t, err)
	target := new("integrations/" + *i.IntegrationId)
	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: new("echo"), AuthorizerId: auth.AuthorizerId, Target: target,
		AuthorizationType: apigwtypes.AuthorizationTypeCustom,
	})
	code(t, err, "BadRequestException")
	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: new("$connect"), AuthorizerId: auth.AuthorizerId, Target: target,
		AuthorizationType: apigwtypes.AuthorizationTypeCustom,
	})
	mustAuthorize(t, err)
	for _, route := range []string{"echo", "$disconnect"} {
		_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{ApiId: id, RouteKey: new(route), Target: target})
		mustAuthorize(t, err)
	}

	_, err = c.CreateStage(ctx, &apigw.CreateStageInput{ApiId: id, AutoDeploy: new(true), StageName: new("prod")})
	mustAuthorize(t, err)
	endpoint := "ws" + strings.TrimPrefix(s.URL, "http") + "/_aws/execute-api/" + *id + "/prod?client=chat"
	rejectSocket(t, ctx, endpoint, nil, http.StatusUnauthorized)
	rejectSocket(t, ctx, endpoint, http.Header{"X-Token": {"denied"}}, http.StatusForbidden)
	assertSocketAuthorizerEvent(t, runtime.authorizations, *id)
	select {
	case ev := <-runtime.events:
		t.Fatalf("rejected connection reached the integration: %+v", ev)
	default:
	}

	sock, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Token": {"allowed"}},
	})
	mustAuthorize(t, err)
	defer sock.CloseNow()
	assertSocketAuthorizerEvent(t, runtime.authorizations, *id)
	checkContext := func(kind, route string) {
		t.Helper()
		event := nextSocketEvent(t, runtime.events, kind, route)
		fields := event.RequestContext.Authorizer
		if fields["principalId"] != "chat-user" || fields["team"] != "chat" {
			t.Fatalf("%s authorizer context = %+v", kind, fields)
		}
	}
	checkContext("CONNECT", "$connect")
	mustAuthorize(t, sock.Write(ctx, websocket.MessageText, []byte(`{"action":"echo"}`)))
	checkContext("MESSAGE", "echo")
	mustAuthorize(t, sock.Close(websocket.StatusNormalClosure, ""))
	checkContext("DISCONNECT", "$disconnect")
}

func createAuthorizedSocketAPI(t *testing.T, c *apigw.Client) *string {
	t.Helper()
	a, err := c.CreateApi(t.Context(), &apigw.CreateApiInput{
		Name: new("socket-auth"), ProtocolType: apigwtypes.ProtocolTypeWebsocket,
		RouteSelectionExpression: new("$request.body.action"),
	})
	mustAuthorize(t, err)
	return a.ApiId
}

func rejectSocket(t *testing.T, ctx context.Context, url string, headers http.Header, want int) {
	t.Helper()
	socket, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if socket != nil {
		_ = socket.CloseNow()
	}

	if err == nil || resp == nil || resp.StatusCode != want {
		t.Fatalf("handshake response = %+v, error = %v, want status %d", resp, err, want)
	}
}

func assertSocketAuthorizerEvent(t *testing.T, events <-chan websocketEvent, id string) {
	t.Helper()
	e := nextSocketEvent(t, events, "CONNECT", "$connect")
	want := "arn:aws:execute-api:us-east-1:000000000000:" + id + "/prod/$connect"
	if e.MethodARN != want || e.Type != "REQUEST" || e.RequestContext.ConnectionID == "" {
		t.Fatalf("authorizer event = %+v, want methodArn %s", e, want)
	}
}

func TestWebSocketIAMAuthorizer(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := apigwClient(s)
	id := createAuthorizedSocketAPI(t, c)
	i, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeMock,
	})
	mustAuthorize(t, err)
	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: new("$connect"), AuthorizationType: apigwtypes.AuthorizationTypeAwsIam,
		Target: new("integrations/" + *i.IntegrationId),
	})
	mustAuthorize(t, err)
	_, err = c.CreateStage(ctx, &apigw.CreateStageInput{ApiId: id, AutoDeploy: new(true), StageName: new("prod")})
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	credentials, err := authorizationProvider(key).Retrieve(ctx)
	mustAuthorize(t, err)
	url := s.URL + "/_aws/execute-api/" + *id + "/prod"
	endpoint := "ws" + strings.TrimPrefix(url, "http")
	signed := func() http.Header {
		t.Helper()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		mustAuthorize(t, reqErr)
		hash := sha256.Sum256(nil)
		mustAuthorize(t, v4.NewSigner().SignHTTP(
			ctx, credentials, req, hex.EncodeToString(hash[:]), "execute-api", "us-east-1", time.Now(),
		))
		return req.Header
	}

	rejectSocket(t, ctx, endpoint, nil, http.StatusForbidden)
	rejectSocket(t, ctx, endpoint, signed(), http.StatusForbidden)
	authorizationPolicy(t, s, fmt.Sprintf(
		`{"Statement":{"Effect":"Allow","Action":"execute-api:Invoke","Resource":%q}}`,
		"arn:aws:execute-api:us-east-1:000000000000:"+*id+"/prod/$connect",
	))
	sock, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: signed()})
	mustAuthorize(t, err)
	mustAuthorize(t, sock.Close(websocket.StatusNormalClosure, ""))
}

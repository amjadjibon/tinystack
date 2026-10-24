package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/appsync"
	astypes "github.com/aws/aws-sdk-go-v2/service/appsync/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/coder/websocket"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func eventConfig(kinds ...astypes.AuthenticationType) *astypes.EventConfig {
	cfg := &astypes.EventConfig{}
	for _, kind := range kinds {
		cfg.AuthProviders = append(cfg.AuthProviders, astypes.AuthProvider{AuthType: kind})
		cfg.ConnectionAuthModes = append(cfg.ConnectionAuthModes, astypes.AuthMode{AuthType: kind})
		cfg.DefaultPublishAuthModes = append(cfg.DefaultPublishAuthModes, astypes.AuthMode{AuthType: kind})
		cfg.DefaultSubscribeAuthModes = append(cfg.DefaultSubscribeAuthModes, astypes.AuthMode{AuthType: kind})
	}

	return cfg
}

func newEventAPI(t *testing.T, s *httptest.Server, cfg *astypes.EventConfig) (a *astypes.Api, apiKey string) {
	t.Helper()
	c := appsyncClient(s)
	out, err := c.CreateApi(context.Background(), &appsync.CreateApiInput{
		Name: new("events"), EventConfig: cfg, Tags: map[string]string{"team": "events"},
	})
	if err != nil {
		t.Fatal(err)
	}

	key, err := c.CreateApiKey(context.Background(), &appsync.CreateApiKeyInput{ApiId: out.Api.ApiId})
	if err != nil {
		t.Fatal(err)
	}

	return out.Api, *key.ApiKey.Id
}

func eventNamespace(t *testing.T, s *httptest.Server, id *string, name string, code *string) {
	t.Helper()
	_, err := appsyncClient(s).CreateChannelNamespace(context.Background(), &appsync.CreateChannelNamespaceInput{
		ApiId: id, Name: new(name), CodeHandlers: code,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func eventHTTP(t *testing.T, s *httptest.Server, a *astypes.Api, channel string, events []string,
	auth map[string]string,
) (status int, responseBody string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"channel": channel, "events": events})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+"/event",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}

	req.Host = a.Dns["HTTP"]
	if auth["Authorization"] == "sign" {
		auth = signedHeader(t, req.Host, "/event", string(body))
	}

	for k, v := range auth {
		req.Header.Set(k, v)
	}

	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(b)
}

func eventDial(t *testing.T, s *httptest.Server, a *astypes.Api, auth map[string]string) *realtime {
	t.Helper()
	b, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/event/realtime",
		&websocket.DialOptions{
			Host:         a.Dns["REALTIME"],
			Subprotocols: []string{"aws-appsync-event-ws", "header-" + base64.RawURLEncoding.EncodeToString(b)},
		})
	if err != nil {
		t.Fatal(err)
	}

	if resp.Body != nil {
		resp.Body.Close()
	}

	t.Cleanup(func() { conn.CloseNow() })
	return &realtime{t: t, conn: conn}
}

func TestAppSyncEventsControlPlane(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c := appsyncClient(s)
	a, key := newEventAPI(t, s, eventConfig(astypes.AuthenticationTypeApiKey, astypes.AuthenticationTypeAwsIam))
	id := a.ApiId
	if a.Dns["HTTP"] == "" || a.Dns["REALTIME"] == "" || a.Created == nil {
		t.Fatalf("api: %+v", a)
	}

	got, err := c.GetApi(ctx, &appsync.GetApiInput{ApiId: id})
	if err != nil || *got.Api.ApiArn != *a.ApiArn {
		t.Fatalf("GetApi: %+v %v", got, err)
	}

	graphqlID := newGraphqlAPI(t, c)
	if _, getErr := c.GetGraphqlApi(ctx, &appsync.GetGraphqlApiInput{ApiId: id}); getErr == nil {
		t.Fatal("GetGraphqlApi returned Event API")
	}

	if _, getErr := c.GetApi(ctx, &appsync.GetApiInput{ApiId: new(graphqlID)}); getErr == nil {
		t.Fatal("GetApi returned GraphQL API")
	}

	graphql, err := c.ListGraphqlApis(ctx, &appsync.ListGraphqlApisInput{})
	if err != nil || len(graphql.GraphqlApis) != 1 {
		t.Fatalf("GraphQL isolation: %+v %v", graphql, err)
	}

	b, _ := newEventAPI(t, s, eventConfig(astypes.AuthenticationTypeApiKey))
	listed, err := c.ListApis(ctx, &appsync.ListApisInput{MaxResults: 1})
	if err != nil || len(listed.Apis) != 1 || listed.NextToken == nil {
		t.Fatalf("ListApis: %+v %v", listed, err)
	}

	next, err := c.ListApis(ctx, &appsync.ListApisInput{MaxResults: 1, NextToken: listed.NextToken})
	if err != nil || len(next.Apis) != 1 || *listed.Apis[0].ApiId >= *next.Apis[0].ApiId {
		t.Fatalf("API pagination: %+v %v", next, err)
	}

	if _, listErr := c.ListApis(ctx, &appsync.ListApisInput{NextToken: new("!")}); listErr == nil {
		t.Fatal("bad pagination token accepted")
	}

	updated, err := c.UpdateApi(
		ctx,
		&appsync.UpdateApiInput{ApiId: id, Name: new("renamed"), EventConfig: a.EventConfig},
	)
	if err != nil || *updated.Api.Name != "renamed" {
		t.Fatalf("UpdateApi: %+v %v", updated, err)
	}

	for _, name := range []string{"z", "a"} {
		eventNamespace(t, s, id, name, nil)
	}

	ns, err := c.ListChannelNamespaces(ctx, &appsync.ListChannelNamespacesInput{ApiId: id, MaxResults: 1})
	if err != nil || len(ns.ChannelNamespaces) != 1 || *ns.ChannelNamespaces[0].Name != "a" || ns.NextToken == nil {
		t.Fatalf("ListChannelNamespaces: %+v %v", ns, err)
	}

	ns2, err := c.ListChannelNamespaces(ctx, &appsync.ListChannelNamespacesInput{
		ApiId: id, MaxResults: 1, NextToken: ns.NextToken,
	})
	if err != nil || len(ns2.ChannelNamespaces) != 1 || *ns2.ChannelNamespaces[0].Name != "z" {
		t.Fatalf("namespace pagination: %+v %v", ns2, err)
	}

	namespace, err := c.GetChannelNamespace(ctx, &appsync.GetChannelNamespaceInput{ApiId: id, Name: new("a")})
	if err != nil || namespace.ChannelNamespace.ChannelNamespaceArn == nil {
		t.Fatalf("GetChannelNamespace: %+v %v", namespace, err)
	}

	_, err = c.UpdateChannelNamespace(ctx, &appsync.UpdateChannelNamespaceInput{
		ApiId: id, Name: new("a"), PublishAuthModes: []astypes.AuthMode{{AuthType: astypes.AuthenticationTypeAwsIam}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if status, body := eventHTTP(
		t,
		s,
		a,
		"/a/item",
		[]string{`{"v":1}`},
		map[string]string{"x-api-key": key},
	); status != http.StatusForbidden {
		t.Fatalf("publish override: %d %s", status, body)
	}

	if status, body := eventHTTP(
		t,
		s,
		a,
		"/a/item",
		[]string{`{"v":1}`},
		map[string]string{"Authorization": "sign"},
	); status != http.StatusOK {
		t.Fatalf("IAM publish: %d %s", status, body)
	}

	for _, arn := range []*string{a.ApiArn, namespace.ChannelNamespace.ChannelNamespaceArn} {
		if _, tagErr := c.TagResource(ctx, &appsync.TagResourceInput{
			ResourceArn: arn,
			Tags:        map[string]string{"owner": "me"},
		}); tagErr != nil {
			t.Fatal(tagErr)
		}

		tags, listErr := c.ListTagsForResource(ctx, &appsync.ListTagsForResourceInput{ResourceArn: arn})
		if listErr != nil || tags.Tags["owner"] != "me" {
			t.Fatalf("ListTags: %+v %v", tags, listErr)
		}

		if _, untagErr := c.UntagResource(ctx, &appsync.UntagResourceInput{
			ResourceArn: arn,
			TagKeys:     []string{"owner"},
		}); untagErr != nil {
			t.Fatal(untagErr)
		}
	}

	keys, err := c.ListApiKeys(ctx, &appsync.ListApiKeysInput{ApiId: id})
	if err != nil || len(keys.ApiKeys) != 1 || *keys.ApiKeys[0].Id != key {
		t.Fatalf("ListApiKeys: %+v %v", keys, err)
	}

	if _, err := c.UpdateApiKey(ctx, &appsync.UpdateApiKeyInput{
		ApiId: id, Id: new(key),
		Description: new("updated"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.DeleteApiKey(ctx, &appsync.DeleteApiKeyInput{ApiId: id, Id: new(key)}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.DeleteChannelNamespace(
		ctx,
		&appsync.DeleteChannelNamespaceInput{ApiId: id, Name: new("z")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.DeleteApi(ctx, &appsync.DeleteApiInput{ApiId: id}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.GetChannelNamespace(ctx, &appsync.GetChannelNamespaceInput{ApiId: id, Name: new("a")}); err == nil {
		t.Fatal("namespace survived DeleteApi")
	}

	if _, err := c.DeleteApi(ctx, &appsync.DeleteApiInput{ApiId: b.ApiId}); err != nil {
		t.Fatal(err)
	}
}

const eventHandlers = `import { util } from '@aws-appsync/utils';
export function onPublish(ctx) {
 return ctx.events.filter(e => e.payload.keep).map(e => ({id: e.id,
  payload: {message: e.payload.message.toUpperCase(), channel: ctx.info.channel.path},
  error: e.payload.fail ? 'rejected' : undefined}));
}
export function onSubscribe(ctx) {
 if (ctx.info.channel.segments.includes('private')) util.unauthorized();
}`

func TestAppSyncEventsDelivery(t *testing.T) {
	t.Parallel()
	s := server(t)
	a, key := newEventAPI(t, s, eventConfig(astypes.AuthenticationTypeApiKey))
	eventNamespace(t, s, a.ApiId, "chat", new(eventHandlers))
	auth := map[string]string{"host": a.Dns["HTTP"], "x-api-key": key}
	r := eventDial(t, s, a, auth)
	r.send(map[string]any{"type": "connection_init"})
	if got := r.receive(); !strings.Contains(got, `"connectionTimeoutMs":300000`) {
		t.Fatalf("ack: %s", got)
	}

	r.send(map[string]any{"type": "subscribe", "id": "denied", "channel": "/chat/private", "authorization": auth})
	if got := r.receive(); !strings.Contains(got, `"type":"subscribe_error"`) {
		t.Fatalf("onSubscribe: %s", got)
	}

	for _, channel := range []string{"/chat/*", "/chat/a/b"} {
		r.send(map[string]any{"type": "subscribe", "id": channelID(channel), "channel": channel, "authorization": auth})
		if got := r.receive(); !strings.Contains(got, `"type":"subscribe_success"`) {
			t.Fatalf("subscribe: %s", got)
		}
	}

	r.send(map[string]any{
		"type": "subscribe", "id": channelID("/chat/*"),
		"channel": "/chat/*", "authorization": auth,
	})
	if got := r.receive(); !strings.Contains(got, `"type":"subscribe_error"`) {
		t.Fatalf("duplicate subscription: %s", got)
	}

	events := []string{`{"keep":true,"message":"hello"}`, `{"keep":false}`, `{"keep":true,"message":"bad","fail":true}`}
	status, body := eventHTTP(t, s, a, "/chat/a/b", events, auth)
	if status != http.StatusOK || !strings.Contains(body, `"failed":[{`) || !strings.Contains(body, `"index":0`) {
		t.Fatalf("HTTP publish: %d %s", status, body)
	}

	for range 2 {
		if got := r.receive(); !strings.Contains(got, `HELLO`) || !strings.Contains(got, `"type":"data"`) {
			t.Fatalf("data: %s", got)
		}
	}

	r.send(map[string]any{
		"type": "publish", "id": "p1", "channel": "/chat/a/b",
		"events": []string{`{"keep":true,"message":"socket"}`}, "authorization": auth,
	})
	success, data := 0, 0
	for range 3 {
		got := r.receive()
		if strings.Contains(got, `"type":"publish_success"`) {
			success++
		}

		if strings.Contains(got, `SOCKET`) {
			data++
		}
	}

	if success != 1 || data != 2 {
		t.Fatalf("WebSocket publish: success=%d data=%d", success, data)
	}

	for _, bad := range []struct {
		channel string
		events  []string
	}{
		{"/missing/a", []string{`{}`}},
		{"/chat/*", []string{`{}`}},
		{"/chat/a/b/c/d/e", []string{`{}`}},
		{"/chat/a", []string{"not json"}},
		{"/chat/a", nil},
		{"/chat/a", []string{"0", "0", "0", "0", "0", "0"}},
		{"//chat/a", []string{`{}`}},
		{"/chat/a//", []string{`{}`}},
	} {
		if status, body := eventHTTP(t, s, a, bad.channel, bad.events, auth); status != http.StatusBadRequest {
			t.Fatalf("invalid publish %s: %d %s", bad.channel, status, body)
		}
	}

	if status, body := eventHTTP(
		t,
		s,
		a,
		"/chat/a",
		[]string{`{}`},
		map[string]string{"x-api-key": "wrong"},
	); status != http.StatusForbidden {
		t.Fatalf("invalid key: %d %s", status, body)
	}

	r.send(map[string]any{"type": "unsubscribe", "id": channelID("/chat/*")})
	if got := r.receive(); !strings.Contains(got, `"type":"unsubscribe_success"`) {
		t.Fatalf("unsubscribe: %s", got)
	}

	r.send(map[string]any{"type": "unsubscribe", "id": "unknown"})
	if got := r.receive(); !strings.Contains(got, `"type":"unsubscribe_error"`) {
		t.Fatalf("unknown unsubscribe: %s", got)
	}
}

func channelID(channel string) string {
	return strings.NewReplacer("/", "_", "*", "all").Replace(channel)
}

// eventAuthRuntime verifies the Events-specific authorizer request and returns handler identity.
type eventAuthRuntime struct{ fakeRuntime }

func (r *eventAuthRuntime) Invoke(ctx context.Context, fn *lambda.Function, payload []byte) (lambda.Result, error) {
	if fn.Handler != "app.eventAuthorize" {
		return r.fakeRuntime.Invoke(ctx, fn, payload)
	}

	var event struct {
		Context struct {
			Channel   *string `json:"channel"`
			Namespace *string `json:"channelNamespaceName"`
			Operation string  `json:"operation"`
		} `json:"requestContext"`
		Token string `json:"authorizationToken"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	allowed := event.Token == "allow" && (event.Context.Operation == "EVENT_CONNECT" ||
		(event.Context.Operation == "EVENT_SUBSCRIBE" || event.Context.Operation == "EVENT_PUBLISH") &&
			event.Context.Channel != nil && event.Context.Namespace != nil && *event.Context.Namespace == "news")
	body, err := json.Marshal(map[string]any{
		"isAuthorized":   allowed,
		"handlerContext": map[string]string{"user": "fay"},
	})
	return lambda.Result{Payload: body}, err
}

func TestAppSyncEventsTokenAuthorization(t *testing.T) {
	t.Parallel()
	s := serverWith(t, &eventAuthRuntime{})
	poolID, clientID, token := poolToken(t, s)
	fn := createFunction(t, lambdaClient(s, "test"), "event-authorizer", "app.eventAuthorize")
	for _, tc := range []struct {
		name     string
		provider astypes.AuthProvider
		token    string
	}{
		{"cognito", astypes.AuthProvider{
			AuthType: astypes.AuthenticationTypeAmazonCognitoUserPools,
			CognitoConfig: &astypes.CognitoConfig{
				UserPoolId: new(poolID), AwsRegion: new("us-east-1"),
				AppIdClientRegex: new("^" + clientID + "$"),
			},
		}, token},
		{"oidc", astypes.AuthProvider{
			AuthType:            astypes.AuthenticationTypeOpenidConnect,
			OpenIDConnectConfig: &astypes.OpenIDConnectConfig{Issuer: new(s.URL + "/" + poolID), ClientId: new(clientID)},
		}, token},
		{"lambda", astypes.AuthProvider{
			AuthType:               astypes.AuthenticationTypeAwsLambda,
			LambdaAuthorizerConfig: &astypes.LambdaAuthorizerConfig{AuthorizerUri: fn.FunctionArn},
		}, "allow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := eventConfig(tc.provider.AuthType)
			cfg.AuthProviders = []astypes.AuthProvider{tc.provider}
			a, _ := newEventAPI(t, s, cfg)
			code := `export function onPublish(ctx) { return ctx.events; }`
			if tc.name == "lambda" {
				code = `import { util } from '@aws-appsync/utils';
export function onPublish(ctx) {
 if (ctx.identity.handlerContext.user !== 'fay') util.unauthorized();
 return ctx.events;
}`
			}

			eventNamespace(t, s, a.ApiId, "news", new(code))
			auth := map[string]string{"Authorization": tc.token, "host": a.Dns["HTTP"]}
			if status, body := eventHTTP(
				t,
				s,
				a,
				"/news/a",
				[]string{`{"message":"hi"}`},
				auth,
			); status != http.StatusOK {
				t.Fatalf("publish: %d %s", status, body)
			}

			r := eventDial(t, s, a, auth)
			r.send(map[string]any{"type": "subscribe", "id": "token", "channel": "/news/*", "authorization": auth})
			if got := r.receive(); !strings.Contains(got, `"type":"subscribe_success"`) {
				t.Fatalf("token subscription: %s", got)
			}

			if status, body := eventHTTP(t, s, a, "/news/a", []string{`{}`},
				map[string]string{"Authorization": "invalid"}); status != http.StatusForbidden {
				t.Fatalf("invalid token: %d %s", status, body)
			}
		})
	}
}

func TestAppSyncEventsSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	a, key := newEventAPI(t, s, eventConfig(astypes.AuthenticationTypeApiKey))
	eventNamespace(t, s, a.ApiId, "chat", new(eventHandlers))
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c := appsyncClient(s)
	got, err := c.GetApi(ctx, &appsync.GetApiInput{ApiId: a.ApiId})
	if err != nil || got.Api.Tags["team"] != "events" {
		t.Fatalf("restart API: %+v %v", got, err)
	}

	ns, err := c.GetChannelNamespace(ctx, &appsync.GetChannelNamespaceInput{ApiId: a.ApiId, Name: new("chat")})
	if err != nil || *ns.ChannelNamespace.CodeHandlers != eventHandlers {
		t.Fatalf("restart namespace: %+v %v", ns, err)
	}

	if status, body := eventHTTP(t, s, a, "/chat/a", []string{`{"keep":true,"message":"restart"}`},
		map[string]string{"x-api-key": key}); status != http.StatusOK {
		t.Fatalf("restart publish: %d %s", status, body)
	}
}

func TestAppSyncEventsValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c := appsyncClient(s)
	for _, cfg := range []*astypes.EventConfig{
		nil,
		{},
		{AuthProviders: []astypes.AuthProvider{{AuthType: astypes.AuthenticationTypeApiKey}}},
		eventConfig(astypes.AuthenticationTypeApiKey, astypes.AuthenticationTypeApiKey),
		eventConfig(astypes.AuthenticationTypeAmazonCognitoUserPools),
		eventConfig(astypes.AuthenticationTypeAwsLambda),
	} {
		if _, err := c.CreateApi(ctx, &appsync.CreateApiInput{Name: new("bad"), EventConfig: cfg}); err == nil {
			t.Fatalf("invalid configuration accepted: %+v", cfg)
		}
	}

	missingPool := eventConfig(astypes.AuthenticationTypeAmazonCognitoUserPools)
	missingPool.AuthProviders[0].CognitoConfig = &astypes.CognitoConfig{
		UserPoolId: new("us-east-1_missing"), AwsRegion: new("us-east-1"),
	}
	missingFn := eventConfig(astypes.AuthenticationTypeAwsLambda)
	missingFn.AuthProviders[0].LambdaAuthorizerConfig = &astypes.LambdaAuthorizerConfig{
		AuthorizerUri: new("arn:aws:lambda:us-east-1:000000000000:function:missing"),
	}
	unknownMode := eventConfig(astypes.AuthenticationTypeApiKey)
	unknownMode.DefaultPublishAuthModes = []astypes.AuthMode{{AuthType: astypes.AuthenticationTypeAwsIam}}
	for _, cfg := range []*astypes.EventConfig{missingPool, missingFn, unknownMode} {
		if _, err := c.CreateApi(ctx, &appsync.CreateApiInput{Name: new("bad"), EventConfig: cfg}); err == nil {
			t.Fatal("invalid reference or auth mode accepted")
		}
	}

	a, key := newEventAPI(t, s, eventConfig(astypes.AuthenticationTypeApiKey))
	for _, name := range []string{"bad_name", "-bad", "bad-", strings.Repeat("a", 51)} {
		if _, err := c.CreateChannelNamespace(ctx, &appsync.CreateChannelNamespaceInput{
			ApiId: a.ApiId,
			Name:  new(name),
		}); err == nil {
			t.Fatalf("invalid namespace accepted: %s", name)
		}
	}

	for _, code := range []string{
		"export function onPublish( {", `export const onPublish = {request(){}}`,
		`export function request(ctx) { return ctx; }`,
	} {
		if _, err := c.CreateChannelNamespace(ctx, &appsync.CreateChannelNamespaceInput{
			ApiId: a.ApiId, Name: new("bad"), CodeHandlers: new(code),
		}); err == nil {
			t.Fatalf("invalid/unsupported handler accepted: %s", code)
		}
	}

	if _, err := c.CreateChannelNamespace(ctx, &appsync.CreateChannelNamespaceInput{
		ApiId: a.ApiId, Name: new("bad"), HandlerConfigs: &astypes.HandlerConfigs{},
	}); err == nil {
		t.Fatal("handlerConfigs accepted")
	}

	eventNamespace(t, s, a.ApiId, "ns", nil)
	if _, err := c.CreateChannelNamespace(ctx, &appsync.CreateChannelNamespaceInput{
		ApiId: a.ApiId, Name: new("ns"),
	}); err == nil {
		t.Fatal("duplicate namespace accepted")
	}

	if _, err := c.UpdateChannelNamespace(ctx, &appsync.UpdateChannelNamespaceInput{
		ApiId:              a.ApiId,
		Name:               new("ns"),
		SubscribeAuthModes: []astypes.AuthMode{{AuthType: astypes.AuthenticationTypeAwsIam}},
	}); err == nil {
		t.Fatal("unconfigured override accepted")
	}

	if _, err := c.DeleteApiKey(ctx, &appsync.DeleteApiKeyInput{ApiId: a.ApiId, Id: new(key)}); err != nil {
		t.Fatal(err)
	}

	if status, body := eventHTTP(
		t,
		s,
		a,
		"/ns/a",
		[]string{`{}`},
		map[string]string{"x-api-key": key},
	); status != http.StatusForbidden {
		t.Fatalf("deleted key: %d %s", status, body)
	}
}

func TestAppSyncEventsCloudFormation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	template := `{"Resources":{
 "Api":{"Type":"AWS::AppSync::Api","Properties":{"Name":"events","EventConfig":{
 "AuthProviders":[{"AuthType":"API_KEY"}],"ConnectionAuthModes":[{"AuthType":"API_KEY"}],
 "DefaultPublishAuthModes":[{"AuthType":"API_KEY"}],"DefaultSubscribeAuthModes":[{"AuthType":"API_KEY"}]
 },"Tags":[{"Key":"team","Value":"original"}]}},
 "Namespace":{"Type":"AWS::AppSync::ChannelNamespace","Properties":{
 "ApiId":{"Fn::GetAtt":["Api","ApiId"]},"Name":"news",
 "Tags":[{"Key":"team","Value":"original"}]}}
 },"Outputs":{"Id":{"Value":{"Fn::GetAtt":["Api","ApiId"]}},
 "Arn":{"Value":{"Ref":"Api"}},"Namespace":{"Value":{"Ref":"Namespace"}},
 "Http":{"Value":{"Fn::GetAtt":["Api","Dns.Http"]}}}}`
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("events"), TemplateBody: new(template)})
	if err != nil {
		t.Fatal(err)
	}

	before := wantStatus(ctx, t, c, "events", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&before)
	if outputs["Http"] == "" || !strings.Contains(outputs["Arn"], outputs["Id"]) {
		t.Fatalf("attributes: %+v", outputs)
	}

	updated := strings.ReplaceAll(template, "original", "platform")
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("events"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}

	after := wantStatus(ctx, t, c, "events", cfntypes.StackStatusUpdateComplete)
	if outputsOf(&after)["Id"] != outputs["Id"] || outputsOf(&after)["Namespace"] != outputs["Namespace"] {
		t.Fatal("tag updates replaced resources")
	}

	ac := appsyncClient(s)
	for _, arn := range []string{outputs["Arn"], outputs["Namespace"]} {
		tags, listErr := ac.ListTagsForResource(ctx, &appsync.ListTagsForResourceInput{ResourceArn: new(arn)})
		if listErr != nil || tags.Tags["team"] != "platform" {
			t.Fatalf("updated tags: %+v %v", tags, listErr)
		}
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("events")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "event stack deletion", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("events")})
		return describeErr != nil
	})
	if _, err := ac.GetApi(ctx, &appsync.GetApiInput{ApiId: new(outputs["Id"])}); err == nil {
		t.Fatal("API survived stack deletion")
	}
}

func TestAppSyncEventsVerifiedIAM(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	a, _ := newEventAPI(t, s, eventConfig(astypes.AuthenticationTypeAwsIam))
	eventNamespace(t, s, a.ApiId, "news", nil)
	if status, body := eventHTTP(t, s, a, "/news/a", []string{`{}`},
		map[string]string{"Authorization": "sign"}); status != http.StatusOK {
		t.Fatalf("verified publish: %d %s", status, body)
	}

	if status, body := eventHTTP(t, s, a, "/news/a", []string{`{}`}, map[string]string{
		"Authorization": "AWS4-HMAC-SHA256 Credential=test/20261024/us-east-1/appsync/aws4_request, " +
			"SignedHeaders=host, Signature=bad",
	}); status == http.StatusOK {
		t.Fatalf("bad signature accepted: %d %s", status, body)
	}

	auth := signedHeader(t, a.Dns["HTTP"], "/event", "{}")
	r := eventDial(t, s, a, auth)
	body := `{"channel":"/news/*"}`
	r.send(map[string]any{
		"type": "subscribe", "id": "iam", "channel": "/news/*",
		"authorization": signedHeader(t, a.Dns["HTTP"], "/event", body),
	})
	if got := r.receive(); !strings.Contains(got, `"type":"subscribe_success"`) {
		t.Fatalf("verified subscribe: %s", got)
	}

	r.send(map[string]any{
		"type": "publish", "id": "iam-publish", "channel": "/news/a", "events": []string{`{}`},
		"authorization": signedHeader(t, a.Dns["HTTP"], "/event", `{"channel":"/news/a","events":["{}"]}`),
	})
	for range 2 {
		if got := r.receive(); strings.Contains(got, `"errors"`) {
			t.Fatalf("verified websocket publish: %s", got)
		}
	}
}

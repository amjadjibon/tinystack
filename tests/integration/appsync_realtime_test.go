package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/appsync"
	astypes "github.com/aws/aws-sdk-go-v2/service/appsync/types"
	"github.com/coder/websocket"

	"github.com/amjadjibon/tinystack/internal/config"
)

const realtimeSchema = `
type Message @aws_api_key @aws_iam { id: ID! room: String text: String severity: Int secret: String @aws_iam }

type Query { message(id: ID!): Message }

type Mutation @aws_api_key @aws_iam {
  send(id: ID!, room: String, text: String, severity: Int, secret: String): Message
}

type Subscription {
  onSend(room: String): Message @aws_subscribe(mutations: ["send"])
  onSevere: Message @aws_subscribe(mutations: ["send"])
  onSecret: Message @aws_subscribe(mutations: ["send"]) @aws_iam
}
`

// realtimeAPI creates an API-key API, with IAM, whose send mutation returns
// its arguments and whose onSevere subscription filters on severity.
func realtimeAPI(t *testing.T, s *httptest.Server) (a *astypes.GraphqlApi, key string) {
	t.Helper()
	ctx := context.Background()
	c := appsyncClient(s)
	created, err := c.CreateGraphqlApi(ctx, &appsync.CreateGraphqlApiInput{
		Name: aws.String("chat"), AuthenticationType: astypes.AuthenticationTypeApiKey,
		AdditionalAuthenticationProviders: []astypes.AdditionalAuthenticationProvider{
			{AuthenticationType: astypes.AuthenticationTypeAwsIam},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.GraphqlApi.ApiId
	if _, err = c.StartSchemaCreation(ctx, &appsync.StartSchemaCreationInput{
		ApiId: id, Definition: []byte(realtimeSchema),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateDataSource(ctx, &appsync.CreateDataSourceInput{
		ApiId: id, Name: aws.String("none"), Type: astypes.DataSourceTypeNone,
	}); err != nil {
		t.Fatal(err)
	}

	runtime := &astypes.AppSyncRuntime{Name: astypes.RuntimeNameAppsyncJs, RuntimeVersion: aws.String("1.0.0")}
	resolvers := map[[2]string]string{
		{"Mutation", "send"}: `export const request = (ctx) => ({ payload: ctx.args });
export const response = (ctx) => ctx.result;`,
		{"Subscription", "onSevere"}: `import { util, extensions } from '@aws-appsync/utils';
export function request(ctx) {
  extensions.setSubscriptionFilter(util.transform.toSubscriptionFilter({ severity: { ge: 5 } }));
  return { payload: null };
}
export const response = () => null;`,
	}
	for field, code := range resolvers {
		if _, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
			ApiId: id, TypeName: aws.String(field[0]), FieldName: aws.String(field[1]),
			DataSourceName: aws.String("none"), Runtime: runtime, Code: aws.String(code),
		}); err != nil {
			t.Fatal(err)
		}
	}

	k, err := c.CreateApiKey(ctx, &appsync.CreateApiKeyInput{ApiId: id})
	if err != nil {
		t.Fatal(err)
	}

	return created.GraphqlApi, *k.ApiKey.Id
}

// realtime is a client of an API's real-time endpoint.
type realtime struct {
	t    *testing.T
	conn *websocket.Conn
}

// dial connects to url, sending host as the Host header when it is set and
// header, the connection's authorization, as the header query parameter.
func dial(t *testing.T, url, host string, header map[string]string) *realtime {
	t.Helper()
	b, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url+"?header="+base64.StdEncoding.EncodeToString(b)+"&payload=e30=",
		&websocket.DialOptions{Subprotocols: []string{"graphql-ws"}, Host: host})
	if err != nil {
		t.Fatal(err)
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	if conn.Subprotocol() != "graphql-ws" {
		t.Fatalf("subprotocol = %q", conn.Subprotocol())
	}

	t.Cleanup(func() { _ = conn.CloseNow() })
	return &realtime{t: t, conn: conn}
}

func (r *realtime) send(m map[string]any) {
	r.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		r.t.Fatal(err)
	}

	if err = r.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		r.t.Fatal(err)
	}
}

// receive returns the next message other than a keep-alive, as JSON.
func (r *realtime) receive() string {
	r.t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, b, err := r.conn.Read(ctx)
		cancel()
		if err != nil {
			r.t.Fatal(err)
		}

		if string(b) != `{"type":"ka"}` {
			return string(b)
		}
	}
}

// start starts subscription q as id, authorized with header.
func (r *realtime) start(id, q string, header map[string]string) {
	r.t.Helper()
	data, err := json.Marshal(map[string]any{"query": q, "variables": map[string]any{}})
	if err != nil {
		r.t.Fatal(err)
	}

	if header == nil {
		r.t.Fatal("no authorization")
	}

	if strings.HasPrefix(header["Authorization"], "sign:") {
		header = signedHeader(r.t, header["host"], "/graphql", string(data))
	}

	r.send(map[string]any{
		"id": id, "type": "start",
		"payload": map[string]any{"data": string(data), "extensions": map[string]any{"authorization": header}},
	})
}

// signedHeader signs a POST of body to http://host<path> as access key test,
// returning the headers as the real-time protocol carries them.
func signedHeader(t *testing.T, host, path, body string) map[string]string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+host+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Accept", "application/json, text/javascript")
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	sum := sha256.Sum256([]byte(body))
	if err = v4.NewSigner().
		SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"},
			req, hex.EncodeToString(sum[:]), "appsync", "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	out := map[string]string{"host": host}
	for k := range req.Header {
		out[k] = req.Header.Get(k)
	}

	return out
}

func TestAppSyncRealtimeSubscriptions(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	a, key := realtimeAPI(t, s)
	id := *a.ApiId
	if uri := a.Uris["REALTIME"]; !strings.HasPrefix(uri, "ws://"+id+".appsync-realtime-api.localhost:") ||
		!strings.HasSuffix(uri, "/graphql") {
		t.Fatalf("REALTIME uri = %q", uri)
	}

	ws := "ws" + strings.TrimPrefix(s.URL, "http")
	auth := map[string]string{"host": strings.TrimPrefix(s.URL, "http://"), "x-api-key": key}
	r := dial(t, ws+"/_aws/appsync-api/"+id+"/graphql/realtime", "", auth)
	r.send(map[string]any{"type": "start", "id": "early"})
	if got := r.receive(); !strings.Contains(got, `"errorType":"UnsupportedOperation"`) {
		t.Fatalf("start before connection_init = %s", got)
	}

	r.send(map[string]any{"type": "connection_init"})
	if got := r.receive(); got != `{"payload":{"connectionTimeoutMs":300000},"type":"connection_ack"}` {
		t.Fatalf("connection_init = %s", got)
	}

	r.start("room-a", `subscription { onSend(room: "a") { id text } }`, auth)
	r.start("severe", `subscription Severe { alias: onSevere { id severity __typename } }`, auth)
	for _, want := range []string{"room-a", "severe"} {
		if got := r.receive(); got != `{"id":"`+want+`","type":"start_ack"}` {
			t.Fatalf("start %s = %s", want, got)
		}
	}

	// onSecret allows IAM only, and the endpoint runs subscriptions only.
	r.start("secret", `subscription { onSecret { id } }`, auth)
	if got := r.receive(); !strings.Contains(got, `"id":"secret","type":"error"`) ||
		!strings.Contains(got, `"errorType":"Unauthorized"`) {
		t.Fatalf("unauthorized start = %s", got)
	}

	r.start("query", `{ message(id: "1") { id } }`, auth)
	if got := r.receive(); !strings.Contains(got, `"errorType":"UnsupportedOperation"`) {
		t.Fatalf("query start = %s", got)
	}

	r.start("bad-key", `subscription { onSevere { id } }`, map[string]string{"x-api-key": "da2-nope"})
	if got := r.receive(); !strings.Contains(got, `"errorType":"UnauthorizedException"`) {
		t.Fatalf("bad key start = %s", got)
	}

	// A mutation reaches the subscriptions whose arguments and filters it
	// passes, as their selection sets ask; text, which the mutation does not
	// select, is null.
	mutate := func(vars map[string]any, header http.Header, selection string) {
		t.Helper()
		got := graphql(t, s, id, `mutation($id: ID!, $room: String, $severity: Int, $secret: String) {
  sent: send(id: $id, room: $room, severity: $severity, secret: $secret) { `+selection+` }
}`, vars, header)
		if len(got.Errors) != 0 {
			t.Fatalf("mutation = %s %+v", got.Data, got.Errors)
		}
	}
	mutate(map[string]any{"id": "1", "room": "a", "severity": 1}, http.Header{"X-Api-Key": {key}}, "id room severity")
	if got := r.receive(); got != `{"payload":{"data":{"onSend":{"id":"1","text":null}}},"id":"room-a","type":"data"}` {
		t.Fatalf("room a = %s", got)
	}

	mutate(map[string]any{"id": "2", "room": "b", "severity": 9}, http.Header{"X-Api-Key": {key}}, "id room severity")
	want := `{"payload":{"data":{"alias":{"id":"2","severity":9,"__typename":"Message"}}},"id":"severe","type":"data"}`
	if got := r.receive(); got != want {
		t.Fatalf("severe = %s", got)
	}

	// After stop, only the other subscription hears room a.
	r.send(map[string]any{"type": "stop", "id": "room-a"})
	if got := r.receive(); got != `{"id":"room-a","type":"complete"}` {
		t.Fatalf("stop = %s", got)
	}

	mutate(map[string]any{"id": "3", "room": "a", "severity": 7}, http.Header{"X-Api-Key": {key}}, "id room severity")
	if got := r.receive(); !strings.Contains(got, `"id":"severe"`) || !strings.Contains(got, `"id":"3"`) {
		t.Fatalf("after stop = %s", got)
	}

	// Over HTTP, a subscription points at the real-time endpoint.
	got := graphql(t, s, id, `subscription { onSevere { id } }`, nil, http.Header{"X-Api-Key": {key}})
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0].Message, "real-time endpoint") {
		t.Fatalf("HTTP subscription = %s %+v", got.Data, got.Errors)
	}

	// IAM: the connection and start are signed requests on the GraphQL host,
	// here reached through <API ID>.appsync-realtime-api.<host>.
	apiHost := id + ".appsync-api.localhost"
	iam := dial(t, ws+"/graphql", id+".appsync-realtime-api.localhost",
		signedHeader(t, apiHost, "/graphql/connect", "{}"))
	iam.send(map[string]any{"type": "connection_init"})
	if got := iam.receive(); !strings.Contains(got, `"type":"connection_ack"`) {
		t.Fatalf("IAM connection_init = %s", got)
	}

	iam.start("secret", `subscription { onSecret { id secret } }`, map[string]string{
		"Authorization": "sign:", "host": apiHost,
	})
	if got := iam.receive(); got != `{"id":"secret","type":"start_ack"}` {
		t.Fatalf("IAM start = %s", got)
	}

	mutate(map[string]any{"id": "4", "secret": "s3"}, http.Header{"Authorization": {"sign:test"}}, "id secret")
	want = `{"payload":{"data":{"onSecret":{"id":"4","secret":"s3"}}},"id":"secret","type":"data"}`
	if got := iam.receive(); got != want {
		t.Fatalf("IAM data = %s", got)
	}

	// A connection whose header does not authorize it is refused.
	bad := dial(t, ws+"/_aws/appsync-api/"+id+"/graphql/realtime", "",
		map[string]string{"host": auth["host"], "x-api-key": "da2-nope"})
	bad.send(map[string]any{"type": "connection_init"})
	if got := bad.receive(); !strings.Contains(got, `"type":"connection_error"`) ||
		!strings.Contains(got, "UnauthorizedException") {
		t.Fatalf("bad connection = %s", got)
	}

	unsigned := dial(t, ws+"/graphql", id+".appsync-realtime-api.localhost",
		map[string]string{"host": apiHost, "Authorization": "AWS4-HMAC-SHA256 " +
			"Credential=test/20200101/us-east-1/appsync/aws4_request, SignedHeaders=host, Signature=00"})
	unsigned.send(map[string]any{"type": "connection_init"})
	if got := unsigned.receive(); !strings.Contains(got, `"type":"connection_error"`) {
		t.Fatalf("bad signature = %s", got)
	}
}

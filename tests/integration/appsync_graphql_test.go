package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/appsync"
	astypes "github.com/aws/aws-sdk-go-v2/service/appsync/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// graphqlResolver is a direct Lambda resolver: it answers by the field it
// resolves, and Query.echo returns the event it received.
func graphqlResolver(payload []byte) (lambda.Result, error) {
	var event struct {
		Arguments map[string]any `json:"arguments"`
		Info      struct {
			FieldName string `json:"fieldName"`
		} `json:"info"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	var out any
	switch event.Info.FieldName {
	case "post":
		id, _ := event.Arguments["id"].(string)
		out = map[string]any{"id": id, "title": "Post " + id, "views": 3, "meta": map[string]any{"a": 1}, "secret": "s"}
	case "posts":
		out = []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}}
	case "failing":
		return lambda.Result{Payload: []byte(`{"errorType":"ValueError","errorMessage":"boom"}`), Error: true}, nil
	case "echo":
		return lambda.Result{Payload: payload}, nil
	}

	body, err := json.Marshal(out)
	return lambda.Result{Payload: body}, err
}

// graphqlAuthorizer is an AppSync Lambda authorizer: only "let-me-in"
// passes, and it may not read Post.secret.
func graphqlAuthorizer(payload []byte) (lambda.Result, error) {
	var event struct {
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	body, err := json.Marshal(map[string]any{
		"isAuthorized": event.AuthorizationToken == "let-me-in", "resolverContext": map[string]any{"user": "fay"},
		"deniedFields": []string{"Post.secret"},
	})
	return lambda.Result{Payload: body}, err
}

const graphqlSchema = `
type Post @aws_api_key @aws_lambda {
  id: ID!
  title: String
  views: Int
  meta: AWSJSON
  secret: String
}

type Query {
  post(id: ID!): Post @aws_api_key @aws_lambda @aws_iam
  posts: [Post!]!
  failing: String
  broken: String!
  echo(msg: String): AWSJSON
}
`

type graphqlAnswer struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		ErrorType string `json:"errorType"`
		Message   string `json:"message"`
		Path      []any  `json:"path"`
	} `json:"errors"`
	status int
}

// graphqlAPI creates an API-key API with graphqlSchema, a Lambda authorizer,
// and IAM, whose fields resolve through graphqlResolver, and returns its ID
// and a key.
func graphqlAPI(t *testing.T, s *httptest.Server) (id, key string) {
	t.Helper()
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	resolverFn := createFunction(t, functions, "resolver", "app.graphql")
	authorizerFn := createFunction(t, functions, "authorizer", "app.graphqlAuthorize")
	c := appsyncClient(s)
	created, err := c.CreateGraphqlApi(ctx, &appsync.CreateGraphqlApiInput{
		Name: aws.String("blog"), AuthenticationType: astypes.AuthenticationTypeApiKey,
		AdditionalAuthenticationProviders: []astypes.AdditionalAuthenticationProvider{
			{
				AuthenticationType:     astypes.AuthenticationTypeAwsLambda,
				LambdaAuthorizerConfig: &astypes.LambdaAuthorizerConfig{AuthorizerUri: authorizerFn.FunctionArn},
			},
			{AuthenticationType: astypes.AuthenticationTypeAwsIam},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	id = *created.GraphqlApi.ApiId
	if _, err = c.StartSchemaCreation(ctx, &appsync.StartSchemaCreationInput{
		ApiId: aws.String(id), Definition: []byte(graphqlSchema),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateDataSource(ctx, &appsync.CreateDataSourceInput{
		ApiId: aws.String(id), Name: aws.String("fn"), Type: astypes.DataSourceTypeAwsLambda,
		LambdaConfig: &astypes.LambdaDataSourceConfig{LambdaFunctionArn: resolverFn.FunctionArn},
	}); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"post", "posts", "failing", "echo"} {
		if _, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
			ApiId: aws.String(id), TypeName: aws.String("Query"), FieldName: aws.String(field),
			DataSourceName: aws.String("fn"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	k, err := c.CreateApiKey(ctx, &appsync.CreateApiKeyInput{ApiId: aws.String(id)})
	if err != nil {
		t.Fatal(err)
	}

	return id, *k.ApiKey.Id
}

// graphql posts a query to the API's endpoint with the given headers.
func graphql(t *testing.T, s *httptest.Server, id, q string, vars map[string]any, header http.Header) graphqlAnswer {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, s.URL+"/_aws/appsync-api/"+id+"/graphql", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	if req.Header.Get("Authorization") == "sign" {
		req.Header.Del("Authorization")
		sum := sha256.Sum256(body)
		creds := aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
		if err = v4.NewSigner().SignHTTP(context.Background(), creds, req, hex.EncodeToString(sum[:]), "appsync",
			"us-east-1", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	answer := graphqlAnswer{status: resp.StatusCode}
	if err = json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}

	return answer
}

func TestAppSyncGraphQLQueries(t *testing.T) {
	s := server(t)
	id, key := graphqlAPI(t, s)
	withKey := http.Header{"X-Api-Key": {key}}

	got := graphql(t, s, id, `query Q($id: ID!) {
  post(id: $id) { id title views meta ... on Post { __typename } }
  list: posts { id }
}`, map[string]any{"id": "7"}, withKey)
	want := `{"post":{"id":"7","title":"Post 7","views":3,"meta":"{\"a\":1}","__typename":"Post"},` +
		`"list":[{"id":"1"},{"id":"2"}]}`
	if got.status != http.StatusOK || string(got.Data) != want || len(got.Errors) != 0 {
		t.Fatalf("query = %d %s %+v", got.status, got.Data, got.Errors)
	}

	got = graphql(t, s, id, `{ failing post(id: "1") { id } }`, nil, withKey)
	if string(got.Data) != `{"failing":null,"post":{"id":"1"}}` || len(got.Errors) != 1 ||
		got.Errors[0].ErrorType != "Lambda:Unhandled" || got.Errors[0].Message != "boom" {
		t.Fatalf("failing = %s %+v", got.Data, got.Errors)
	}

	// A null non-null root field makes the whole result null.
	got = graphql(t, s, id, `{ broken post(id: "1") { id } }`, nil, withKey)
	if string(got.Data) != "null" || len(got.Errors) != 1 || !strings.Contains(got.Errors[0].Message, "broken") {
		t.Fatalf("broken = %s %+v", got.Data, got.Errors)
	}

	got = graphql(t, s, id, `{ nope }`, nil, withKey)
	if string(got.Data) != "null" || len(got.Errors) != 1 || got.Errors[0].ErrorType != "ValidationError" {
		t.Fatalf("invalid query = %s %+v", got.Data, got.Errors)
	}

	got = graphql(t, s, id, `{ echo(msg: "hi") }`, nil, withKey)
	var echoed string
	if err := json.Unmarshal(got.Data, &struct {
		Echo *string `json:"echo"`
	}{&echoed}); err != nil {
		t.Fatal(err)
	}

	var event struct {
		Arguments map[string]any `json:"arguments"`
		Identity  any            `json:"identity"`
		Request   struct {
			Headers map[string]string `json:"headers"`
		} `json:"request"`
		Info struct {
			ParentTypeName string `json:"parentTypeName"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(echoed), &event); err != nil || event.Arguments["msg"] != "hi" ||
		event.Identity != nil || event.Info.ParentTypeName != "Query" || event.Request.Headers["x-api-key"] != key {
		t.Fatalf("echo event = %s, %v", echoed, err)
	}

	got = graphql(t, s, id, `{ __schema { queryType { name } } __type(name: "Post") { fields { name } } }`, nil,
		withKey)
	if !strings.Contains(string(got.Data), `"queryType":{"name":"Query"}`) ||
		!strings.Contains(string(got.Data), `{"name":"secret"}`) {
		t.Fatalf("introspection = %s %+v", got.Data, got.Errors)
	}

	schema, err := appsyncClient(s).GetIntrospectionSchema(context.Background(), &appsync.GetIntrospectionSchemaInput{
		ApiId: aws.String(id), Format: astypes.OutputTypeJson,
	})
	if err != nil || !strings.Contains(string(schema.Schema), `"__schema"`) ||
		!strings.Contains(string(schema.Schema), `"AWSJSON"`) {
		t.Fatalf("GetIntrospectionSchema JSON = %.200s, %v", schema.Schema, err)
	}
}

func TestAppSyncGraphQLAuthorization(t *testing.T) {
	s := server(t)
	id, key := graphqlAPI(t, s)

	for _, header := range []http.Header{{}, {"X-Api-Key": {"da2-wrong"}}, {"Authorization": {"no"}}} {
		if got := graphql(t, s, id, `{ post(id: "1") { id } }`, nil, header); got.status != http.StatusUnauthorized ||
			got.Errors[0].ErrorType != "UnauthorizedException" {
			t.Fatalf("%v = %d %+v", header, got.status, got.Errors)
		}
	}

	// The Lambda authorizer passes the token, but only fields that name
	// @aws_lambda are allowed, and Post.secret is denied.
	got := graphql(t, s, id, `{ post(id: "1") { id secret } posts { id } }`, nil,
		http.Header{"Authorization": {"let-me-in"}})
	if string(got.Data) != "null" || len(got.Errors) != 2 || got.Errors[0].ErrorType != "Unauthorized" ||
		got.Errors[1].ErrorType != "Unauthorized" {
		t.Fatalf("lambda = %s %+v", got.Data, got.Errors)
	}

	got = graphql(t, s, id, `{ post(id: "1") { id secret } }`, nil, http.Header{"Authorization": {"let-me-in"}})
	if string(got.Data) != `{"post":{"id":"1","secret":null}}` {
		t.Fatalf("lambda secret = %s %+v", got.Data, got.Errors)
	}

	// IAM may read Query.post, but Post names only API keys and Lambda.
	got = graphql(t, s, id, `{ post(id: "1") { id } }`, nil, http.Header{"Authorization": {"sign"}})
	if string(got.Data) != `{"post":null}` || len(got.Errors) != 1 || got.Errors[0].ErrorType != "Unauthorized" {
		t.Fatalf("iam = %d %s %+v", got.status, got.Data, got.Errors)
	}

	got = graphql(t, s, id, `{ post(id: "1") { id } }`, nil, http.Header{"X-Api-Key": {key}})
	if string(got.Data) != `{"post":{"id":"1"}}` {
		t.Fatalf("api key = %s %+v", got.Data, got.Errors)
	}
}

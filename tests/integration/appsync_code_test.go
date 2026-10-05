package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appsync"
	astypes "github.com/aws/aws-sdk-go-v2/service/appsync/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const jsSchema = `
type Note { id: ID! text: String count: Int }
type NotePage { items: [Note] nextToken: String }

type Query {
  hello(name: String): String
  note(id: ID!): Note
  notes(limit: Int, nextToken: String): NotePage
  shout(text: String): String
  early: String
  oops: String
  warn: String
  fromLambda(id: ID!): AWSJSON
  backend: AWSJSON
  publish: AWSJSON
}

type Mutation {
  addNote(id: ID!, text: String): Note
  bump(id: ID!): Note
  removeNote(id: ID!): Note
  guarded(id: ID!): Note
}
`

// jsCode is an APPSYNC_JS module with the given request and response bodies.
func jsCode(request, response string) *string {
	return aws.String(`import { util, runtime } from '@aws-appsync/utils';
import * as ddb from '@aws-appsync/utils/dynamodb';

export function request(ctx) {
  ` + request + `
}

export function response(ctx) {
  ` + response + `
}
`)
}

func TestAppSyncJavaScriptResolvers(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `","q":"` + r.URL.Query().Get("q") + `"}`))
	}))
	t.Cleanup(backend.Close)

	if _, err := client(s, "test", "us-east-1").CreateTable(ctx, &sdk.CreateTableInput{
		TableName: aws.String("notes"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema:   []types.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash}},
		BillingMode: types.BillingModePayPerRequest,
	}); err != nil {
		t.Fatal(err)
	}

	fn := createFunction(t, lambdaClient(s, "test"), "js-resolver", "app.graphql")
	c := appsyncClient(s)
	created, err := c.CreateGraphqlApi(ctx, &appsync.CreateGraphqlApiInput{
		Name: aws.String("js"), AuthenticationType: astypes.AuthenticationTypeApiKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.GraphqlApi.ApiId
	if _, err = c.StartSchemaCreation(
		ctx,
		&appsync.StartSchemaCreationInput{ApiId: id, Definition: []byte(jsSchema)},
	); err != nil {
		t.Fatal(err)
	}

	sources := []*appsync.CreateDataSourceInput{
		{Name: aws.String("none"), Type: astypes.DataSourceTypeNone},
		{
			Name: aws.String("table"),
			Type: astypes.DataSourceTypeAmazonDynamodb,
			DynamodbConfig: &astypes.DynamodbDataSourceConfig{
				TableName: aws.String("notes"),
				AwsRegion: aws.String("us-east-1"),
			},
		},
		{
			Name: aws.String("fn"), Type: astypes.DataSourceTypeAwsLambda,
			LambdaConfig: &astypes.LambdaDataSourceConfig{LambdaFunctionArn: fn.FunctionArn},
		},
		{
			Name: aws.String("web"), Type: astypes.DataSourceTypeHttp,
			HttpConfig: &astypes.HttpDataSourceConfig{Endpoint: aws.String(backend.URL)},
		},
		{
			Name: aws.String("bus"), Type: astypes.DataSourceTypeAmazonEventbridge,
			EventBridgeConfig: &astypes.EventBridgeDataSourceConfig{
				EventBusArn: aws.String("arn:aws:events:us-east-1:000000000000:event-bus/default"),
			},
		},
	}
	for _, in := range sources {
		in.ApiId = id
		if _, err = c.CreateDataSource(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	runtime := &astypes.AppSyncRuntime{Name: astypes.RuntimeNameAppsyncJs, RuntimeVersion: aws.String("1.0.0")}
	unit := func(typeName, field, source string, code *string) {
		t.Helper()
		if _, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
			ApiId: id, TypeName: aws.String(typeName), FieldName: aws.String(field), DataSourceName: aws.String(source),
			Runtime: runtime, Code: code,
		}); err != nil {
			t.Fatal(err)
		}
	}

	unit("Query", "hello", "none", jsCode("return { payload: `Hello ${ctx.args.name}` };", "return ctx.result;"))
	unit("Query", "note", "table", jsCode("return ddb.get({ key: { id: ctx.args.id } });", "return ctx.result;"))
	unit("Query", "notes", "table", jsCode("return ddb.scan({ limit: ctx.args.limit, nextToken: ctx.args.nextToken });",
		"return ctx.result;"))
	unit("Query", "early", "none", jsCode("runtime.earlyReturn('short'); return {};", "return 'long';"))
	unit("Query", "oops", "none", jsCode("util.error('bad thing', 'Custom', null, { why: 1 });", "return null;"))
	unit("Query", "warn", "none", jsCode("util.appendError('careful', 'Warning'); return { payload: 'ok' };",
		"return ctx.result;"))
	unit("Query", "fromLambda", "fn", jsCode(
		"return { operation: 'Invoke', payload: { info: { fieldName: 'post' }, arguments: { id: ctx.args.id } } };",
		"if (ctx.error) util.error(ctx.error.message, ctx.error.type); return ctx.result;"))
	unit("Query", "backend", "web", jsCode(
		"return { method: 'GET', resourcePath: '/items', params: { query: { q: 'x' } } };",
		"return JSON.parse(ctx.result.body);"))
	unit("Query", "publish", "bus", jsCode(
		"return { operation: 'PutEvents', events: [{ source: 'notes', detailType: 'Created', detail: { id: 1 } }] };",
		"return ctx.result;"))
	unit("Mutation", "addNote", "table", jsCode(
		"return ddb.put({ key: { id: ctx.args.id }, item: { text: ctx.args.text, count: 0 } });", "return ctx.result;"))
	unit("Mutation", "bump", "table", jsCode(
		"return ddb.update({ key: { id: ctx.args.id }, update: { count: ddb.operations.increment(1) } });",
		"return ctx.result;"))
	unit(
		"Mutation",
		"removeNote",
		"table",
		jsCode("return ddb.remove({ key: { id: ctx.args.id } });", "return ctx.result;"),
	)
	unit("Mutation", "guarded", "table", jsCode(
		"return ddb.put({ key: { id: ctx.args.id }, item: {}, condition: { id: { attributeExists: false } } });",
		"if (ctx.error) util.error(ctx.error.message, ctx.error.type); return ctx.result;"))

	// A pipeline: the resolver stashes a suffix, one function upper-cases
	// through NONE, and the next adds the suffix.
	var functions []string
	for _, code := range []*string{
		jsCode("return { payload: ctx.prev.result.text.toUpperCase() };", "return ctx.result;"),
		jsCode("return { payload: ctx.prev.result + ctx.stash.suffix };", "return ctx.result;"),
	} {
		f, fnErr := c.CreateFunction(ctx, &appsync.CreateFunctionInput{
			ApiId: id, Name: aws.String("step"), DataSourceName: aws.String("none"), Runtime: runtime, Code: code,
		})
		if fnErr != nil {
			t.Fatal(fnErr)
		}

		functions = append(functions, *f.FunctionConfiguration.FunctionId)
	}

	if _, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
		ApiId: id, TypeName: aws.String("Query"), FieldName: aws.String("shout"), Kind: astypes.ResolverKindPipeline,
		Runtime: runtime, PipelineConfig: &astypes.PipelineConfig{Functions: functions},
		Code: jsCode("ctx.stash.suffix = '!'; return { text: ctx.args.text };", "return ctx.prev.result;"),
	}); err != nil {
		t.Fatal(err)
	}

	k, err := c.CreateApiKey(ctx, &appsync.CreateApiKeyInput{ApiId: id})
	if err != nil {
		t.Fatal(err)
	}

	key := http.Header{"X-Api-Key": {*k.ApiKey.Id}}
	run := func(q string) graphqlAnswer {
		t.Helper()
		return graphql(t, s, *id, q, nil, key)
	}

	expect := func(q, want string) {
		t.Helper()
		if got := run(q); string(got.Data) != want || len(got.Errors) != 0 {
			t.Fatalf("%s = %s %+v, want %s", q, got.Data, got.Errors, want)
		}
	}

	expect(`{ hello(name: "fay") shout(text: "hi") early }`, `{"hello":"Hello fay","shout":"HI!","early":"short"}`)
	expect(`mutation { addNote(id: "a", text: "first") { id text count } }`,
		`{"addNote":{"id":"a","text":"first","count":0}}`)
	expect(`mutation { b: addNote(id: "b", text: "second") { id } }`, `{"b":{"id":"b"}}`)
	expect(`mutation { bump(id: "a") { count } }`, `{"bump":{"count":1}}`)
	expect(`{ note(id: "a") { id text count } }`, `{"note":{"id":"a","text":"first","count":1}}`)

	got := run(`{ notes(limit: 1) { items { id } nextToken } }`)
	var page struct {
		Notes struct {
			NextToken *string `json:"nextToken"`
			Items     []struct {
				ID string `json:"id"`
			} `json:"items"`
		} `json:"notes"`
	}
	if err = json.Unmarshal(got.Data, &page); err != nil || len(page.Notes.Items) != 1 || page.Notes.NextToken == nil {
		t.Fatalf("notes = %s %+v", got.Data, got.Errors)
	}

	got = run(`{ notes(limit: 5, nextToken: "` + *page.Notes.NextToken + `") { items { id } } }`)
	if !strings.Contains(string(got.Data), `"items":[{"id":"`) || strings.Count(string(got.Data), `"id"`) != 1 {
		t.Fatalf("second page = %s %+v", got.Data, got.Errors)
	}

	got = run(`mutation { guarded(id: "a") { id } }`)
	if string(got.Data) != `{"guarded":null}` || len(got.Errors) != 1 ||
		got.Errors[0].ErrorType != "DynamoDB:ConditionalCheckFailedException" {
		t.Fatalf("guarded = %s %+v", got.Data, got.Errors)
	}

	expect(`mutation { removeNote(id: "a") { id text } }`, `{"removeNote":{"id":"a","text":"first"}}`)
	expect(`{ note(id: "a") { id } }`, `{"note":null}`)

	got = run(`{ oops warn }`)
	if string(got.Data) != `{"oops":null,"warn":"ok"}` || len(got.Errors) != 2 ||
		got.Errors[0].ErrorType != "Custom" || got.Errors[0].Message != "bad thing" ||
		got.Errors[1].ErrorType != "Warning" {
		t.Fatalf("errors = %s %+v", got.Data, got.Errors)
	}

	got = run(`{ fromLambda(id: "9") backend publish }`)
	var out struct {
		FromLambda string `json:"fromLambda"`
		Backend    string `json:"backend"`
		Publish    string `json:"publish"`
	}
	if err = json.Unmarshal(got.Data, &out); err != nil || len(got.Errors) != 0 ||
		!strings.Contains(out.FromLambda, `"title":"Post 9"`) || out.Backend != `{"path":"/items","q":"x"}` ||
		!strings.Contains(out.Publish, `"FailedEntryCount":0`) {
		t.Fatalf("sources = %s %+v", got.Data, got.Errors)
	}
}

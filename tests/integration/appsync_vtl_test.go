package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appsync"
	astypes "github.com/aws/aws-sdk-go-v2/service/appsync/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// graphqlBatch is a batching resolver: for a batch of events or payloads,
// each naming a post by source.id or id, it returns each post's author with
// the size of the batch it came in.
func graphqlBatch(payload []byte) (lambda.Result, error) {
	var batch []struct {
		Source struct {
			ID string `json:"id"`
		} `json:"source"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &batch); err != nil {
		return lambda.Result{}, err
	}

	out := make([]any, len(batch))
	for i, e := range batch {
		id := e.ID
		if id == "" {
			id = e.Source.ID
		}

		out[i] = map[string]any{"name": "author-" + id, "batchSize": len(batch)}
	}

	body, err := json.Marshal(out)
	return lambda.Result{Payload: body}, err
}

const vtlSchema = `
type Author { name: String batchSize: Int }
type Post { id: ID! author: Author vtlAuthor: Author jsAuthor: Author }
type Note { id: ID! text: String }

type Query {
  posts: [Post]
  hello(name: String): AWSJSON
  guarded: String
  note(id: ID!): Note
  notes(ids: [ID!]!): [Note]
  starting(prefix: String!): [Note]
  pipe: AWSJSON
}

type Mutation {
  saveNotes(a: ID!, b: ID!): AWSJSON
  removeNotes(ids: [ID!]!): AWSJSON
}
`

func TestAppSyncMappingTemplatesAndBatching(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	if _, err := client(s, "test", "us-east-1").CreateTable(ctx, &sdk.CreateTableInput{
		TableName: aws.String("vtlnotes"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema:   []types.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash}},
		BillingMode: types.BillingModePayPerRequest,
	}); err != nil {
		t.Fatal(err)
	}

	fn := createFunction(t, lambdaClient(s, "test"), "batcher", "app.graphqlBatch")
	c := appsyncClient(s)
	created, err := c.CreateGraphqlApi(ctx, &appsync.CreateGraphqlApiInput{
		Name: aws.String("vtl"), AuthenticationType: astypes.AuthenticationTypeApiKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.GraphqlApi.ApiId
	if _, err = c.StartSchemaCreation(ctx, &appsync.StartSchemaCreationInput{
		ApiId: id, Definition: []byte(vtlSchema),
	}); err != nil {
		t.Fatal(err)
	}

	for _, in := range []*appsync.CreateDataSourceInput{
		{Name: aws.String("none"), Type: astypes.DataSourceTypeNone},
		{
			Name: aws.String("table"), Type: astypes.DataSourceTypeAmazonDynamodb,
			DynamodbConfig: &astypes.DynamodbDataSourceConfig{
				TableName: aws.String("vtlnotes"), AwsRegion: aws.String("us-east-1"),
			},
		},
		{
			Name: aws.String("fn"), Type: astypes.DataSourceTypeAwsLambda,
			LambdaConfig: &astypes.LambdaDataSourceConfig{LambdaFunctionArn: fn.FunctionArn},
		},
	} {
		in.ApiId = id
		if _, err = c.CreateDataSource(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	resolve := func(in *appsync.CreateResolverInput) {
		t.Helper()
		in.ApiId = id
		if _, err = c.CreateResolver(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	vtlResolver := func(typeName, field, source, request, response string) {
		t.Helper()
		resolve(&appsync.CreateResolverInput{
			TypeName: aws.String(typeName), FieldName: aws.String(field), DataSourceName: aws.String(source),
			RequestMappingTemplate: aws.String(request), ResponseMappingTemplate: aws.String(response),
		})
	}
	toJSON := "$util.toJson($ctx.result)"
	vtlResolver("Query", "posts", "none",
		`{"version": "2018-05-29", "payload": [{"id": "1"}, {"id": "2"}, {"id": "3"}]}`, toJSON)
	vtlResolver("Query", "hello", "none", `{"version": "2018-05-29", "payload": {
  "greeting": "Hello $util.defaultIfNull($ctx.args.name, "you")",
  "id": "$util.autoId()",
  "keys": $util.toJson($util.dynamodb.toMapValues({"n": 1, "s": "x", "b": true}))
}}`, `#set($out = {"greeting": $ctx.result.greeting, "idLength": $ctx.result.id.length(), "keys": $ctx.result.keys})
$util.toJson($out)`)
	vtlResolver("Query", "guarded", "none", `#if(!$ctx.args.ok)$util.error("Not allowed", "Custom", {"a": 1})#end{}`,
		toJSON)
	vtlResolver("Query", "note", "table",
		`{"version": "2018-05-29", "operation": "GetItem", "key": {"id": $util.dynamodb.toDynamoDBJson($ctx.args.id)}}`,
		toJSON)
	vtlResolver("Query", "notes", "table", `#set($keys = [])
#foreach($id in $ctx.args.ids)$util.qr($keys.add({"id": $util.dynamodb.toDynamoDB($id)}))#end
{"version": "2018-05-29", "operation": "BatchGetItem", "tables": {"vtlnotes": {"keys": $util.toJson($keys)}}}`,
		"$util.toJson($ctx.result.data.vtlnotes)")
	vtlResolver("Query", "starting", "table", `{"version": "2018-05-29", "operation": "Scan",
  "filter": $util.transform.toDynamoDBFilterExpression({"text": {"beginsWith": $ctx.args.prefix}})}`,
		"$util.toJson($ctx.result.items)")
	vtlResolver("Mutation", "saveNotes", "table", `{"version": "2018-05-29", "operation": "TransactWriteItems",
  "transactItems": [
    {"table": "vtlnotes", "operation": "PutItem", "key": {"id": $util.dynamodb.toDynamoDBJson($ctx.args.a)},
     "attributeValues": {"text": {"S": "first"}}},
    {"table": "vtlnotes", "operation": "PutItem", "key": {"id": $util.dynamodb.toDynamoDBJson($ctx.args.b)},
     "attributeValues": {"text": {"S": "second"}}}
  ]}`, toJSON)
	runtime := &astypes.AppSyncRuntime{Name: astypes.RuntimeNameAppsyncJs, RuntimeVersion: aws.String("1.0.0")}
	resolve(&appsync.CreateResolverInput{
		TypeName: aws.String("Mutation"), FieldName: aws.String("removeNotes"), DataSourceName: aws.String("table"),
		Runtime: runtime, Code: aws.String(`import { util } from '@aws-appsync/utils';
export const request = (ctx) => ({
  operation: 'BatchDeleteItem',
  tables: { vtlnotes: ctx.args.ids.map((id) => util.dynamodb.toMapValues({ id })) },
});
export const response = (ctx) => ctx.result.data.vtlnotes;`),
	})

	// Post.author is a direct Lambda resolver and vtlAuthor and jsAuthor
	// BatchInvoke it, so a list of posts reaches the function in batches.
	resolve(&appsync.CreateResolverInput{
		TypeName: aws.String("Post"), FieldName: aws.String("author"), DataSourceName: aws.String("fn"),
		MaxBatchSize: 2,
	})
	resolve(&appsync.CreateResolverInput{
		TypeName:       aws.String("Post"),
		FieldName:      aws.String("vtlAuthor"),
		DataSourceName: aws.String("fn"),
		MaxBatchSize:   5,
		RequestMappingTemplate: aws.String(
			`{"version": "2018-05-29", "operation": "BatchInvoke", "payload": {"id": "$ctx.source.id"}}`,
		),
		ResponseMappingTemplate: aws.String(toJSON),
	})
	resolve(&appsync.CreateResolverInput{
		TypeName: aws.String("Post"), FieldName: aws.String("jsAuthor"), DataSourceName: aws.String("fn"),
		MaxBatchSize: 5, Runtime: runtime, Code: aws.String(`export const request = (ctx) =>
  ({ operation: 'BatchInvoke', payload: { id: ctx.source.id } });
export const response = (ctx) => ctx.result;`),
	})

	// A pipeline of mapping templates shares the stash; a function's
	// #return skips its data source.
	var functionIDs []string
	for _, f := range []struct{ name, request, response string }{
		{
			"count", `{"version": "2018-05-29", "payload": {"n": $ctx.stash.start}}`,
			`#set($ctx.stash.n = $ctx.result.n + 1)$util.toJson($ctx.result)`,
		},
		{"early", `#return({"n": $ctx.stash.n})`, toJSON},
	} {
		out, fnErr := c.CreateFunction(ctx, &appsync.CreateFunctionInput{
			ApiId:                   id,
			Name:                    aws.String(f.name),
			DataSourceName:          aws.String("none"),
			FunctionVersion:         aws.String("2018-05-29"),
			RequestMappingTemplate:  aws.String(f.request),
			ResponseMappingTemplate: aws.String(f.response),
		})
		if fnErr != nil {
			t.Fatal(fnErr)
		}

		functionIDs = append(functionIDs, *out.FunctionConfiguration.FunctionId)
	}

	resolve(&appsync.CreateResolverInput{
		TypeName: aws.String("Query"), FieldName: aws.String("pipe"), Kind: astypes.ResolverKindPipeline,
		PipelineConfig:          &astypes.PipelineConfig{Functions: functionIDs},
		RequestMappingTemplate:  aws.String(`$util.qr($ctx.stash.put("start", 1)){}`),
		ResponseMappingTemplate: aws.String(`$util.toJson({"final": $ctx.prev.result.n, "stash": $ctx.stash.n})`),
	})

	_, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
		ApiId: id, TypeName: aws.String("Query"), FieldName: aws.String("guarded"), DataSourceName: aws.String("none"),
		RequestMappingTemplate: aws.String(`#if($x){}`), ResponseMappingTemplate: aws.String(toJSON),
	})
	if errorCode(err) != "BadRequestException" {
		t.Fatalf("broken template: %v", err)
	}

	key, err := c.CreateApiKey(ctx, &appsync.CreateApiKeyInput{ApiId: id})
	if err != nil {
		t.Fatal(err)
	}

	header := http.Header{"X-Api-Key": {*key.ApiKey.Id}}
	query := func(q string, vars map[string]any) graphqlAnswer {
		t.Helper()
		return graphql(t, s, *id, q, vars, header)
	}

	got := query(`{ posts { id author { name batchSize } vtlAuthor { batchSize } jsAuthor { name batchSize } } }`, nil)
	post := func(id, size string) string {
		return `{"id":"` + id + `","author":{"name":"author-` + id + `","batchSize":` + size + `},` +
			`"vtlAuthor":{"batchSize":3},"jsAuthor":{"name":"author-` + id + `","batchSize":3}}`
	}
	want := `{"posts":[` + post("1", "2") + "," + post("2", "2") + "," + post("3", "1") + "]}"
	if string(got.Data) != want || len(got.Errors) != 0 {
		t.Fatalf("batched = %s %+v", got.Data, got.Errors)
	}

	got = query(`{ hello(name: "Ana") }`, nil)
	var hello map[string]any
	var wrapper struct{ Hello string }
	if err = json.Unmarshal(got.Data, &wrapper); err != nil || json.Unmarshal([]byte(wrapper.Hello), &hello) != nil ||
		hello["greeting"] != "Hello Ana" || hello["idLength"] != 36.0 {
		t.Fatalf("hello = %s %+v", got.Data, got.Errors)
	}

	if keys, _ := hello["keys"].(map[string]any); keys["n"].(map[string]any)["N"] != "1" ||
		keys["b"].(map[string]any)["BOOL"] != true {
		t.Fatalf("toMapValues = %v", hello["keys"])
	}

	got = query(`{ guarded }`, nil)
	if string(got.Data) != `{"guarded":null}` || len(got.Errors) != 1 || got.Errors[0].ErrorType != "Custom" ||
		got.Errors[0].Message != "Not allowed" {
		t.Fatalf("guarded = %s %+v", got.Data, got.Errors)
	}

	got = query(`mutation { saveNotes(a: "a", b: "b") }`, nil)
	if string(got.Data) != `{"saveNotes":"{\"cancellationReasons\":null,\"keys\":[{\"id\":\"a\"},{\"id\":\"b\"}]}"}` {
		t.Fatalf("transaction = %s %+v", got.Data, got.Errors)
	}

	got = query(
		`{ note(id: "a") { id text } notes(ids: ["b", "missing", "a"]) { id text } starting(prefix: "sec") { id } }`,
		nil,
	)
	reads := `{"note":{"id":"a","text":"first"},"notes":[{"id":"b","text":"second"},null,` +
		`{"id":"a","text":"first"}],"starting":[{"id":"b"}]}`
	if string(got.Data) != reads {
		t.Fatalf("reads = %s %+v", got.Data, got.Errors)
	}

	got = query(`{ pipe }`, nil)
	if string(got.Data) != `{"pipe":"{\"final\":2,\"stash\":2}"}` {
		t.Fatalf("pipeline = %s %+v", got.Data, got.Errors)
	}

	got = query(`mutation { removeNotes(ids: ["a", "b"]) }`, nil)
	if string(got.Data) != `{"removeNotes":"[{\"id\":\"a\"},{\"id\":\"b\"}]"}` {
		t.Fatalf("batch delete = %s %+v", got.Data, got.Errors)
	}

	got = query(`{ notes(ids: ["a"]) { id } }`, nil)
	if string(got.Data) != `{"notes":[null]}` {
		t.Fatalf("after delete = %s %+v", got.Data, got.Errors)
	}
}

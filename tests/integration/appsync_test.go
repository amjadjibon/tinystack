package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appsync"
	astypes "github.com/aws/aws-sdk-go-v2/service/appsync/types"
)

func appsyncClient(s *httptest.Server) *appsync.Client {
	return appsync.New(appsync.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

const appsyncSchema = `
type Post @aws_api_key {
  id: ID!
  title: String
  createdAt: AWSDateTime
}

type Query {
  post(id: ID!): Post
  posts: [Post]
}

schema {
  query: Query
}
`

// newGraphqlAPI creates an API-key API with appsyncSchema.
func newGraphqlAPI(t *testing.T, c *appsync.Client) string {
	t.Helper()
	ctx := context.Background()
	created, err := c.CreateGraphqlApi(ctx, &appsync.CreateGraphqlApiInput{
		Name: aws.String("blog"), AuthenticationType: astypes.AuthenticationTypeApiKey,
		Tags: map[string]string{"team": "a"},
	})
	if err != nil {
		t.Fatalf("CreateGraphqlApi: %v", err)
	}

	id := *created.GraphqlApi.ApiId
	if _, err = c.StartSchemaCreation(ctx, &appsync.StartSchemaCreationInput{
		ApiId: aws.String(id), Definition: []byte(appsyncSchema),
	}); err != nil {
		t.Fatalf("StartSchemaCreation: %v", err)
	}

	return id
}

func TestAppSyncGraphqlAPIs(t *testing.T) {
	c := appsyncClient(server(t))
	ctx := context.Background()
	id := newGraphqlAPI(t, c)

	got, err := c.GetGraphqlApi(ctx, &appsync.GetGraphqlApiInput{ApiId: aws.String(id)})
	if err != nil {
		t.Fatalf("GetGraphqlApi: %v", err)
	}

	a := got.GraphqlApi
	if len(id) != 26 || *a.Name != "blog" || !strings.HasSuffix(*a.Arn, ":apis/"+id) ||
		!strings.Contains(a.Uris["GRAPHQL"], id+".appsync-api.localhost") || a.Tags["team"] != "a" {
		t.Fatalf("api = %+v", a)
	}

	updated, err := c.UpdateGraphqlApi(ctx, &appsync.UpdateGraphqlApiInput{
		ApiId: aws.String(id), Name: aws.String("blog2"), AuthenticationType: astypes.AuthenticationTypeAwsIam,
		AdditionalAuthenticationProviders: []astypes.AdditionalAuthenticationProvider{
			{AuthenticationType: astypes.AuthenticationTypeApiKey},
		},
	})
	if err != nil || *updated.GraphqlApi.Name != "blog2" ||
		updated.GraphqlApi.AuthenticationType != astypes.AuthenticationTypeAwsIam {
		t.Fatalf("UpdateGraphqlApi = %+v, %v", updated, err)
	}

	_, err = c.CreateGraphqlApi(ctx, &appsync.CreateGraphqlApiInput{
		Name: aws.String("pool"), AuthenticationType: astypes.AuthenticationTypeAmazonCognitoUserPools,
	})
	if code := errorCode(err); code != "BadRequestException" {
		t.Fatalf("Cognito API without a pool: %v", err)
	}

	listed, err := c.ListGraphqlApis(ctx, &appsync.ListGraphqlApisInput{})
	if err != nil || len(listed.GraphqlApis) != 1 {
		t.Fatalf("ListGraphqlApis = %+v, %v", listed, err)
	}

	if _, err = c.TagResource(ctx, &appsync.TagResourceInput{
		ResourceArn: a.Arn, Tags: map[string]string{"env": "dev"},
	}); err != nil {
		t.Fatalf("TagResource: %v", err)
	}

	if _, err = c.UntagResource(ctx, &appsync.UntagResourceInput{
		ResourceArn: a.Arn, TagKeys: []string{"team"},
	}); err != nil {
		t.Fatalf("UntagResource: %v", err)
	}

	tags, err := c.ListTagsForResource(ctx, &appsync.ListTagsForResourceInput{ResourceArn: a.Arn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["env"] != "dev" {
		t.Fatalf("ListTagsForResource = %+v, %v", tags, err)
	}

	if _, err = c.DeleteGraphqlApi(ctx, &appsync.DeleteGraphqlApiInput{ApiId: aws.String(id)}); err != nil {
		t.Fatalf("DeleteGraphqlApi: %v", err)
	}

	_, err = c.GetGraphqlApi(ctx, &appsync.GetGraphqlApiInput{ApiId: aws.String(id)})
	if code := errorCode(err); code != "NotFoundException" {
		t.Fatalf("GetGraphqlApi after delete: %v", err)
	}
}

func TestAppSyncAPIKeys(t *testing.T) {
	c := appsyncClient(server(t))
	ctx := context.Background()
	id := newGraphqlAPI(t, c)

	created, err := c.CreateApiKey(ctx, &appsync.CreateApiKeyInput{ApiId: aws.String(id), Description: aws.String("k")})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}

	k := created.ApiKey
	week := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Hour).Unix()
	if !strings.HasPrefix(*k.Id, "da2-") || k.Expires != week || k.Deletes != week+60*24*3600 {
		t.Fatalf("key = %+v", k)
	}

	_, err = c.CreateApiKey(ctx, &appsync.CreateApiKeyInput{ApiId: aws.String(id), Expires: time.Now().Unix()})
	if code := errorCode(err); code != "ApiKeyValidityOutOfBoundsException" {
		t.Fatalf("CreateApiKey expiring now: %v", err)
	}

	later := time.Now().Add(30 * 24 * time.Hour).Unix()
	updated, err := c.UpdateApiKey(ctx, &appsync.UpdateApiKeyInput{ApiId: aws.String(id), Id: k.Id, Expires: later})
	if err != nil || updated.ApiKey.Expires != later-later%3600 || *updated.ApiKey.Description != "k" {
		t.Fatalf("UpdateApiKey = %+v, %v", updated, err)
	}

	listed, err := c.ListApiKeys(ctx, &appsync.ListApiKeysInput{ApiId: aws.String(id)})
	if err != nil || len(listed.ApiKeys) != 1 {
		t.Fatalf("ListApiKeys = %+v, %v", listed, err)
	}

	if _, err = c.DeleteApiKey(ctx, &appsync.DeleteApiKeyInput{ApiId: aws.String(id), Id: k.Id}); err != nil {
		t.Fatalf("DeleteApiKey: %v", err)
	}

	_, err = c.DeleteApiKey(ctx, &appsync.DeleteApiKeyInput{ApiId: aws.String(id), Id: k.Id})
	if code := errorCode(err); code != "NotFoundException" {
		t.Fatalf("DeleteApiKey twice: %v", err)
	}
}

func TestAppSyncSchemaAndTypes(t *testing.T) {
	c := appsyncClient(server(t))
	ctx := context.Background()
	id := newGraphqlAPI(t, c)

	status, err := c.GetSchemaCreationStatus(ctx, &appsync.GetSchemaCreationStatusInput{ApiId: aws.String(id)})
	if err != nil || status.Status != astypes.SchemaStatusSuccess {
		t.Fatalf("GetSchemaCreationStatus = %+v, %v", status, err)
	}

	if _, err = c.StartSchemaCreation(ctx, &appsync.StartSchemaCreationInput{
		ApiId: aws.String(id), Definition: []byte("type Query { broken: Missing }"),
	}); err != nil {
		t.Fatalf("StartSchemaCreation: %v", err)
	}

	status, err = c.GetSchemaCreationStatus(ctx, &appsync.GetSchemaCreationStatusInput{ApiId: aws.String(id)})
	if err != nil || status.Status != astypes.SchemaStatusFailed || !strings.Contains(*status.Details, "Missing") {
		t.Fatalf("failed schema status = %+v, %v", status, err)
	}

	created, err := c.CreateType(ctx, &appsync.CreateTypeInput{
		ApiId: aws.String(id), Format: astypes.TypeDefinitionFormatSdl,
		Definition: aws.String(`"A comment." type Comment { id: ID! body: String }`),
	})
	if err != nil || *created.Type.Name != "Comment" || *created.Type.Description != "A comment." {
		t.Fatalf("CreateType = %+v, %v", created, err)
	}

	if _, err = c.UpdateType(ctx, &appsync.UpdateTypeInput{
		ApiId: aws.String(id), TypeName: aws.String("Comment"), Format: astypes.TypeDefinitionFormatSdl,
		Definition: aws.String("type Comment { id: ID! body: String author: String }"),
	}); err != nil {
		t.Fatalf("UpdateType: %v", err)
	}

	got, err := c.GetType(ctx, &appsync.GetTypeInput{
		ApiId: aws.String(id), TypeName: aws.String("Comment"), Format: astypes.TypeDefinitionFormatSdl,
	})
	if err != nil || !strings.Contains(*got.Type.Definition, "author: String") {
		t.Fatalf("GetType = %+v, %v", got, err)
	}

	listed, err := c.ListTypes(
		ctx,
		&appsync.ListTypesInput{ApiId: aws.String(id), Format: astypes.TypeDefinitionFormatSdl},
	)
	if err != nil || len(listed.Types) != 3 || *listed.Types[0].Name != "Comment" {
		t.Fatalf("ListTypes = %+v, %v", listed, err)
	}

	if _, err = c.DeleteType(ctx, &appsync.DeleteTypeInput{
		ApiId: aws.String(id), TypeName: aws.String("Post"),
	}); errorCode(err) != "BadRequestException" {
		t.Fatalf("DeleteType of a referenced type: %v", err)
	}

	if _, err = c.DeleteType(ctx, &appsync.DeleteTypeInput{
		ApiId: aws.String(id), TypeName: aws.String("Comment"),
	}); err != nil {
		t.Fatalf("DeleteType: %v", err)
	}

	sdl, err := c.GetIntrospectionSchema(ctx, &appsync.GetIntrospectionSchemaInput{
		ApiId: aws.String(id), Format: astypes.OutputTypeSdl, IncludeDirectives: aws.Bool(true),
	})
	if err != nil || !strings.Contains(string(sdl.Schema), "@aws_api_key") ||
		!strings.Contains(string(sdl.Schema), "createdAt: AWSDateTime") ||
		strings.Contains(string(sdl.Schema), "Comment") {
		t.Fatalf("GetIntrospectionSchema = %s, %v", sdl.Schema, err)
	}
}

func TestAppSyncDataSourcesResolversAndFunctions(t *testing.T) {
	c := appsyncClient(server(t))
	ctx := context.Background()
	id := newGraphqlAPI(t, c)

	if _, err := c.CreateDataSource(ctx, &appsync.CreateDataSourceInput{
		ApiId: aws.String(id), Name: aws.String("posts"), Type: astypes.DataSourceTypeAwsLambda,
		LambdaConfig: &astypes.LambdaDataSourceConfig{
			LambdaFunctionArn: aws.String("arn:aws:lambda:us-east-1:000000000000:function:posts"),
		},
	}); err != nil {
		t.Fatalf("CreateDataSource: %v", err)
	}

	_, err := c.CreateDataSource(ctx, &appsync.CreateDataSourceInput{
		ApiId: aws.String(id), Name: aws.String("bad"), Type: astypes.DataSourceTypeAwsLambda,
	})
	if code := errorCode(err); code != "BadRequestException" {
		t.Fatalf("Lambda data source without lambdaConfig: %v", err)
	}

	if _, err = c.CreateDataSource(ctx, &appsync.CreateDataSourceInput{
		ApiId: aws.String(id), Name: aws.String("local"), Type: astypes.DataSourceTypeNone,
	}); err != nil {
		t.Fatalf("CreateDataSource NONE: %v", err)
	}

	sources, err := c.ListDataSources(ctx, &appsync.ListDataSourcesInput{ApiId: aws.String(id)})
	if err != nil || len(sources.DataSources) != 2 || *sources.DataSources[0].Name != "local" {
		t.Fatalf("ListDataSources = %+v, %v", sources, err)
	}

	resolver, err := c.CreateResolver(ctx, &appsync.CreateResolverInput{
		ApiId: aws.String(id), TypeName: aws.String("Query"), FieldName: aws.String("post"),
		DataSourceName: aws.String("posts"),
	})
	if err != nil || resolver.Resolver.Kind != astypes.ResolverKindUnit ||
		!strings.HasSuffix(*resolver.Resolver.ResolverArn, "/types/Query/resolvers/post") {
		t.Fatalf("CreateResolver = %+v, %v", resolver, err)
	}

	_, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
		ApiId: aws.String(id), TypeName: aws.String("Query"), FieldName: aws.String("missing"),
		DataSourceName: aws.String("posts"),
	})
	if code := errorCode(err); code != "NotFoundException" {
		t.Fatalf("CreateResolver on a missing field: %v", err)
	}

	runtime := &astypes.AppSyncRuntime{Name: astypes.RuntimeNameAppsyncJs, RuntimeVersion: aws.String("1.0.0")}
	fn, err := c.CreateFunction(ctx, &appsync.CreateFunctionInput{
		ApiId:          aws.String(id),
		Name:           aws.String("load"),
		DataSourceName: aws.String("local"),
		Runtime:        runtime,
		Code: aws.String(
			"export function request(ctx) { return {} }\nexport function response(ctx) { return ctx.result }",
		),
	})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}

	if _, err = c.CreateResolver(ctx, &appsync.CreateResolverInput{
		ApiId:          aws.String(id),
		TypeName:       aws.String("Query"),
		FieldName:      aws.String("posts"),
		Kind:           astypes.ResolverKindPipeline,
		Runtime:        runtime,
		PipelineConfig: &astypes.PipelineConfig{Functions: []string{*fn.FunctionConfiguration.FunctionId}},
		Code: aws.String(
			"export function request(ctx) { return {} }\nexport function response(ctx) { return ctx.prev.result }",
		),
	}); err != nil {
		t.Fatalf("CreateResolver PIPELINE: %v", err)
	}

	byFunction, err := c.ListResolversByFunction(ctx, &appsync.ListResolversByFunctionInput{
		ApiId: aws.String(id), FunctionId: fn.FunctionConfiguration.FunctionId,
	})
	if err != nil || len(byFunction.Resolvers) != 1 || *byFunction.Resolvers[0].FieldName != "posts" {
		t.Fatalf("ListResolversByFunction = %+v, %v", byFunction, err)
	}

	resolvers, err := c.ListResolvers(
		ctx,
		&appsync.ListResolversInput{ApiId: aws.String(id), TypeName: aws.String("Query")},
	)
	if err != nil || len(resolvers.Resolvers) != 2 {
		t.Fatalf("ListResolvers = %+v, %v", resolvers, err)
	}

	if _, err = c.UpdateFunction(ctx, &appsync.UpdateFunctionInput{
		ApiId: aws.String(id), FunctionId: fn.FunctionConfiguration.FunctionId, Name: aws.String("load2"),
		DataSourceName: aws.String("local"), Runtime: runtime, Code: fn.FunctionConfiguration.Code,
	}); err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}

	gotFn, err := c.GetFunction(ctx, &appsync.GetFunctionInput{
		ApiId: aws.String(id), FunctionId: fn.FunctionConfiguration.FunctionId,
	})
	if err != nil || *gotFn.FunctionConfiguration.Name != "load2" {
		t.Fatalf("GetFunction = %+v, %v", gotFn, err)
	}

	if _, err = c.DeleteResolver(ctx, &appsync.DeleteResolverInput{
		ApiId: aws.String(id), TypeName: aws.String("Query"), FieldName: aws.String("post"),
	}); err != nil {
		t.Fatalf("DeleteResolver: %v", err)
	}

	_, err = c.GetResolver(ctx, &appsync.GetResolverInput{
		ApiId: aws.String(id), TypeName: aws.String("Query"), FieldName: aws.String("post"),
	})
	if code := errorCode(err); code != "NotFoundException" {
		t.Fatalf("GetResolver after delete: %v", err)
	}
}

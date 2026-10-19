package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

func restClient(s *httptest.Server) *restapi.Client {
	return restapi.New(restapi.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func replace(path, v string) resttypes.PatchOperation {
	return resttypes.PatchOperation{Op: resttypes.OpReplace, Path: aws.String(path), Value: aws.String(v)}
}

// restMethod adds a method with an integration to a resource.
func restMethod(t *testing.T, c *restapi.Client, apiID, resourceID, method string, in *restapi.PutIntegrationInput) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: aws.String(apiID), ResourceId: aws.String(resourceID), HttpMethod: aws.String(method),
		AuthorizationType: aws.String("NONE"),
	}); err != nil {
		t.Fatal(err)
	}

	in.RestApiId, in.ResourceId, in.HttpMethod = aws.String(apiID), aws.String(resourceID), aws.String(method)
	if _, err := c.PutIntegration(ctx, in); err != nil {
		t.Fatal(err)
	}
}

func lambdaURI(arn *string) *string {
	return aws.String("arn:aws:apigateway:us-east-1:lambda:path/2015-03-31/functions/" + aws.ToString(arn) +
		"/invocations")
}

func TestRESTAPIManagement(t *testing.T) {
	s := server(t)
	c := restClient(s)
	ctx := context.Background()

	created, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{
		Name: aws.String("pets"), Tags: map[string]string{"team": "a"}, BinaryMediaTypes: []string{"image/png"},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.Id
	if created.RootResourceId == nil || created.EndpointConfiguration.Types[0] != resttypes.EndpointTypeEdge ||
		created.ApiKeySource != resttypes.ApiKeySourceTypeHeader || created.CreatedDate == nil {
		t.Fatalf("%+v", created)
	}

	updated, err := c.UpdateRestApi(
		ctx,
		&restapi.UpdateRestApiInput{RestApiId: id, PatchOperations: []resttypes.PatchOperation{
			replace("/name", "petstore"),
			{Op: resttypes.OpAdd, Path: aws.String("/binaryMediaTypes/image~1jpeg")},
			{Op: resttypes.OpRemove, Path: aws.String("/binaryMediaTypes/image~1png")},
			replace("/endpointConfiguration/types/EDGE", "REGIONAL"),
			replace("/minimumCompressionSize", "1024"),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(updated.Name) != "petstore" || len(updated.BinaryMediaTypes) != 1 ||
		updated.BinaryMediaTypes[0] != "image/jpeg" ||
		updated.EndpointConfiguration.Types[0] != resttypes.EndpointTypeRegional ||
		aws.ToInt32(updated.MinimumCompressionSize) != 1024 {
		t.Fatalf("%+v", updated)
	}

	_, err = c.UpdateRestApi(ctx, &restapi.UpdateRestApiInput{
		RestApiId: id, PatchOperations: []resttypes.PatchOperation{replace("/id", "x")},
	})
	code(t, err, "BadRequestException")

	pets, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: id, ParentId: created.RootResourceId, PathPart: aws.String("pets"),
	})
	if err != nil || aws.ToString(pets.Path) != "/pets" {
		t.Fatalf("%+v %v", pets, err)
	}

	pet, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: id, ParentId: pets.Id, PathPart: aws.String("{id}"),
	})
	if err != nil || aws.ToString(pet.Path) != "/pets/{id}" {
		t.Fatalf("%+v %v", pet, err)
	}

	_, err = c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: id, ParentId: created.RootResourceId, PathPart: aws.String("pets"),
	})
	code(t, err, "ConflictException")

	_, err = c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: id, ParentId: pets.Id, PathPart: aws.String("{name}"),
	})
	code(t, err, "BadRequestException")

	// A deployment needs methods with integrations.
	_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: id, StageName: aws.String("prod")})
	code(t, err, "BadRequestException")

	if _, err = c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), AuthorizationType: aws.String("NONE"),
		RequestParameters: map[string]bool{"method.request.path.id": true},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), AuthorizationType: aws.String("NONE"),
	})
	code(t, err, "ConflictException")

	method, err := c.UpdateMethod(ctx, &restapi.UpdateMethodInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"),
		PatchOperations: []resttypes.PatchOperation{
			replace("/operationName", "GetPet"),
			{
				Op:    resttypes.OpAdd,
				Path:  aws.String("/requestParameters/method.request.querystring.verbose"),
				Value: aws.String("false"),
			},
		},
	})
	if err != nil || aws.ToString(method.OperationName) != "GetPet" || len(method.RequestParameters) != 2 {
		t.Fatalf("%+v %v", method, err)
	}

	_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), Type: resttypes.IntegrationTypeAws,
		IntegrationHttpMethod: aws.String("POST"), Uri: aws.String("arn:aws:apigateway:us-east-1:sqs:path/x"),
	})
	code(t, err, "BadRequestException")

	if _, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), Type: resttypes.IntegrationTypeMock,
		RequestTemplates: map[string]string{"application/json": `{"statusCode": 200}`},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
	})
	code(t, err, "NotFoundException")

	if _, err = c.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
		ResponseParameters: map[string]bool{"method.response.header.X-Pet": true},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
		RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET"), StatusCode: aws.String("200"),
		ResponseParameters: map[string]string{"method.response.header.X-Pet": "'rex'"},
		ResponseTemplates:  map[string]string{"application/json": `{"name":"rex"}`},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetMethod(
		ctx,
		&restapi.GetMethodInput{RestApiId: id, ResourceId: pet.Id, HttpMethod: aws.String("GET")},
	)
	if err != nil || got.MethodIntegration == nil || len(got.MethodIntegration.IntegrationResponses) != 1 ||
		len(got.MethodResponses) != 1 || got.MethodIntegration.TimeoutInMillis != 29000 {
		t.Fatalf("%+v %v", got, err)
	}

	deployment, err := c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{
		RestApiId: id, StageName: aws.String("prod"), Variables: map[string]string{"color": "blue"},
	})
	if err != nil {
		t.Fatal(err)
	}

	stage, err := c.UpdateStage(ctx, &restapi.UpdateStageInput{
		RestApiId: id, StageName: aws.String("prod"), PatchOperations: []resttypes.PatchOperation{
			replace("/variables/color", "green"),
			replace("/*/*/metrics/enabled", "true"),
			replace("/*/*/throttling/burstLimit", "10"),
			replace("/tracingEnabled", "true"),
		},
	})
	if err != nil || stage.Variables["color"] != "green" || !stage.MethodSettings["*/*"].MetricsEnabled ||
		stage.MethodSettings["*/*"].ThrottlingBurstLimit != 10 || !stage.TracingEnabled ||
		aws.ToString(stage.DeploymentId) != aws.ToString(deployment.Id) {
		t.Fatalf("%+v %v", stage, err)
	}

	_, err = c.DeleteDeployment(ctx, &restapi.DeleteDeploymentInput{RestApiId: id, DeploymentId: deployment.Id})
	code(t, err, "BadRequestException")

	// Moving a resource moves the paths under it.
	food, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: id, ParentId: created.RootResourceId, PathPart: aws.String("food"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.UpdateResource(ctx, &restapi.UpdateResourceInput{
		RestApiId:       id,
		ResourceId:      pets.Id,
		PatchOperations: []resttypes.PatchOperation{replace("/parentId", aws.ToString(food.Id))},
	}); err != nil {
		t.Fatal(err)
	}

	moved, err := c.GetResource(
		ctx,
		&restapi.GetResourceInput{RestApiId: id, ResourceId: pet.Id, Embed: []string{"methods"}},
	)
	if err != nil || aws.ToString(moved.Path) != "/food/pets/{id}" ||
		moved.ResourceMethods["GET"].MethodIntegration == nil {
		t.Fatalf("%+v %v", moved, err)
	}

	var paths []string
	var position *string
	for {
		page, listErr := c.GetResources(
			ctx,
			&restapi.GetResourcesInput{RestApiId: id, Limit: aws.Int32(2), Position: position},
		)
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, r := range page.Items {
			paths = append(paths, aws.ToString(r.Path))
		}

		if position = page.Position; position == nil {
			break
		}
	}

	if len(paths) != 4 {
		t.Fatalf("%v", paths)
	}

	if _, err = c.DeleteResource(ctx, &restapi.DeleteResourceInput{RestApiId: id, ResourceId: food.Id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetResource(ctx, &restapi.GetResourceInput{RestApiId: id, ResourceId: pet.Id})
	code(t, err, "NotFoundException")

	_, err = c.DeleteResource(ctx, &restapi.DeleteResourceInput{RestApiId: id, ResourceId: created.RootResourceId})
	code(t, err, "BadRequestException")

	// Tags on APIs and stages.
	arn := aws.String("arn:aws:apigateway:us-east-1::/restapis/" + aws.ToString(id))
	if _, err = c.TagResource(ctx, &restapi.TagResourceInput{
		ResourceArn: aws.String(aws.ToString(arn) + "/stages/prod"), Tags: map[string]string{"env": "prod"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(
		ctx,
		&restapi.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.GetTags(ctx, &restapi.GetTagsInput{ResourceArn: aws.String(aws.ToString(arn) + "/stages/prod")})
	if err != nil || tags.Tags["env"] != "prod" {
		t.Fatalf("%+v %v", tags, err)
	}

	if _, err = c.DeleteRestApi(ctx, &restapi.DeleteRestApiInput{RestApiId: id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetRestApi(ctx, &restapi.GetRestApiInput{RestApiId: id})
	code(t, err, "NotFoundException")
}

func TestRESTAPIExecution(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	functions := lambdaClient(s, "test")
	respond := createFunction(t, functions, "respond", "app.respond")
	createFunction(t, functions, "fail", "app.fail")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery))
	}))
	t.Cleanup(backend.Close)

	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: aws.String("shop")})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(api.Id)
	resource := func(parent *string, part string) *string {
		out, createErr := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: parent, PathPart: aws.String(part),
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		return out.Id
	}

	pets := resource(api.RootResourceId, "pets")
	pet := resource(pets, "{id}")
	special := resource(pets, "special")
	broken := resource(api.RootResourceId, "broken")
	backendProxy := resource(resource(api.RootResourceId, "backend"), "{proxy+}")
	lambdaProxy := &restapi.PutIntegrationInput{
		Type:                  resttypes.IntegrationTypeAwsProxy,
		IntegrationHttpMethod: aws.String("POST"),
		Uri:                   lambdaURI(respond.FunctionArn),
	}
	restMethod(t, c, id, aws.ToString(pet), "GET", lambdaProxy)
	restMethod(t, c, id, aws.ToString(special), "ANY", lambdaProxy)
	restMethod(t, c, id, aws.ToString(broken), "GET", &restapi.PutIntegrationInput{
		Type: resttypes.IntegrationTypeAwsProxy, IntegrationHttpMethod: aws.String("POST"),
		Uri: lambdaURI(aws.String("arn:aws:lambda:us-east-1:000000000000:function:fail")),
	})
	restMethod(t, c, id, aws.ToString(backendProxy), "ANY", &restapi.PutIntegrationInput{
		Type: resttypes.IntegrationTypeHttpProxy, IntegrationHttpMethod: aws.String("ANY"),
		Uri: aws.String(backend.URL + "/${stageVariables.base}/{proxy}"),
	})

	// A MOCK OPTIONS method answers CORS preflights.
	restMethod(t, c, id, aws.ToString(pets), "OPTIONS", &restapi.PutIntegrationInput{
		Type:             resttypes.IntegrationTypeMock,
		RequestTemplates: map[string]string{"application/json": `{"statusCode": 200}`},
	})
	if _, err = c.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
		RestApiId: api.Id, ResourceId: pets, HttpMethod: aws.String("OPTIONS"), StatusCode: aws.String("200"),
		ResponseParameters: map[string]bool{"method.response.header.Access-Control-Allow-Origin": true},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
		RestApiId: api.Id, ResourceId: pets, HttpMethod: aws.String("OPTIONS"), StatusCode: aws.String("200"),
		ResponseParameters: map[string]string{"method.response.header.Access-Control-Allow-Origin": "'*'"},
		ResponseTemplates:  map[string]string{"application/json": ""},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{
		RestApiId: api.Id, StageName: aws.String("prod"), Variables: map[string]string{"base": "v2"},
	}); err != nil {
		t.Fatal(err)
	}

	status, header, body := call(t, s, "GET", id, "/prod/pets/42?q=1", nil, "")
	if status != http.StatusCreated || header.Get("X-Handler") != "respond" || header.Get("X-Amzn-Requestid") == "" {
		t.Fatalf("%d %v %s", status, header, body)
	}

	var event struct {
		Resource       string
		Path           string
		HTTPMethod     string `json:"httpMethod"`
		PathParameters map[string]string
		StageVariables map[string]string
		RequestContext struct {
			Path         string
			Stage        string
			ResourcePath string
		}
	}
	if err = json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatal(err)
	}

	if event.Resource != "/pets/{id}" || event.Path != "/pets/42" || event.PathParameters["id"] != "42" ||
		event.RequestContext.Path != "/prod/pets/42" || event.RequestContext.Stage != "prod" ||
		event.StageVariables["base"] != "v2" {
		t.Fatalf("%+v", event)
	}

	// The literal resource outranks {id}, and ANY takes every method.
	if status, _, body = call(t, s, "DELETE", id, "/prod/pets/special", nil, ""); status != http.StatusCreated ||
		!strings.Contains(body, `"resource":"/pets/special"`) {
		t.Fatalf("%d %s", status, body)
	}

	status, header, body = call(t, s, "OPTIONS", id, "/prod/pets", nil, "")
	if status != http.StatusOK || header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("mock: %d %v %s", status, header, body)
	}

	if status, _, body = call(t, s, "POST", id, "/prod/backend/a/b?x=1", nil, "hi"); status != http.StatusOK ||
		body != "POST /v2/a/b?x=1" {
		t.Fatalf("http proxy: %d %s", status, body)
	}

	for _, tc := range []struct {
		method, path, message string
		status                int
	}{
		{"GET", "/prod/broken", "Internal server error", http.StatusBadGateway},
		{"POST", "/prod/pets/42", "Missing Authentication Token", http.StatusForbidden},
		{"GET", "/prod/nowhere", "Missing Authentication Token", http.StatusForbidden},
		{"GET", "/test/pets/42", "Forbidden", http.StatusForbidden},
	} {
		got, _, gotBody := call(t, s, tc.method, id, tc.path, nil, "")
		if got != tc.status || !strings.Contains(gotBody, tc.message) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, got, gotBody)
		}
	}
}

func TestRESTAPIAuthorizers(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := restClient(s)
	functions := lambdaClient(s, "test")
	respond := createFunction(t, functions, "respond", "app.respond")
	policy := createFunction(t, functions, "policy", "app.policy")

	admin := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, admin, "api users", nil)
	client, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("web"),
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowAdminUserPasswordAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: aws.String("fay"), MessageAction: ciptypes.MessageActionTypeSuppress,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = admin.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: pool.Id, Username: aws.String("fay"), Password: aws.String("Passw0rd!"), Permanent: true,
	}); err != nil {
		t.Fatal(err)
	}

	signedIn, err := admin.AdminInitiateAuth(ctx, &cip.AdminInitiateAuthInput{
		UserPoolId:     pool.Id,
		ClientId:       client.UserPoolClient.ClientId,
		AuthFlow:       ciptypes.AuthFlowTypeAdminUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "fay", "PASSWORD": "Passw0rd!"},
	})
	if err != nil {
		t.Fatal(err)
	}

	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: aws.String("secure")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
		RestApiId: api.Id, Name: aws.String("bad"), Type: resttypes.AuthorizerTypeCognitoUserPools,
	})
	code(t, err, "BadRequestException")

	cognito, err := c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
		RestApiId: api.Id, Name: aws.String("users"), Type: resttypes.AuthorizerTypeCognitoUserPools,
		ProviderARNs: []string{aws.ToString(pool.Arn)},
	})
	if err != nil || aws.ToString(cognito.IdentitySource) != "method.request.header.Authorization" {
		t.Fatalf("%+v %v", cognito, err)
	}

	token, err := c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
		RestApiId: api.Id, Name: aws.String("token"), Type: resttypes.AuthorizerTypeToken,
		AuthorizerUri: lambdaURI(policy.FunctionArn), IdentitySource: aws.String("method.request.header.X-Token"),
		IdentityValidationExpression: aws.String("[a-z]+"),
	})
	if err != nil || aws.ToInt32(token.AuthorizerResultTtlInSeconds) != 300 {
		t.Fatalf("%+v %v", token, err)
	}

	integration := &restapi.PutIntegrationInput{
		Type:                  resttypes.IntegrationTypeAwsProxy,
		IntegrationHttpMethod: aws.String("POST"),
		Uri:                   lambdaURI(respond.FunctionArn),
	}
	add := func(part, authType string, authorizer *string, scopes []string) {
		r, createErr := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: aws.String(part),
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		if _, createErr = c.PutMethod(ctx, &restapi.PutMethodInput{
			RestApiId: api.Id, ResourceId: r.Id, HttpMethod: aws.String("GET"), AuthorizationType: aws.String(authType),
			AuthorizerId: authorizer, AuthorizationScopes: scopes,
		}); createErr != nil {
			t.Fatal(createErr)
		}

		integration.RestApiId, integration.ResourceId, integration.HttpMethod = api.Id, r.Id, aws.String("GET")
		if _, createErr = c.PutIntegration(ctx, integration); createErr != nil {
			t.Fatal(createErr)
		}
	}

	add("me", "COGNITO_USER_POOLS", cognito.Id, nil)
	add("admin", "COGNITO_USER_POOLS", cognito.Id, []string{"aws.cognito.signin.user.admin"})
	add("gold", "CUSTOM", token.Id, nil)
	add("iam", "AWS_IAM", nil, nil)

	_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: api.Id, ResourceId: api.RootResourceId, HttpMethod: aws.String("GET"),
		AuthorizationType: aws.String("CUSTOM"), AuthorizerId: cognito.Id,
	})
	code(t, err, "BadRequestException")

	_, err = c.DeleteAuthorizer(ctx, &restapi.DeleteAuthorizerInput{RestApiId: api.Id, AuthorizerId: token.Id})
	code(t, err, "ConflictException")

	if _, err = c.CreateDeployment(
		ctx,
		&restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: aws.String("v1")},
	); err != nil {
		t.Fatal(err)
	}

	idToken := aws.ToString(signedIn.AuthenticationResult.IdToken)
	accessToken := aws.ToString(signedIn.AuthenticationResult.AccessToken)
	for _, tc := range []struct {
		path, header, value, want string
		status                    int
	}{
		{"/v1/me", "", "", "", http.StatusUnauthorized},
		{"/v1/me", "Authorization", "not-a-token", "", http.StatusUnauthorized},
		{"/v1/me", "Authorization", idToken, `"cognito:username":"fay"`, http.StatusCreated},
		{"/v1/admin", "Authorization", idToken, "", http.StatusUnauthorized},
		{"/v1/admin", "Authorization", accessToken, `"username":"fay"`, http.StatusCreated},
		{"/v1/gold", "X-Token", "allow", `"tier":"gold"`, http.StatusCreated},
		{"/v1/gold", "X-Token", "deny", "explicit deny", http.StatusForbidden},
		{"/v1/gold", "X-Token", "who", "Unauthorized", http.StatusUnauthorized},
		{"/v1/gold", "X-Token", "NOT-VALID-1", "Unauthorized", http.StatusUnauthorized},
		{"/v1/iam", "", "", "Missing Authentication Token", http.StatusForbidden},
		{"/v1/iam", "Authorization", "AWS4-HMAC-SHA256 Credential=x", "", http.StatusForbidden},
	} {
		header := map[string]string{}
		if tc.header != "" {
			header[tc.header] = tc.value
		}

		status, _, body := call(t, s, "GET", aws.ToString(api.Id), tc.path, header, "")
		if status != tc.status || !strings.Contains(body, tc.want) {
			t.Fatalf("%s with %s=%.10s: %d %s", tc.path, tc.header, tc.value, status, body)
		}
	}
}

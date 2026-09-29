package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	apigw "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

func apigwClient(s *httptest.Server) *apigw.Client {
	return apigw.New(apigw.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// call sends a request to an API through TinyStack's execute-api path and
// returns the status, headers, and body.
func call(t *testing.T, s *httptest.Server, method, apiID, path string, header map[string]string, body string) (
	status int, respHeader http.Header, respBody string,
) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+"/_aws/execute-api/"+apiID+path,
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range header {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, string(data)
}

func TestHTTPAPIManagement(t *testing.T) {
	s := server(t)
	c := apigwClient(s)
	ctx := context.Background()

	_, err := c.CreateApi(
		ctx,
		&apigw.CreateApiInput{Name: aws.String("ws"), ProtocolType: apigwtypes.ProtocolTypeWebsocket},
	)
	code(t, err, "BadRequestException")

	created, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name: aws.String("pets"), ProtocolType: apigwtypes.ProtocolTypeHttp, Tags: map[string]string{"team": "a"},
		CorsConfiguration: &apigwtypes.Cors{AllowOrigins: []string{"https://example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := created.ApiId
	if len(aws.ToString(id)) != 10 || !strings.Contains(aws.ToString(created.ApiEndpoint), ".execute-api.localhost:") ||
		aws.ToString(
			created.RouteSelectionExpression,
		) != "$request.method $request.path" || created.CreatedDate == nil {
		t.Fatalf("%+v", created)
	}

	_, err = c.GetApi(ctx, &apigw.GetApiInput{ApiId: aws.String("missing123")})
	code(t, err, "NotFoundException")

	updated, err := c.UpdateApi(ctx, &apigw.UpdateApiInput{ApiId: id, Description: aws.String("pet store")})
	if err != nil || aws.ToString(updated.Description) != "pet store" || aws.ToString(updated.Name) != "pets" {
		t.Fatalf("%+v %v", updated, err)
	}

	if _, err = c.DeleteCorsConfiguration(ctx, &apigw.DeleteCorsConfigurationInput{ApiId: id}); err != nil {
		t.Fatal(err)
	}

	integration, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeHttpProxy, IntegrationMethod: aws.String("ANY"),
		IntegrationUri: aws.String("https://example.com/{proxy}"), PayloadFormatVersion: aws.String("1.0"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeMock,
	})
	code(t, err, "BadRequestException")

	target := aws.String("integrations/" + aws.ToString(integration.IntegrationId))
	route, err := c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: aws.String("GET /pets/{id}"), Target: target,
	})
	if err != nil || route.AuthorizationType != apigwtypes.AuthorizationTypeNone {
		t.Fatalf("%+v %v", route, err)
	}

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{ApiId: id, RouteKey: aws.String("GET /pets/{id}")})
	code(t, err, "ConflictException")

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{ApiId: id, RouteKey: aws.String("FETCH /pets")})
	code(t, err, "BadRequestException")

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: aws.String("GET /x"), Target: aws.String("integrations/nope"),
	})
	code(t, err, "BadRequestException")

	_, err = c.DeleteIntegration(
		ctx,
		&apigw.DeleteIntegrationInput{ApiId: id, IntegrationId: integration.IntegrationId},
	)
	code(t, err, "ConflictException")

	// A manually deployed stage serves the deployment it names.
	stage, err := c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: aws.String("prod"), StageVariables: map[string]string{"color": "blue"},
	})
	if err != nil || stage.DeploymentId != nil || aws.ToBool(stage.AutoDeploy) {
		t.Fatalf("%+v %v", stage, err)
	}

	deployment, err := c.CreateDeployment(ctx, &apigw.CreateDeploymentInput{ApiId: id, StageName: aws.String("prod")})
	if err != nil || deployment.DeploymentStatus != apigwtypes.DeploymentStatusDeployed {
		t.Fatalf("%+v %v", deployment, err)
	}

	got, err := c.GetStage(ctx, &apigw.GetStageInput{ApiId: id, StageName: aws.String("prod")})
	if err != nil || aws.ToString(got.DeploymentId) != aws.ToString(deployment.DeploymentId) {
		t.Fatalf("%+v %v", got, err)
	}

	_, err = c.DeleteDeployment(ctx, &apigw.DeleteDeploymentInput{ApiId: id, DeploymentId: deployment.DeploymentId})
	code(t, err, "BadRequestException")

	// An automatically deployed stage follows every change.
	auto, err := c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: aws.String("$default"), AutoDeploy: aws.Bool(true),
	})
	if err != nil || auto.DeploymentId == nil {
		t.Fatalf("%+v %v", auto, err)
	}

	if _, err = c.UpdateRoute(ctx, &apigw.UpdateRouteInput{
		ApiId: id, RouteId: route.RouteId, RouteKey: aws.String("GET /pets/{petId}"),
	}); err != nil {
		t.Fatal(err)
	}

	after, err := c.GetStage(ctx, &apigw.GetStageInput{ApiId: id, StageName: aws.String("$default")})
	if err != nil || aws.ToString(after.DeploymentId) == aws.ToString(auto.DeploymentId) {
		t.Fatalf("the change was not deployed: %+v %v", after, err)
	}

	deployments, err := c.GetDeployments(ctx, &apigw.GetDeploymentsInput{ApiId: id})
	if err != nil || len(deployments.Items) != 2 {
		t.Fatalf("the replaced automatic deployment was kept: %+v %v", deployments, err)
	}

	routes, err := c.GetRoutes(ctx, &apigw.GetRoutesInput{ApiId: id})
	if err != nil || len(routes.Items) != 1 || aws.ToString(routes.Items[0].RouteKey) != "GET /pets/{petId}" {
		t.Fatalf("%+v %v", routes, err)
	}

	// Tags on APIs and stages.
	arn := aws.String("arn:aws:apigateway:us-east-1::/apis/" + aws.ToString(id))
	stageARN := aws.String(aws.ToString(arn) + "/stages/prod")
	if _, err = c.TagResource(ctx, &apigw.TagResourceInput{ResourceArn: stageARN, Tags: map[string]string{
		"env": "prod",
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(
		ctx,
		&apigw.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.GetTags(ctx, &apigw.GetTagsInput{ResourceArn: stageARN})
	if err != nil || tags.Tags["env"] != "prod" {
		t.Fatalf("%+v %v", tags, err)
	}

	tags, err = c.GetTags(ctx, &apigw.GetTagsInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 0 {
		t.Fatalf("%+v %v", tags, err)
	}

	// Paging.
	for i := range 3 {
		if _, err = c.CreateApi(ctx, &apigw.CreateApiInput{
			Name: aws.String("more"), ProtocolType: apigwtypes.ProtocolTypeHttp,
		}); err != nil {
			t.Fatal(i, err)
		}
	}

	var names []string
	var next *string
	for {
		out, listErr := c.GetApis(ctx, &apigw.GetApisInput{MaxResults: aws.String("2"), NextToken: next})
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, a := range out.Items {
			names = append(names, aws.ToString(a.ApiId))
		}

		if next = out.NextToken; next == nil {
			break
		}
	}

	if len(names) != 4 {
		t.Fatalf("%v", names)
	}

	if _, err = c.DeleteApi(ctx, &apigw.DeleteApiInput{ApiId: id}); err != nil {
		t.Fatal(err)
	}

	if status, _, _ := call(t, s, "GET", aws.ToString(id), "/pets/1", nil, ""); status != http.StatusNotFound {
		t.Fatalf("a deleted API answered %d", status)
	}
}

func TestHTTPAPILambdaProxy(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	echo := createFunction(t, functions, "echo", "app.echo")
	respond := createFunction(t, functions, "respond", "app.respond")
	createFunction(t, functions, "fail", "app.fail")
	c := apigwClient(s)

	// Quick create: a $default route and stage for the function.
	quick, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name: aws.String("quick"), ProtocolType: apigwtypes.ProtocolTypeHttp, Target: echo.FunctionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	status, header, body := call(t, s, "POST", aws.ToString(quick.ApiId), "/anything/here?a=1&a=2&b=3",
		map[string]string{"Content-Type": "application/json", "Cookie": "a=1; b=2"}, `{"hello":"world"}`)
	if status != http.StatusOK || header.Get("Apigw-Requestid") == "" {
		t.Fatalf("%d %s", status, body)
	}

	var echoed struct {
		Event struct {
			Headers               map[string]string
			QueryStringParameters map[string]string
			RequestContext        struct {
				APIID string `json:"apiId"`
				Stage string
				HTTP  struct{ Method, Path string }
			}
			Version         string
			RouteKey        string
			RawPath         string
			RawQueryString  string
			Body            string
			Cookies         []string
			IsBase64Encoded bool
		}
	}
	if err = json.Unmarshal([]byte(body), &echoed); err != nil {
		t.Fatalf("%s: %v", body, err)
	}

	e := echoed.Event
	if e.Version != "2.0" || e.RouteKey != "$default" || e.RawPath != "/anything/here" ||
		e.RawQueryString != "a=1&a=2&b=3" || e.QueryStringParameters["a"] != "1,2" || len(e.Cookies) != 2 ||
		e.Headers["content-type"] != "application/json" || e.Body != `{"hello":"world"}` || e.IsBase64Encoded ||
		e.RequestContext.APIID != aws.ToString(quick.ApiId) || e.RequestContext.Stage != "$default" ||
		e.RequestContext.HTTP.Method != "POST" {
		t.Fatalf("%+v", e)
	}

	// Routes, path parameters, a named stage, and payload format 1.0.
	api, err := c.CreateApi(
		ctx,
		&apigw.CreateApiInput{Name: aws.String("pets"), ProtocolType: apigwtypes.ProtocolTypeHttp},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := api.ApiId
	integrate := func(fn *string, format string) *string {
		out, integrateErr := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
			ApiId: id, IntegrationType: apigwtypes.IntegrationTypeAwsProxy, PayloadFormatVersion: aws.String(format),
			IntegrationUri: aws.String("arn:aws:apigateway:us-east-1:lambda:path/2015-03-31/functions/" +
				aws.ToString(fn) + "/invocations"),
		})
		if integrateErr != nil {
			t.Fatal(integrateErr)
		}

		return aws.String("integrations/" + aws.ToString(out.IntegrationId))
	}

	routes := map[string]*string{
		"GET /pets/{id}":        integrate(respond.FunctionArn, "2.0"),
		"GET /pets/special":     integrate(respond.FunctionArn, "1.0"),
		"ANY /files/{path+}":    integrate(echo.FunctionArn, "2.0"),
		"DELETE /pets/{id}":     integrate(aws.String("arn:aws:lambda:us-east-1:000000000000:function:fail"), "2.0"),
		"GET /missing-function": integrate(aws.String("arn:aws:lambda:us-east-1:000000000000:function:gone"), "2.0"),
	}
	for key, target := range routes {
		if _, err = c.CreateRoute(
			ctx,
			&apigw.CreateRouteInput{ApiId: id, RouteKey: aws.String(key), Target: target},
		); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: aws.String("v1"), AutoDeploy: aws.Bool(true),
		StageVariables: map[string]string{"color": "blue"},
	}); err != nil {
		t.Fatal(err)
	}

	status, header, body = call(t, s, "GET", aws.ToString(id), "/v1/pets/42", nil, "")
	if status != http.StatusCreated || header.Get("X-Handler") != "respond" ||
		header.Get("Set-Cookie") != "flavor=oat" {
		t.Fatalf("%d %v %s", status, header, body)
	}

	var event struct {
		PathParameters map[string]string
		StageVariables map[string]string
		RouteKey       string
		RawPath        string
	}
	if err = json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatal(err)
	}

	if event.RouteKey != "GET /pets/{id}" || event.PathParameters["id"] != "42" || event.RawPath != "/v1/pets/42" ||
		event.StageVariables["color"] != "blue" {
		t.Fatalf("%+v", event)
	}

	// A literal segment outranks a parameter; payload format 1.0 carries
	// multi-value maps and the resource path.
	status, _, body = call(t, s, "GET", aws.ToString(id), "/v1/pets/special?x=1&x=2", nil, "")
	var v1 struct {
		MultiValueQueryStringParameters map[string][]string
		Version                         string
		Resource                        string
		HTTPMethod                      string `json:"httpMethod"`
	}
	if err = json.Unmarshal([]byte(body), &v1); err != nil || status != http.StatusCreated {
		t.Fatalf("%d %s %v", status, body, err)
	}

	if v1.Version != "1.0" || v1.Resource != "/pets/special" || v1.HTTPMethod != "GET" ||
		len(v1.MultiValueQueryStringParameters["x"]) != 2 {
		t.Fatalf("%+v", v1)
	}

	status, _, body = call(t, s, "PUT", aws.ToString(id), "/v1/files/a/b/c.txt", nil, "")
	if status != http.StatusOK || !strings.Contains(body, `"path":"a/b/c.txt"`) {
		t.Fatalf("%d %s", status, body)
	}

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"DELETE", "/v1/pets/1", http.StatusInternalServerError},
		{"GET", "/v1/missing-function", http.StatusInternalServerError},
		{"POST", "/v1/pets/1", http.StatusNotFound},
		{"GET", "/v2/pets/1", http.StatusNotFound},
	} {
		if got, _, got2 := call(t, s, tc.method, aws.ToString(id), tc.path, nil, ""); got != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, got, got2)
		}
	}

	// The API's own host name reaches it too.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/v1/pets/7", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = aws.ToString(id) + ".execute-api.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("host addressing: %d", resp.StatusCode)
	}
}

func TestHTTPAPIProxyCorsAndLogs(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := apigwClient(s)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Path", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(r.Method + " " + r.URL.RawQuery))
	}))
	t.Cleanup(backend.Close)

	api, err := c.CreateApi(ctx, &apigw.CreateApiInput{
		Name: aws.String("proxy"), ProtocolType: apigwtypes.ProtocolTypeHttp,
		CorsConfiguration: &apigwtypes.Cors{
			AllowOrigins: []string{"https://app.example"}, AllowMethods: []string{"GET", "POST"},
			AllowHeaders: []string{"content-type"}, ExposeHeaders: []string{"x-backend-path"}, MaxAge: aws.Int32(600),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := api.ApiId
	integration, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId:             id,
		IntegrationType:   apigwtypes.IntegrationTypeHttpProxy,
		IntegrationMethod: aws.String("ANY"),
		IntegrationUri: aws.String(
			backend.URL + "/${stageVariables.base}/{proxy}",
		),
		PayloadFormatVersion: aws.String("1.0"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: aws.String("ANY /{proxy+}"),
		Target: aws.String("integrations/" + aws.ToString(integration.IntegrationId)),
	}); err != nil {
		t.Fatal(err)
	}

	group := "arn:aws:logs:us-east-1:000000000000:log-group:api-access"
	if _, err = c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: aws.String("$default"), AutoDeploy: aws.Bool(true),
		StageVariables: map[string]string{"base": "v9"},
		AccessLogSettings: &apigwtypes.AccessLogSettings{
			DestinationArn: aws.String(group),
			Format:         aws.String("$context.httpMethod $context.routeKey $context.status $context.unknown"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	status, header, body := call(t, s, "POST", aws.ToString(id), "/orders/7?q=1",
		map[string]string{"Origin": "https://app.example"}, "x")
	if status != http.StatusAccepted || body != "POST q=1" || header.Get("X-Backend-Path") != "/v9/orders/7" ||
		header.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		header.Get("Access-Control-Expose-Headers") != "x-backend-path" {
		t.Fatalf("%d %v %s", status, header, body)
	}

	status, header, _ = call(t, s, "OPTIONS", aws.ToString(id), "/orders/7", map[string]string{
		"Origin": "https://app.example", "Access-Control-Request-Method": "POST",
	}, "")
	if status != http.StatusNoContent || header.Get("Access-Control-Allow-Methods") != "GET,POST" ||
		header.Get("Access-Control-Max-Age") != "600" {
		t.Fatalf("preflight: %d %v", status, header)
	}

	status, header, _ = call(
		t,
		s,
		"GET",
		aws.ToString(id),
		"/x",
		map[string]string{"Origin": "https://evil.example"},
		"",
	)
	if status != http.StatusAccepted || header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other origin: %d %v", status, header)
	}

	logClient := cloudwatchlogs.New(cloudwatchlogs.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
	})
	events, err := logClient.FilterLogEvents(
		ctx,
		&cloudwatchlogs.FilterLogEventsInput{LogGroupName: aws.String("api-access")},
	)
	if err != nil || len(events.Events) != 3 ||
		aws.ToString(events.Events[0].Message) != "POST ANY /{proxy+} 202 -" {
		t.Fatalf("%+v %v", events, err)
	}
}

func TestHTTPAPIAuthorizers(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := apigwClient(s)
	functions := lambdaClient(s, "test")
	echo := createFunction(t, functions, "echo", "app.echo")
	authorizerFn := createFunction(t, functions, "authorizer", "app.authorize")

	// A Cognito user pool issues the tokens a JWT authorizer checks.
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

	clientID := client.UserPoolClient.ClientId
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
		UserPoolId: pool.Id, ClientId: clientID, AuthFlow: ciptypes.AuthFlowTypeAdminUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "fay", "PASSWORD": "Passw0rd!"},
	})
	if err != nil {
		t.Fatal(err)
	}

	api, err := c.CreateApi(
		ctx,
		&apigw.CreateApiInput{Name: aws.String("secure"), ProtocolType: apigwtypes.ProtocolTypeHttp},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := api.ApiId
	_, err = c.CreateAuthorizer(ctx, &apigw.CreateAuthorizerInput{
		ApiId: id, Name: aws.String("bad"), AuthorizerType: apigwtypes.AuthorizerTypeJwt,
		IdentitySource: []string{"$request.header.Authorization"},
	})
	code(t, err, "BadRequestException")

	jwt, err := c.CreateAuthorizer(ctx, &apigw.CreateAuthorizerInput{
		ApiId: id, Name: aws.String("cognito"), AuthorizerType: apigwtypes.AuthorizerTypeJwt,
		IdentitySource: []string{"$request.header.Authorization"},
		JwtConfiguration: &apigwtypes.JWTConfiguration{
			Issuer: aws.String(s.URL + "/" + aws.ToString(pool.Id)), Audience: []string{aws.ToString(clientID)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	custom, err := c.CreateAuthorizer(ctx, &apigw.CreateAuthorizerInput{
		ApiId: id, Name: aws.String("lambda"), AuthorizerType: apigwtypes.AuthorizerTypeRequest,
		AuthorizerUri: aws.String("arn:aws:apigateway:us-east-1:lambda:path/2015-03-31/functions/" +
			aws.ToString(authorizerFn.FunctionArn) + "/invocations"),
		AuthorizerPayloadFormatVersion: aws.String("2.0"), EnableSimpleResponses: aws.Bool(true),
		IdentitySource: []string{"$request.header.X-Token"},
	})
	if err != nil {
		t.Fatal(err)
	}

	integration, err := c.CreateIntegration(ctx, &apigw.CreateIntegrationInput{
		ApiId: id, IntegrationType: apigwtypes.IntegrationTypeAwsProxy, IntegrationUri: echo.FunctionArn,
		PayloadFormatVersion: aws.String("2.0"),
	})
	if err != nil {
		t.Fatal(err)
	}

	target := aws.String("integrations/" + aws.ToString(integration.IntegrationId))
	for _, r := range []apigw.CreateRouteInput{
		{RouteKey: aws.String("GET /me"), AuthorizationType: apigwtypes.AuthorizationTypeJwt, AuthorizerId: jwt.AuthorizerId},
		{
			RouteKey: aws.String("GET /admin"), AuthorizationType: apigwtypes.AuthorizationTypeJwt,
			AuthorizerId: jwt.AuthorizerId, AuthorizationScopes: []string{"admin"},
		},
		{
			RouteKey: aws.String("GET /custom"), AuthorizationType: apigwtypes.AuthorizationTypeCustom,
			AuthorizerId: custom.AuthorizerId,
		},
		{RouteKey: aws.String("GET /iam"), AuthorizationType: apigwtypes.AuthorizationTypeAwsIam},
	} {
		r.ApiId, r.Target = id, target
		if _, err = c.CreateRoute(ctx, &r); err != nil {
			t.Fatal(aws.ToString(r.RouteKey), err)
		}
	}

	_, err = c.CreateRoute(ctx, &apigw.CreateRouteInput{
		ApiId: id, RouteKey: aws.String("GET /wrong"), AuthorizationType: apigwtypes.AuthorizationTypeCustom,
		AuthorizerId: jwt.AuthorizerId,
	})
	code(t, err, "BadRequestException")

	_, err = c.DeleteAuthorizer(ctx, &apigw.DeleteAuthorizerInput{ApiId: id, AuthorizerId: jwt.AuthorizerId})
	code(t, err, "ConflictException")

	if _, err = c.CreateStage(ctx, &apigw.CreateStageInput{
		ApiId: id, StageName: aws.String("$default"), AutoDeploy: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	idToken := aws.ToString(signedIn.AuthenticationResult.IdToken)
	accessToken := aws.ToString(signedIn.AuthenticationResult.AccessToken)
	for _, tc := range []struct {
		path, header, value string
		status              int
	}{
		{"/me", "", "", http.StatusUnauthorized},
		{"/me", "Authorization", "Bearer not.a.token", http.StatusUnauthorized},
		{"/me", "Authorization", "Bearer " + idToken, http.StatusOK},
		// The access token's client_id stands in for an audience.
		{"/me", "Authorization", accessToken, http.StatusOK},
		{"/admin", "Authorization", accessToken, http.StatusForbidden},
		{"/custom", "", "", http.StatusUnauthorized},
		{"/custom", "X-Token", "wrong", http.StatusForbidden},
		{"/custom", "X-Token", "let-me-in", http.StatusOK},
		{"/iam", "", "", http.StatusForbidden},
		{
			"/iam", "Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/execute-api/aws4_request",
			http.StatusOK,
		},
	} {
		header := map[string]string{}
		if tc.header != "" {
			header[tc.header] = tc.value
		}

		status, _, body := call(t, s, "GET", aws.ToString(id), tc.path, header, "")
		if status != tc.status {
			t.Fatalf("%s with %s: %d %s", tc.path, tc.header, status, body)
		}

		switch {
		case tc.path == "/me" && status == http.StatusOK && strings.HasPrefix(tc.value, "Bearer "):
			if !strings.Contains(body, `"cognito:username":"fay"`) {
				t.Fatalf("claims were not passed: %s", body)
			}
		case tc.path == "/custom" && status == http.StatusOK:
			if !strings.Contains(body, `"lambda":{"user":"fay"}`) {
				t.Fatalf("the authorizer's context was not passed: %s", body)
			}
		}
	}
}

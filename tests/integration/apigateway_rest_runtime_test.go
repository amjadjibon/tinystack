package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func TestRESTAPIThrottling(t *testing.T) {
	s := server(t)
	c := restClient(s)
	ctx := t.Context()
	target, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("throttled")})
	mustAuthorize(t, err)
	account, err := c.GetAccount(ctx, &restapi.GetAccountInput{})
	mustAuthorize(t, err)
	if account.ThrottleSettings == nil || account.ThrottleSettings.BurstLimit != 5000 ||
		account.ThrottleSettings.RateLimit != 10000 {
		t.Fatalf("unexpected account limits: %+v", account.ThrottleSettings)
	}

	fn := createFunction(t, lambdaClient(s, "test"), "rest-throttle", "app.respond")
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
			RestApiId: target.Id, ResourceId: target.RootResourceId, HttpMethod: new(method),
			AuthorizationType: new("NONE"),
		})
		mustAuthorize(t, err)
		_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
			RestApiId: target.Id, ResourceId: target.RootResourceId, HttpMethod: new(method),
			Type: resttypes.IntegrationTypeAwsProxy, IntegrationHttpMethod: new("POST"), Uri: lambdaURI(fn.FunctionArn),
		})
		mustAuthorize(t, err)
	}

	_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: target.Id, StageName: new("prod")})
	mustAuthorize(t, err)
	update := func(ops ...resttypes.PatchOperation) error {
		t.Helper()
		_, err := c.UpdateStage(ctx, &restapi.UpdateStageInput{
			RestApiId: target.Id, StageName: new("prod"), PatchOperations: ops,
		})
		return err
	}
	code(t, update(replace("/*/*/throttling/rateLimit", "-1")), "BadRequestException")
	mustAuthorize(t, update(
		replace("/*/*/throttling/rateLimit", "0"),
		replace("/*/*/throttling/burstLimit", "1"),
		replace("/~1/POST/throttling/burstLimit", "2"),
	))

	check := func(method string, want int) {
		t.Helper()
		status, _, body := call(t, s, method, *target.Id, "/prod", nil, "")
		if status != want {
			t.Fatalf("%s = %d %s, want %d", method, status, body, want)
		}
	}
	check(http.MethodGet, http.StatusCreated)
	check(http.MethodGet, http.StatusTooManyRequests)
	check(http.MethodPost, http.StatusCreated)
	check(http.MethodPost, http.StatusCreated)
	check(http.MethodPost, http.StatusTooManyRequests)

	mustAuthorize(t, update(replace("/~1/GET/throttling/rateLimit", "2")))
	check(http.MethodGet, http.StatusCreated)
	check(http.MethodGet, http.StatusTooManyRequests)
	waitFor(t, "REST method bucket to refill", func() bool {
		status, _, _ := call(t, s, http.MethodGet, *target.Id, "/prod", nil, "")
		return status == http.StatusCreated
	})
}

const authorizerAPIKeyValue = "authorizer-0123456789"

type restAuthorizerRuntime struct {
	fakeRuntime
	calls atomic.Int32
}

func (r *restAuthorizerRuntime) Invoke(
	ctx context.Context, fn *lambda.Function, payload []byte,
) (lambda.Result, error) {
	if fn.Handler != "app.cachedAuthorizer" {
		return r.fakeRuntime.Invoke(ctx, fn, payload)
	}

	var event struct {
		MethodARN string `json:"methodArn"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	r.calls.Add(1)
	out, err := json.Marshal(map[string]any{
		"principalId": "caller", "usageIdentifierKey": authorizerAPIKeyValue,
		"policyDocument": map[string]any{"Statement": []any{map[string]string{
			"Action": "execute-api:Invoke", "Effect": "Allow", "Resource": event.MethodARN,
		}}},
	})
	return lambda.Result{Payload: out}, err
}

func TestRESTAPIAuthorizerCacheAndUsageIdentifier(t *testing.T) {
	runtime := new(restAuthorizerRuntime)
	s := serverWith(t, runtime)
	ctx := t.Context()
	c := restClient(s)
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{
		Name: new("cached"), ApiKeySource: resttypes.ApiKeySourceTypeAuthorizer,
	})
	mustAuthorize(t, err)
	functions := lambdaClient(s, "test")
	authFn := createFunction(t, functions, "rest-cache-authorizer", "app.cachedAuthorizer")
	fn := createFunction(t, functions, "rest-cache-integration", "app.respond")
	auth, err := c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
		RestApiId: api.Id, Name: new("cached"), Type: resttypes.AuthorizerTypeRequest,
		AuthorizerUri:                lambdaURI(authFn.FunctionArn),
		IdentitySource:               new("method.request.header.X-Token, method.request.querystring.client"),
		AuthorizerResultTtlInSeconds: new(int32(60)),
	})
	mustAuthorize(t, err)
	for _, path := range []string{"first", "second"} {
		resource, resourceErr := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: new(path),
		})
		mustAuthorize(t, resourceErr)
		_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
			RestApiId: api.Id, ResourceId: resource.Id, HttpMethod: new(http.MethodGet),
			AuthorizationType: new("CUSTOM"),
			AuthorizerId:      auth.Id, ApiKeyRequired: true,
		})
		mustAuthorize(t, err)
		_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
			RestApiId: api.Id, ResourceId: resource.Id, HttpMethod: new(http.MethodGet),
			Type: resttypes.IntegrationTypeAwsProxy, IntegrationHttpMethod: new("POST"), Uri: lambdaURI(fn.FunctionArn),
		})
		mustAuthorize(t, err)
	}

	_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("prod")})
	mustAuthorize(t, err)
	plan, err := c.CreateUsagePlan(ctx, &restapi.CreateUsagePlanInput{
		Name: new("cached"), ApiStages: []resttypes.ApiStage{{ApiId: api.Id, Stage: new("prod")}},
	})
	mustAuthorize(t, err)
	key, err := c.CreateApiKey(ctx, &restapi.CreateApiKeyInput{
		Name: new("cached"), Value: new(authorizerAPIKeyValue), Enabled: true,
	})
	mustAuthorize(t, err)
	_, err = c.CreateUsagePlanKey(ctx, &restapi.CreateUsagePlanKeyInput{
		UsagePlanId: plan.Id, KeyId: key.Id, KeyType: new("API_KEY"),
	})
	mustAuthorize(t, err)

	check := func(path string, want int, invocations int32) {
		t.Helper()
		got, _, body := call(t, s, http.MethodGet, *api.Id, path, map[string]string{
			"X-Token": "token", "x-api-key": "incorrect-header-key",
		}, "")
		if got != want || runtime.calls.Load() != invocations {
			t.Fatalf("%s = %d %s, invocations %d, want %d and %d", path, got, body,
				runtime.calls.Load(), want, invocations)
		}
	}
	check("/prod/first", http.StatusUnauthorized, 0)
	check("/prod/first?client=a", http.StatusCreated, 1)
	check("/prod/first?client=a", http.StatusCreated, 1)
	check("/prod/second?client=a", http.StatusForbidden, 1)
	check("/prod/second?client=b", http.StatusCreated, 2)

	_, err = c.FlushStageAuthorizersCache(ctx, &restapi.FlushStageAuthorizersCacheInput{
		RestApiId: api.Id, StageName: new("prod"),
	})
	mustAuthorize(t, err)
	check("/prod/second?client=a", http.StatusCreated, 3)
}

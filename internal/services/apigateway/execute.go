package apigateway

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// call is one request to a REST API as it runs.
type call struct {
	scope     request.Scope
	req       *execute.Request
	api       *restAPI
	stage     *api.Stage
	snapshot  *deployment
	resource  *api.Resource
	method    *api.Method
	params    map[string]string
	requestID string
	// path is the request path after the stage.
	path             string
	authorizerError  string
	integrationError string
	integrationCode  int
	// integrated is how long the integration took, when the request reached it.
	integrated time.Duration
}

// Execute runs a request to the REST API id names: the first path segment
// names the stage, whose deployment's most specific resource and method
// (or ANY) answer it after the method's authorization.
func (s *Service) Execute(ctx context.Context, id string, r *execute.Request) (*execute.Response, bool, error) {
	c := &call{req: r, requestID: uuid.NewV4().String()}
	started := time.Now()
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		loc, ok, err := storage.Read[location](tx, indexKey(id))
		if err != nil || !ok {
			return err
		}

		found, c.scope = true, request.Scope{AccountID: loc.Account, Region: loc.Region}
		if c.api, err = loadAPI(tx, c.scope, &id); err != nil {
			return err
		}

		name, rest, _ := strings.Cut(strings.TrimPrefix(r.Path, "/"), "/")
		c.path = "/" + rest
		st, ok, err := storage.Read[api.Stage](tx, owned(c.scope, kindStage, id, name))
		if err != nil || !ok {
			return err
		}

		c.stage = st
		c.snapshot, _, err = storage.Read[deployment](
			tx,
			owned(c.scope, kindDeployment, id, common.Value(st.DeploymentId)),
		)
		return err
	})
	if err != nil || !found {
		return nil, found, err
	}

	resp := s.run(ctx, c)
	resp.Header.Set("X-Amzn-Requestid", c.requestID)
	s.accessLog(ctx, c, resp)
	s.publish(c, resp, started, c.integrated)
	return resp, true, nil
}

func (s *Service) run(ctx context.Context, c *call) *execute.Response {
	if common.Value(c.api.API.DisableExecuteApiEndpoint) || c.stage == nil || c.snapshot == nil {
		return execute.Message(http.StatusForbidden, "Forbidden")
	}

	c.resource, c.method, c.params = matchMethod(c.snapshot.Resources, c.req.Method, c.path)
	if c.method == nil {
		return execute.Message(http.StatusForbidden, "Missing Authentication Token")
	}

	authorizer, denied := s.authorize(ctx, c)
	if denied != nil {
		return denied
	}

	if keyDenied := s.checkAPIKey(ctx, c, authorizer); keyDenied != nil {
		return keyDenied
	}

	integrating := time.Now()
	resp := s.integrate(ctx, c, authorizer)
	c.integrated = time.Since(integrating)
	return resp
}

// matchMethod picks the most specific resource with a method for the
// request: literal path segments outrank parameters, which outrank greedy
// parameters, and the request's method outranks ANY.
func matchMethod(
	resources map[string]*api.Resource,
	httpMethod, path string,
) (resource *api.Resource, m *api.Method, params map[string]string) {
	var bestRank []int
	for _, id := range slices.Sorted(maps.Keys(resources)) {
		r := resources[id]
		bound, rank, ok := proxy.Bind(common.Value(r.Path), path)
		if !ok {
			continue
		}

		candidate, exact := r.ResourceMethods[httpMethod]
		if !exact {
			var anyMethod bool
			if candidate, anyMethod = r.ResourceMethods[methodAny]; !anyMethod {
				continue
			}
		} else {
			rank = append(rank, 1)
		}

		if m == nil || slices.Compare(rank, bestRank) > 0 {
			resource, m, params, bestRank = r, &candidate, bound, rank
		}
	}

	return resource, m, params
}

// methodARN is the ARN authorizer policies name the request by.
func (c *call) methodARN() string {
	return execute.APIResource(c.scope, *c.api.API.Id, *c.stage.StageName, c.req.Method, c.path).ARN
}

// integrationARN is the ARN of the method that invokes a function, as function policies name it in aws:SourceArn:
// the resource's path as configured, not as requested.
func (c *call) integrationARN() string {
	path := common.Value(c.resource.Path)
	return execute.APIResource(c.scope, *c.api.API.Id, *c.stage.StageName, c.req.Method, path).ARN
}

// identityValue reads an identity source such as method.request.header.Authorization.
func identityValue(c *call, source string) string {
	switch {
	case strings.HasPrefix(source, "method.request.header."):
		return c.req.Header.Get(strings.TrimPrefix(source, "method.request.header."))
	case strings.HasPrefix(source, "method.request.querystring."):
		return c.req.Query.Get(strings.TrimPrefix(source, "method.request.querystring."))
	case strings.HasPrefix(source, "stageVariables."):
		return c.stage.Variables[strings.TrimPrefix(source, "stageVariables.")]
	default:
		return contextVariable(c, nil, strings.TrimPrefix(source, "context."))
	}
}

// authorize runs the method's authorization and returns what the
// integration receives as requestContext.authorizer, or the response that
// denies the request.
func (s *Service) authorize(ctx context.Context, c *call) (map[string]any, *execute.Response) {
	unauthorized := execute.Message(http.StatusUnauthorized, "Unauthorized")
	switch common.Value(c.method.AuthorizationType) {
	case authIAM:
		target := c.invokeTarget()
		denied := c.req.AuthorizeIAM(ctx, &target, "Missing Authentication Token")
		if denied != nil {
			c.authorizerError = string(denied.Body)
		}

		return nil, denied
	case authCognito:
		authorizer, denied := s.cognitoAuthorize(ctx, c)
		if denied != nil {
			return nil, denied
		}

		// A valid token does not stand in for an Allow: the resource policy must grant access itself.
		return authorizer, c.authorizePolicy(ctx, execute.Anonymous)
	case authCustom:
		// The resource policy's explicit denials apply before the authorizer runs.
		if denied := c.authorizePolicy(ctx, execute.Granted); denied != nil {
			return nil, denied
		}

		a := c.snapshot.Authorizers[common.Value(c.method.AuthorizerId)]
		var values []string
		for _, source := range sources(a) {
			v := identityValue(c, source)
			if v == "" {
				c.authorizerError = "missing identity source " + source
				return nil, unauthorized
			}

			values = append(values, v)
		}

		if common.Value(a.Type) == typeToken {
			if p := common.Value(
				a.IdentityValidationExpression,
			); p != "" &&
				!regexp.MustCompile("^(?:"+p+")$").MatchString(values[0]) {
				c.authorizerError = "the token does not match the identity validation expression"
				return nil, unauthorized
			}
		}

		return s.lambdaAuthorize(ctx, c, a, values)
	}

	// NONE: no authorizer vouches for the caller, so the resource policy alone must allow it.
	return nil, c.authorizePolicy(ctx, execute.Anonymous)
}

// cognitoAuthorize checks the request's token against the authorizer's user
// pools. A method with scopes needs an access token with one of them.
func (s *Service) cognitoAuthorize(ctx context.Context, c *call) (map[string]any, *execute.Response) {
	unauthorized := execute.Message(http.StatusUnauthorized, "Unauthorized")
	a := c.snapshot.Authorizers[common.Value(c.method.AuthorizerId)]
	token := strings.TrimSpace(identityValue(c, common.Value(a.IdentitySource)))
	if len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
		token = strings.TrimSpace(token[7:])
	}

	if token == "" {
		c.authorizerError = "missing token"
		return nil, unauthorized
	}

	var claims map[string]any
	for _, arn := range a.ProviderARNs {
		_, poolID, _ := strings.Cut(arn, ":userpool/")
		verified, err := s.pools.Verify(ctx, poolID, token)
		if err == nil {
			claims = verified
			break
		}
	}

	if claims == nil {
		c.authorizerError = "invalid token"
		return nil, unauthorized
	}

	if len(c.method.AuthorizationScopes) > 0 {
		scopes := proxy.Scopes(claims)
		if claims["token_use"] != "access" ||
			!slices.ContainsFunc(
				c.method.AuthorizationScopes,
				func(sc string) bool { return slices.Contains(scopes, sc) },
			) {
			c.authorizerError = "insufficient scopes"
			return nil, unauthorized
		}
	}

	return map[string]any{"claims": proxy.Strings(claims)}, nil
}

// lambdaAuthorize runs a Lambda TOKEN or REQUEST authorizer and applies the
// IAM policy it returns. Its results are not cached.
func (s *Service) lambdaAuthorize(
	ctx context.Context,
	c *call,
	a *api.Authorizer,
	values []string,
) (map[string]any, *execute.Response) {
	failed := execute.Message(http.StatusInternalServerError, "Internal Server Error")
	methodARN := c.methodARN()
	event := map[string]any{"type": common.Value(a.Type), "methodArn": methodARN}
	if common.Value(a.Type) == typeToken {
		event["authorizationToken"] = values[0]
	} else {
		data, err := proxy.EventV1(c.req, c.proxyContext(nil), c.params, c.stage.Variables)
		if err == nil {
			err = json.Unmarshal(data, &event)
		}

		if err != nil {
			return nil, failed
		}

		event["type"], event["methodArn"] = typeRequest, methodARN
	}

	payload, _ := json.Marshal(event)
	arn, _ := proxy.LambdaARN(common.Value(a.AuthorizerUri))
	err := s.authorizer.Authorize(ctx, execute.AuthorizerInvocation(
		c.scope, *c.api.API.Id, common.Value(a.Id), common.Value(a.AuthorizerCredentials), arn))
	var out []byte
	var functionError bool
	if err == nil {
		out, functionError, err = s.invoke(ctx, c, arn, payload, maxTimeout*time.Millisecond)
	}

	if err != nil {
		c.authorizerError = err.Error()
		return nil, failed
	}

	if functionError {
		// A function that raises "Unauthorized" rejects the token.
		var raised struct {
			ErrorMessage string `json:"errorMessage"`
		}
		if json.Unmarshal(out, &raised) == nil && raised.ErrorMessage == "Unauthorized" {
			c.authorizerError = "Unauthorized"
			return nil, execute.Message(http.StatusUnauthorized, "Unauthorized")
		}

		c.authorizerError = "the authorizer function failed"
		return nil, failed
	}

	p, ok := proxy.ReadPolicy(out, methodARN)
	switch {
	case !ok:
		c.authorizerError = "invalid authorizer response"
		return nil, failed
	case p.Denied:
		c.authorizerError = "denied"
		return nil, execute.Message(http.StatusForbidden,
			"User is not authorized to access this resource with an explicit deny")
	case !p.Allowed && (!c.hasPolicy() || c.authorizePolicy(ctx, execute.Anonymous) != nil):
		// A policy that neither allows nor denies leaves the decision to a resource policy Allow.
		c.authorizerError = "not allowed"
		return nil, execute.Message(http.StatusForbidden, "User is not authorized to access this resource")
	}

	authorizer := map[string]any{"principalId": p.Principal}
	maps.Copy(authorizer, p.Context)
	return authorizer, nil
}

func (c *call) proxyContext(authorizer map[string]any) *proxy.Context {
	domain, _, _ := strings.Cut(c.req.Host, ":")
	return &proxy.Context{
		IAM:        common.Value(c.method.AuthorizationType) == authIAM,
		Authorizer: authorizer, AccountID: c.scope.AccountID, APIID: *c.api.API.Id, Stage: *c.stage.StageName,
		RequestID: c.requestID, RouteKey: common.Value(c.resource.Id), Resource: common.Value(c.resource.Path),
		Path: "/" + *c.stage.StageName + c.path, ResourcePath: c.path, DomainName: domain,
	}
}

// integrate runs the method's integration.
func (s *Service) integrate(ctx context.Context, c *call, authorizer map[string]any) *execute.Response {
	i := c.method.MethodIntegration
	timeout := time.Duration(common.Value(i.TimeoutInMillis)) * time.Millisecond
	uri := proxy.Substitute(common.Value(i.Uri), c.stage.Variables)
	switch common.Value(i.Type) {
	case typeMock:
		return mock(c, i)
	case typeHTTP:
		method := common.Value(i.HttpMethod)
		if method == methodAny {
			method = c.req.Method
		}

		for name, v := range c.params {
			uri = strings.ReplaceAll(strings.ReplaceAll(uri, "{"+name+"+}", v), "{"+name+"}", v)
		}

		if c.req.RawQuery != "" {
			uri += map[bool]string{true: "&", false: "?"}[strings.Contains(uri, "?")] + c.req.RawQuery
		}

		resp := proxy.Forward(ctx, method, uri, c.req.Header, c.req.Body, timeout)
		c.integrationCode = resp.Status
		return resp
	}

	event, err := proxy.EventV1(c.req, c.proxyContext(authorizer), c.params, c.stage.Variables)
	if err != nil {
		return execute.Message(http.StatusInternalServerError, "Internal server error")
	}

	arn, _ := proxy.LambdaARN(uri)
	var out []byte
	var functionError bool
	// A role invokes the function as that role; otherwise the function's policy must allow API Gateway.
	delivery := catalog.ServiceDelivery{
		Principal:     "apigateway.amazonaws.com",
		SourceARN:     c.integrationARN(),
		SourceAccount: c.scope.AccountID,
		Target:        arn,
	}
	credentials := common.Value(i.Credentials)
	if strings.Contains(credentials, ":role/") {
		delivery.Target = ""
		delivery.Role = &catalog.DeliveryRole{
			ARN: credentials, Session: "APIGateway-" + *c.api.API.Id, Service: "lambda", Operation: "Invoke",
			Params: map[string]any{"FunctionName": arn, "InvocationType": "RequestResponse"},
		}
	}

	// Credentials that are not a role, arn:aws:iam::*:user/*, invoke as the caller, whom the request authorized.
	if credentials == "" || delivery.Role != nil {
		err = s.authorizer.Authorize(ctx, delivery)
	}

	if err == nil {
		out, functionError, err = s.invoke(ctx, c, arn, event, timeout)
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.integrationError = "timed out"
		return execute.Message(http.StatusGatewayTimeout, "Endpoint request timed out")
	case err != nil:
		c.integrationError = err.Error()
		return execute.Message(http.StatusInternalServerError, "Internal server error")
	}

	c.integrationCode = http.StatusOK
	return proxy.ResponseV1(out, functionError, http.StatusBadGateway)
}

// mock answers a MOCK integration: the status comes from the request
// template's {"statusCode": ...} (200 without one), and the integration
// response it selects supplies the status, literal header values
// ('value'), and body template, which is returned as it is.
func mock(c *call, i *api.Integration) *execute.Response {
	status := http.StatusOK
	for _, template := range i.RequestTemplates {
		var t struct {
			StatusCode int `json:"statusCode"`
		}
		if json.Unmarshal([]byte(template), &t) == nil && t.StatusCode != 0 {
			status = t.StatusCode
		}
	}

	c.integrationCode = status
	var chosen *api.IntegrationResponse
	for _, code := range slices.Sorted(maps.Keys(i.IntegrationResponses)) {
		r := i.IntegrationResponses[code]
		pattern := common.Value(r.SelectionPattern)
		if pattern == "" && chosen == nil {
			chosen = &r
		}

		if pattern != "" && regexp.MustCompile("^(?:"+pattern+")$").MatchString(strconv.Itoa(status)) {
			chosen = &r
			break
		}
	}

	if chosen == nil {
		c.integrationError = "no integration response matched"
		return execute.Message(http.StatusInternalServerError, "Internal server error")
	}

	code, _ := strconv.Atoi(common.Value(chosen.StatusCode))
	resp := &execute.Response{Status: code, Header: http.Header{"Content-Type": {"application/json"}}}
	for k, v := range chosen.ResponseParameters {
		if header, ok := strings.CutPrefix(
			k,
			"method.response.header.",
		); ok && len(v) >= 2 &&
			strings.HasPrefix(v, "'") {
			resp.Header.Set(header, strings.Trim(v, "'"))
		}
	}

	if body, ok := chosen.ResponseTemplates["application/json"]; ok {
		resp.Body = []byte(body)
	}

	return resp
}

// invoke runs a Lambda function synchronously in its own account and region.
func (s *Service) invoke(
	ctx context.Context,
	c *call,
	arn string,
	payload []byte,
	timeout time.Duration,
) (out []byte, functionError bool, err error) {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 {
		return nil, false, errors.New("invalid function ARN " + arn)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ctx = request.WithMetadata(ctx, request.Metadata{
		Scope: request.Scope{AccountID: parts[4], Region: parts[3]}, RequestID: c.requestID, Service: service,
	})
	resp, err := s.functions.Invoke(ctx, &lambdaapi.InvocationRequest{FunctionName: &arn, Payload: payload})
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, context.DeadlineExceeded
		}

		return nil, false, err
	}

	return resp.Payload, resp.FunctionError != nil, nil
}

// contextVariable returns a $context variable's value for a request and, once
// it is known, its response, or "" for one TinyStack does not report.
func contextVariable(c *call, resp *execute.Response, name string) string {
	r := c.req
	switch name {
	case "requestId", "extendedRequestId":
		return c.requestID
	case "apiId":
		return common.Value(c.api.API.Id)
	case "accountId":
		return c.scope.AccountID
	case "stage":
		if c.stage != nil {
			return common.Value(c.stage.StageName)
		}
	case "httpMethod":
		return r.Method
	case "path":
		return r.Path
	case "resourcePath":
		if c.resource != nil {
			return common.Value(c.resource.Path)
		}
	case "resourceId":
		if c.resource != nil {
			return common.Value(c.resource.Id)
		}
	case "protocol":
		return r.Protocol
	case "requestTime":
		return r.Time.Format("02/Jan/2006:15:04:05 -0700")
	case "requestTimeEpoch":
		return strconv.FormatInt(r.Time.UnixMilli(), 10)
	case "identity.sourceIp":
		return r.SourceIP
	case "identity.userAgent":
		return r.Header.Get("User-Agent")
	case "authorizer.error":
		return c.authorizerError
	case "integrationErrorMessage", "integration.error":
		return c.integrationError
	case "integrationStatus", "integration.status":
		if c.integrationCode != 0 {
			return strconv.Itoa(c.integrationCode)
		}
	}

	if resp != nil {
		switch name {
		case "status":
			return strconv.Itoa(resp.Status)
		case "responseLength":
			return strconv.Itoa(len(resp.Body))
		}
	}

	return ""
}

// accessLog writes the stage's access log line for a request, if the stage
// has access logging, to a stream named after the stage.
func (s *Service) accessLog(ctx context.Context, c *call, resp *execute.Response) {
	if c.stage == nil || c.stage.AccessLogSettings == nil {
		return
	}

	scope, group, ok := proxy.LogGroup(common.Value(c.stage.AccessLogSettings.DestinationArn))
	if !ok {
		return
	}

	line := proxy.FormatLog(common.Value(c.stage.AccessLogSettings.Format), func(name string) string {
		return contextVariable(c, resp, name)
	})
	// A failure to log does not fail the request.
	_ = s.logs.Append(context.WithoutCancel(ctx), scope, group, common.Value(c.stage.StageName), []string{line})
}

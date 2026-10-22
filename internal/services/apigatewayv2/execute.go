package apigatewayv2

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// call is one request to an API as it runs.
type call struct {
	scope     request.Scope
	req       *execute.Request
	api       *api.Api
	stage     *api.Stage
	snapshot  *deployment
	route     *api.Route
	params    map[string]string
	requestID string
	// path is the request path after the stage.
	path string
	// authorizerError is why an authorizer failed, for access logs.
	authorizerError string
	// integrationError is why the integration failed, for access logs.
	integrationError string
	integrationCode  int
	// integrated is how long the integration took, when the request reached it.
	integrated time.Duration
}

// Execute runs a request to the HTTP API id names: it picks the stage from
// the first path segment (or the $default stage), matches a route of the
// stage's deployment, runs the route's authorizer and integration, and
// applies the API's CORS configuration.
func (s *Service) Execute(ctx context.Context, id string, r *execute.Request) (*execute.Response, bool, error) {
	c := &call{req: r, requestID: newRequestID()}
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

		return c.resolveStage(tx)
	})
	if err != nil || !found {
		return nil, found, err
	}

	resp := s.run(ctx, c)
	resp.Header.Set("Apigw-Requestid", c.requestID)
	s.accessLog(ctx, c, resp)
	s.publish(c, resp, started, c.integrated)
	return resp, true, nil
}

func newRequestID() string {
	return strings.ToUpper(common.RandomID(8)) + "=" // shaped like API Gateway's
}

// resolveStage picks the stage the request's first path segment names, or
// else the $default stage, and loads its deployment.
func (c *call) resolveStage(tx storage.ReadTx) error {
	first, rest, _ := strings.Cut(strings.TrimPrefix(c.req.Path, "/"), "/")
	name, path := first, "/"+rest
	if first == defaultStage || first == "" {
		name = ""
	}

	st, found, err := storage.Read[api.Stage](tx, owned(c.scope, kindStage, *c.api.ApiId, name))
	if err != nil {
		return err
	}

	if !found || name == "" {
		name, path = defaultStage, c.req.Path
		if st, found, err = storage.Read[api.Stage](tx, owned(c.scope, kindStage, *c.api.ApiId, name)); err != nil {
			return err
		}
	}

	if !found || st.DeploymentId == nil {
		return nil
	}

	c.stage, c.path = st, path
	c.snapshot, _, err = storage.Read[deployment](tx, owned(c.scope, kindDeployment, *c.api.ApiId, *st.DeploymentId))
	return err
}

// run answers the request.
func (s *Service) run(ctx context.Context, c *call) *execute.Response {
	r := c.req
	if common.Value(c.api.ProtocolType) == protocolWebSocket {
		return execute.Message(http.StatusBadRequest, "WebSocket upgrade required")
	}
	if common.Value(c.api.DisableExecuteApiEndpoint) {
		return execute.Message(http.StatusForbidden, "Forbidden")
	}

	if c.stage == nil || c.snapshot == nil {
		return execute.Message(http.StatusNotFound, "Not Found")
	}

	origin, preflight := corsRequest(c.api.CorsConfiguration, r)
	if preflight {
		return preflightResponse(c.api.CorsConfiguration, origin)
	}

	c.route, c.params = matchRoute(c.snapshot.Routes, r.Method, c.path)
	if c.route == nil {
		return withCors(c.api.CorsConfiguration, origin, execute.Message(http.StatusNotFound, "Not Found"))
	}

	authorizer, denied := s.authorize(ctx, c)
	if denied != nil {
		return withCors(c.api.CorsConfiguration, origin, denied)
	}

	integrating := time.Now()
	resp := s.integrate(ctx, c, authorizer)
	c.integrated = time.Since(integrating)
	return withCors(c.api.CorsConfiguration, origin, resp)
}

// matchRoute picks the most specific route for a request: literal segments
// outrank parameters, which outrank greedy parameters, and a route for the
// method outranks ANY. $default takes what no other route matches.
func matchRoute(routes []api.Route, method, path string) (route *api.Route, params map[string]string) {
	var bestRank []int
	for i := range routes {
		routeMethod, template, ok := strings.Cut(common.Value(routes[i].RouteKey), " ")
		if !ok || (routeMethod != method && routeMethod != methodAny) {
			continue
		}

		bound, rank, matched := proxy.Bind(template, path)
		if !matched {
			continue
		}

		if routeMethod != methodAny {
			rank = append(rank, 1)
		}

		if route == nil || slices.Compare(rank, bestRank) > 0 {
			route, params, bestRank = &routes[i], bound, rank
		}
	}

	if route != nil {
		return route, params
	}

	for i := range routes {
		if common.Value(routes[i].RouteKey) == defaultRoute {
			return &routes[i], nil
		}
	}

	return nil, nil
}

// corsRequest returns a request's origin, if the API allows it, and whether
// the request is a CORS preflight, which API Gateway answers itself.
func corsRequest(cors *api.Cors, r *execute.Request) (string, bool) {
	origin := r.Header.Get("Origin")
	if cors == nil || origin == "" {
		return "", false
	}

	preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
	if !slices.Contains(cors.AllowOrigins, "*") && !slices.Contains(cors.AllowOrigins, origin) {
		return "", preflight
	}

	return origin, preflight
}

func preflightResponse(cors *api.Cors, origin string) *execute.Response {
	resp := &execute.Response{Status: http.StatusNoContent, Header: http.Header{}}
	if origin == "" {
		return resp
	}

	h := resp.Header
	h.Set("Access-Control-Allow-Origin", allowedOrigin(cors, origin))
	if len(cors.AllowMethods) > 0 {
		h.Set("Access-Control-Allow-Methods", strings.Join(cors.AllowMethods, ","))
	}

	if len(cors.AllowHeaders) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(cors.AllowHeaders, ","))
	}

	if cors.MaxAge != nil && *cors.MaxAge >= 0 {
		h.Set("Access-Control-Max-Age", strconv.FormatInt(*cors.MaxAge, 10))
	}

	if common.Value(cors.AllowCredentials) {
		h.Set("Access-Control-Allow-Credentials", "true")
	}

	return resp
}

func allowedOrigin(cors *api.Cors, origin string) string {
	if slices.Contains(cors.AllowOrigins, "*") {
		return "*"
	}

	return origin
}

// withCors adds the CORS headers of an allowed origin to a response,
// replacing any the integration set.
func withCors(cors *api.Cors, origin string, resp *execute.Response) *execute.Response {
	if resp.Header == nil {
		resp.Header = http.Header{}
	}

	if origin == "" {
		return resp
	}

	resp.Header.Set("Access-Control-Allow-Origin", allowedOrigin(cors, origin))
	if len(cors.ExposeHeaders) > 0 {
		resp.Header.Set("Access-Control-Expose-Headers", strings.Join(cors.ExposeHeaders, ","))
	}

	if common.Value(cors.AllowCredentials) {
		resp.Header.Set("Access-Control-Allow-Credentials", "true")
	}

	return resp
}

// identityValue reads an identity source such as $request.header.Authorization.
func identityValue(c *call, source string) string {
	r := c.req
	switch {
	case strings.HasPrefix(source, "$request.header."):
		return r.Header.Get(strings.TrimPrefix(source, "$request.header."))
	case strings.HasPrefix(source, "$request.querystring."):
		return r.Query.Get(strings.TrimPrefix(source, "$request.querystring."))
	case strings.HasPrefix(source, "$stageVariables."):
		return c.stage.StageVariables[strings.TrimPrefix(source, "$stageVariables.")]
	default:
		return contextVariable(c, nil, strings.TrimPrefix(source, "$context."))
	}
}

// authorize runs the route's authorization and returns what integrations
// receive as requestContext.authorizer, or the response that denies the
// request.
func (s *Service) authorize(ctx context.Context, c *call) (map[string]any, *execute.Response) {
	unauthorized := execute.Message(http.StatusUnauthorized, "Unauthorized")
	forbidden := execute.Message(http.StatusForbidden, "Forbidden")
	switch common.Value(c.route.AuthorizationType) {
	case authIAM:
		method := c.req.Method
		if common.Value(c.route.RouteKey) == "$default" {
			method = "$default"
		}

		target := execute.APIResource(c.scope, *c.api.ApiId, *c.stage.StageName, method, c.path)
		denied := c.req.AuthorizeIAM(ctx, &target, "Forbidden")
		if denied != nil {
			c.authorizerError = string(denied.Body)
		}

		return nil, denied
	case authJWT:
		a := c.snapshot.Authorizers[common.Value(c.route.AuthorizerId)]
		token := identityValue(c, a.IdentitySource[0])
		if token == "" {
			c.authorizerError = "missing token"
			return nil, unauthorized
		}

		claims, err := s.verifier.Verify(
			ctx,
			token,
			common.Value(a.JwtConfiguration.Issuer),
			a.JwtConfiguration.Audience,
		)
		if err != nil {
			c.authorizerError = err.Error()
			return nil, unauthorized
		}

		scopes := proxy.Scopes(claims)
		if len(c.route.AuthorizationScopes) > 0 &&
			!slices.ContainsFunc(
				c.route.AuthorizationScopes,
				func(sc string) bool { return slices.Contains(scopes, sc) },
			) {
			c.authorizerError = "insufficient scopes"
			return nil, forbidden
		}

		return map[string]any{"claims": proxy.Strings(claims), "scopes": scopes}, nil
	case authCustom:
		return s.lambdaAuthorize(ctx, c)
	}

	return nil, nil
}

// lambdaAuthorize runs a Lambda REQUEST authorizer. Its results are not
// cached: the function runs for every request.
func (s *Service) lambdaAuthorize(ctx context.Context, c *call) (map[string]any, *execute.Response) {
	a := c.snapshot.Authorizers[common.Value(c.route.AuthorizerId)]
	var sources []string
	for _, source := range a.IdentitySource {
		v := identityValue(c, source)
		if v == "" {
			c.authorizerError = "missing identity source " + source
			return nil, execute.Message(http.StatusUnauthorized, "Unauthorized")
		}

		sources = append(sources, v)
	}

	routeARN := execute.APIResource(c.scope, *c.api.ApiId, *c.stage.StageName, c.req.Method, c.path).ARN
	pc := c.proxyContext(nil)
	var event map[string]any
	var err error
	if common.Value(a.AuthorizerPayloadFormatVersion) == payload2 {
		event, err = eventObject(proxy.EventV2(c.req, pc, c.params, c.stage.StageVariables))
		event["type"], event["routeArn"], event["identitySource"] = typeRequest, routeARN, sources
		delete(event, "body")
		delete(event, "isBase64Encoded")
	} else {
		event, err = eventObject(proxy.EventV1(c.req, pc, c.params, c.stage.StageVariables))
		event["type"], event["methodArn"] = typeRequest, routeARN
		event["identitySource"] = strings.Join(sources, ",")
	}

	failed := execute.Message(http.StatusInternalServerError, "Internal Server Error")
	if err != nil {
		return nil, failed
	}

	payload, _ := json.Marshal(event)
	arn, _ := proxy.LambdaARN(common.Value(a.AuthorizerUri))
	// HTTP API authorizers cannot name credentials, so the function's resource policy must allow API Gateway.
	err = s.authorizer.Authorize(ctx, execute.AuthorizerInvocation(
		c.scope, *c.api.ApiId, common.Value(a.AuthorizerId), "", arn))
	var out []byte
	var functionError bool
	if err == nil {
		out, functionError, err = s.invoke(ctx, c, arn, payload, maxTimeout*time.Millisecond)
	}

	if err != nil || functionError {
		c.authorizerError = "authorizer function failed"
		return nil, failed
	}

	allowed, authContext, ok := authorizerResult(out, common.Value(a.EnableSimpleResponses), routeARN)
	switch {
	case !ok:
		c.authorizerError = "invalid authorizer response"
		return nil, failed
	case !allowed:
		c.authorizerError = "denied"
		return nil, execute.Message(http.StatusForbidden, "Forbidden")
	}

	return map[string]any{"lambda": authContext}, nil
}

func eventObject(data []byte, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}

	var event map[string]any
	err = json.Unmarshal(data, &event)
	return event, err
}

// authorizerResult reads a Lambda authorizer's answer: a simple response
// ({"isAuthorized": bool, "context": {...}}) or an IAM policy.
func authorizerResult(payload []byte, simple bool, routeARN string) (allowed bool, ctx map[string]any, ok bool) {
	if !simple {
		p, isPolicy := proxy.ReadPolicy(payload, routeARN)
		return p.Allowed, p.Context, isPolicy
	}

	var out struct {
		IsAuthorized *bool          `json:"isAuthorized"`
		Context      map[string]any `json:"context"`
	}
	if json.Unmarshal(payload, &out) != nil {
		return false, nil, false
	}

	return common.Value(out.IsAuthorized), out.Context, out.IsAuthorized != nil
}

// integrationARN is the ARN of the route that invokes a function, as function policies name it in aws:SourceArn:
// the route's path as configured, not as requested.
func (c *call) integrationARN() string {
	method, route := c.req.Method, common.Value(c.route.RouteKey)
	if route == defaultRoute {
		method = defaultRoute
	}

	_, path, _ := strings.Cut(route, " ")
	return execute.APIResource(c.scope, *c.api.ApiId, *c.stage.StageName, method, path).ARN
}

func (c *call) proxyContext(authorizer map[string]any) *proxy.Context {
	domain := c.req.Host
	if i := strings.IndexByte(domain, ':'); i >= 0 {
		domain = domain[:i]
	}

	resource := ""
	if _, path, ok := strings.Cut(common.Value(c.route.RouteKey), " "); ok {
		resource = path
	}

	path := c.req.Path
	return &proxy.Context{
		IAM:        common.Value(c.route.AuthorizationType) == authIAM,
		Authorizer: authorizer,
		AccountID:  c.scope.AccountID,
		APIID:      *c.api.ApiId,
		Stage:      *c.stage.StageName,
		RequestID:  c.requestID,
		RouteKey:   common.Value(c.route.RouteKey),
		Resource:   resource,
		Path:       path,
		DomainName: domain,
	}
}

// integrate runs the route's integration.
func (s *Service) integrate(ctx context.Context, c *call, authorizer map[string]any) *execute.Response {
	failed := execute.Message(http.StatusInternalServerError, "Internal Server Error")
	i, ok := c.snapshot.Integrations[integrationID(common.Value(c.route.Target))]
	if !ok {
		c.integrationError = "the route has no integration"
		return failed
	}

	timeout := time.Duration(common.Value(i.TimeoutInMillis)) * time.Millisecond
	uri := proxy.Substitute(common.Value(i.IntegrationUri), c.stage.StageVariables)
	if common.Value(i.IntegrationType) == typeHTTP {
		method := common.Value(i.IntegrationMethod)
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

	// Payload format 2.0 reports JWT claims under "jwt" and 1.0 at the top.
	var event []byte
	var err error
	pc := c.proxyContext(authorizer)
	if common.Value(i.PayloadFormatVersion) == payload2 {
		if authorizer != nil && authorizer["claims"] != nil {
			pc.Authorizer = map[string]any{"jwt": authorizer}
		}

		event, err = proxy.EventV2(c.req, pc, c.params, c.stage.StageVariables)
	} else {
		if lambdaContext, isLambda := authorizer["lambda"].(map[string]any); isLambda {
			pc.Authorizer = maps.Clone(lambdaContext)
		}

		event, err = proxy.EventV1(c.req, pc, c.params, c.stage.StageVariables)
	}

	if err != nil {
		return failed
	}

	arn, _ := proxy.LambdaARN(uri)
	// The function's resource policy must allow API Gateway: HTTP API integrations cannot name a role.
	err = s.authorizer.Authorize(ctx, catalog.ServiceDelivery{
		Principal:     "apigateway.amazonaws.com",
		SourceARN:     c.integrationARN(),
		SourceAccount: c.scope.AccountID,
		Target:        arn,
	})
	var out []byte
	var functionError bool
	if err == nil {
		out, functionError, err = s.invoke(ctx, c, arn, event, timeout)
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.integrationError = "timed out"
		return execute.Message(http.StatusServiceUnavailable, "Service Unavailable")
	case err != nil:
		c.integrationError = err.Error()
		return failed
	}

	c.integrationCode = http.StatusOK
	if common.Value(i.PayloadFormatVersion) == payload2 {
		return proxy.ResponseV2(out, functionError)
	}

	return proxy.ResponseV1(out, functionError, http.StatusInternalServerError)
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
		Scope: request.Scope{AccountID: parts[4], Region: parts[3]}, RequestID: c.requestID, Service: "apigateway",
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

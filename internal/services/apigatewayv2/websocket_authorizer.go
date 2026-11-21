package apigatewayv2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
)

var socketIdentitySource = regexp.MustCompile(
	`^(route\.request\.(header|querystring)\.[^\s,]+|\$?stageVariables\.\w+|\$?context\.[\w.]+)$`,
)

func checkProtocolAuthorizer(owner *api.Api, a *api.Authorizer) error {
	if common.Value(owner.ProtocolType) == protocolHTTP {
		return checkAuthorizer(a)
	}

	if common.Value(a.AuthorizerType) != typeRequest || !isFunction(common.Value(a.AuthorizerUri)) {
		return badRequest("WebSocket authorizers require type REQUEST and a Lambda invocation URI.")
	}

	if n := len(common.Value(a.Name)); n < 1 || n > 128 {
		return badRequest("Name must be 1 to 128 characters.")
	}

	if a.AuthorizerPayloadFormatVersion != nil || a.EnableSimpleResponses != nil ||
		a.AuthorizerResultTtlInSeconds != nil || a.JwtConfiguration != nil {
		return badRequest("Payload formats, simple responses, TTL and JWT are only supported for HTTP authorizers.")
	}

	for _, source := range a.IdentitySource {
		if !socketIdentitySource.MatchString(source) || !validSocketIdentity(source) {
			return badRequest("Invalid WebSocket identity source %s.", source)
		}
	}

	return nil
}

func validSocketIdentity(source string) bool {
	name, isContext := strings.CutPrefix(strings.TrimPrefix(source, "$"), "context.")
	if !isContext {
		return true
	}

	switch name {
	case "apiId", "connectedAt", "connectionId", "domainName", "eventType", contextSourceIP, contextUserAgent,
		"messageDirection", "requestId", "requestTime", "requestTimeEpoch", "routeKey", "stage":
		return true
	}

	return false
}

func socketSource(c *call, fields map[string]any, source string) string {
	source = strings.TrimPrefix(source, "$")
	if name, ok := strings.CutPrefix(source, "route.request.header."); ok {
		return c.req.Header.Get(name)
	}

	if name, ok := strings.CutPrefix(source, "route.request.querystring."); ok {
		return c.req.Query.Get(name)
	}

	if name, ok := strings.CutPrefix(source, "stageVariables."); ok {
		return c.stage.StageVariables[name]
	}

	name := strings.TrimPrefix(source, "context.")
	if key, ok := strings.CutPrefix(name, "identity."); ok {
		fields, _ = fields["identity"].(map[string]any)
		name = key
	}

	if value, ok := fields[name]; ok {
		return fmt.Sprint(value)
	}

	return ""
}

func (s *Service) authorizeSocket(
	ctx context.Context, c *call, conn *connection,
) (map[string]any, *execute.Response) {
	switch common.Value(c.route.AuthorizationType) {
	case authIAM:
		target := execute.APIResource(c.scope, *c.api.ApiId, *c.stage.StageName, socketConnect, "")
		return nil, c.req.AuthorizeIAM(ctx, &target, "Forbidden")
	case authCustom:
		return s.authorizeSocketLambda(ctx, c, conn)
	}

	return nil, nil
}

func (s *Service) authorizeSocketLambda(
	ctx context.Context, c *call, conn *connection,
) (map[string]any, *execute.Response) {
	a := c.snapshot.Authorizers[common.Value(c.route.AuthorizerId)]
	event := s.socketPayload(c, conn, socketConnectEvent, nil)
	fields := event["requestContext"].(map[string]any)
	for _, source := range a.IdentitySource {
		if socketSource(c, fields, source) == "" {
			return nil, execute.Message(http.StatusUnauthorized, "Unauthorized")
		}
	}

	methodARN := c.socketRouteARN()
	event["type"], event["methodArn"] = typeRequest, methodARN
	delete(event, "isBase64Encoded")
	payload, err := json.Marshal(event)
	failed := execute.Message(http.StatusInternalServerError, "Internal Server Error")
	if err != nil {
		return nil, failed
	}

	arn, _ := proxy.LambdaARN(common.Value(a.AuthorizerUri))
	if authErr := s.authorizer.Authorize(ctx, execute.AuthorizerInvocation(
		c.scope, *c.api.ApiId, common.Value(a.AuthorizerId), "", arn,
	)); authErr != nil {
		return nil, failed
	}

	out, functionError, err := s.invoke(ctx, c, arn, payload, maxTimeout*time.Millisecond)
	if err != nil {
		return nil, failed
	}

	if functionError {
		var raised struct {
			ErrorMessage string `json:"errorMessage"`
		}
		if json.Unmarshal(out, &raised) == nil && raised.ErrorMessage == "Unauthorized" {
			return nil, execute.Message(http.StatusUnauthorized, "Unauthorized")
		}

		return nil, failed
	}

	policy, ok := proxy.ReadPolicy(out, methodARN)
	if !ok {
		return nil, failed
	}

	if !policy.Allowed {
		return nil, execute.Message(http.StatusForbidden, "Forbidden")
	}

	if policy.Context == nil {
		policy.Context = make(map[string]any)
	}

	policy.Context["principalId"] = policy.Principal
	return policy.Context, nil
}

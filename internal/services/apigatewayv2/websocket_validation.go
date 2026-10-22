package apigatewayv2

import (
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/common"
)

var bodySelection = regexp.MustCompile(`^\$request\.body\.[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func checkProtocolAPI(protocol string, name, selection, ip *string, cors *api.Cors, target, route *string) error {
	if protocol == protocolHTTP {
		return checkAPI(name, selection, ip, cors)
	}

	switch {
	case name != nil && !apiName.MatchString(*name):
		return badRequest("Name must be 1 to 128 characters.")
	case !bodySelection.MatchString(common.Value(selection)):
		return badRequest("WebSocket RouteSelectionExpression must be $request.body.<field> with dotted field paths.")
	case cors != nil || ip != nil || target != nil || route != nil:
		return badRequest("CorsConfiguration, IpAddressType, Target and RouteKey are only supported for HTTP APIs.")
	}

	return nil
}

func checkProtocolRoute(a *api.Api, r *api.Route) error {
	if common.Value(a.ProtocolType) == protocolHTTP {
		return checkRouteKey(common.Value(r.RouteKey))
	}

	key := common.Value(r.RouteKey)
	if key == "" || strings.ContainsAny(key, " \t\r\n/") ||
		(strings.HasPrefix(key, "$") && key != "$connect" && key != "$disconnect" && key != defaultRoute) {
		return badRequest("Invalid WebSocket route key %s.", key)
	}

	if common.Value(r.AuthorizationType) != authNone || len(r.AuthorizationScopes) != 0 || r.AuthorizerId != nil {
		return badRequest("WebSocket routes currently support NONE authorization only.")
	}

	return nil
}

func checkProtocolIntegration(a *api.Api, i *api.Integration) error {
	if common.Value(a.ProtocolType) == protocolHTTP {
		return checkIntegration(i)
	}

	if i.PayloadFormatVersion != nil {
		return badRequest("PayloadFormatVersion is only supported for HTTP APIs.")
	}

	switch common.Value(i.IntegrationType) {
	case typeLambda:
		if !isFunction(common.Value(i.IntegrationUri)) || common.Value(i.IntegrationMethod) != "POST" {
			return badRequest("WebSocket AWS_PROXY integrations require a Lambda URI and POST method.")
		}
	case "MOCK":
		if i.IntegrationUri != nil || i.IntegrationMethod != nil {
			return badRequest("MOCK integrations do not accept IntegrationUri or IntegrationMethod.")
		}
	default:
		return badRequest("WebSocket integrations support AWS_PROXY (Lambda) and MOCK only.")
	}

	if common.Value(i.ConnectionType) != connectionInternet {
		return badRequest("TinyStack supports INTERNET connections only.")
	}

	if t := common.Value(i.TimeoutInMillis); t < minTimeout || t > 29000 {
		return badRequest("WebSocket TimeoutInMillis must be between 50 and 29000.")
	}

	return nil
}

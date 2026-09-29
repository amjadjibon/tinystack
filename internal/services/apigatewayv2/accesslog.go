package apigatewayv2

import (
	"context"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
)

// contextVariable returns a $context variable's value for a request and, once
// it is known, its response, or "" for one TinyStack does not report.
func contextVariable(c *call, resp *execute.Response, name string) string {
	r := c.req
	switch name {
	case "requestId", "extendedRequestId":
		return c.requestID
	case "apiId":
		return value(c.api.ApiId)
	case "accountId":
		return c.scope.AccountID
	case "stage":
		if c.stage != nil {
			return value(c.stage.StageName)
		}
	case "domainName":
		host, _, _ := strings.Cut(r.Host, ":")
		return host
	case "httpMethod":
		return r.Method
	case "path":
		return r.Path
	case "protocol":
		return r.Protocol
	case "routeKey":
		if c.route != nil {
			return value(c.route.RouteKey)
		}
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

	scope, group, ok := proxy.LogGroup(value(c.stage.AccessLogSettings.DestinationArn))
	if !ok {
		return
	}

	line := proxy.FormatLog(value(c.stage.AccessLogSettings.Format), func(name string) string {
		return contextVariable(c, resp, name)
	})
	// A failure to log does not fail the request.
	_ = s.logs.Append(context.WithoutCancel(ctx), scope, group, value(c.stage.StageName), []string{line})
}

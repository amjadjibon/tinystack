// Package proxy holds what API Gateway's REST and HTTP APIs share when they
// run a request: Lambda proxy events and responses (payload formats 1.0 and
// 2.0), forwarding to HTTP backends, and JWT verification for JWT and Cognito
// authorizers.
package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
)

// Context is what an event's requestContext reports about the API.
type Context struct {
	// Authorizer is what an authorizer returned: the claims and scopes of a
	// JWT, or a Lambda authorizer's context.
	Authorizer map[string]any
	AccountID  string
	APIID      string
	Stage      string
	RequestID  string
	// RouteKey is an HTTP API's route key, such as "GET /pets/{id}".
	RouteKey string
	// Resource is the resource path that matched, such as "/pets/{id}".
	Resource string
	// Path is the request path with the stage, as the client sent it.
	Path string
	// ResourcePath is the path payload format 1.0 reports at its top level,
	// when it differs from Path: a REST API's path without the stage.
	ResourcePath string
	DomainName   string
	// IAM enables resolved caller identity fields for an AWS_IAM method or route.
	IAM bool
}

// textTypes are the content types whose bodies events carry as text.
var textTypes = regexp.MustCompile(
	`^(text/.*|application/(json|xml|javascript|x-www-form-urlencoded|.*\+json|.*\+xml))$`,
)

// body returns a request body as an event carries it: text, or base64 for
// binary content.
func body(r *execute.Request) (text *string, encoded bool) {
	if len(r.Body) == 0 {
		return nil, false
	}

	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if (contentType == "" || textTypes.MatchString(contentType)) && utf8.Valid(r.Body) {
		return new(string(r.Body)), false
	}

	return new(base64.StdEncoding.EncodeToString(r.Body)), true
}

// single joins repeated header or query values with commas, as payload
// format 2.0 carries them, and returns nil for none.
func single(values map[string][]string, lower bool) map[string]string {
	if len(values) == 0 {
		return nil
	}

	out := map[string]string{}
	for k, v := range values {
		if lower {
			k = strings.ToLower(k)
		}

		out[k] = strings.Join(v, ",")
	}

	return out
}

// last keeps the last of repeated values, as payload format 1.0's
// single-value maps do.
func last(values map[string][]string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	out := map[string]string{}
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[len(v)-1]
		}
	}

	return out
}

func orNil(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}

	return m
}

// keyPath is the member events report a request's path in.
const keyPath = "path"

const keyAccountID = "accountId"

// timeText is the time format requestContext reports.
const timeText = "02/Jan/2006:15:04:05 -0700"

// EventV2 builds payload format 2.0's event for an HTTP API.
func EventV2(r *execute.Request, c *Context, pathParameters, stageVariables map[string]string) ([]byte, error) {
	text, encoded := body(r)
	headers := r.Header.Clone()
	headers.Del("Cookie")
	var cookies []string
	for _, line := range r.Header.Values("Cookie") {
		for cookie := range strings.SplitSeq(line, ";") {
			if cookie = strings.TrimSpace(cookie); cookie != "" {
				cookies = append(cookies, cookie)
			}
		}
	}

	requestContext := map[string]any{
		keyAccountID: c.AccountID, "apiId": c.APIID, "domainName": c.DomainName,
		"domainPrefix": strings.Split(c.DomainName, ".")[0], "requestId": c.RequestID, "routeKey": c.RouteKey,
		"stage": c.Stage, "time": r.Time.Format(timeText), "timeEpoch": r.Time.UnixMilli(),
		"http": map[string]any{
			"method": r.Method, keyPath: c.Path, "protocol": r.Protocol, "sourceIp": r.SourceIP,
			"userAgent": r.Header.Get("User-Agent"),
		},
	}
	if c.Authorizer != nil {
		requestContext["authorizer"] = c.Authorizer
	}

	if c.IAM && r.Caller != nil && r.Caller.Principal != nil && r.Caller.Principal.ARN != "" {
		authorizer := maps.Clone(c.Authorizer)
		if authorizer == nil {
			authorizer = map[string]any{}
		}

		authorizer["iam"] = iamIdentity(r.Caller)
		requestContext["authorizer"] = authorizer
	}

	event := map[string]any{
		"version": "2.0", "routeKey": c.RouteKey, "rawPath": c.Path, "rawQueryString": r.RawQuery,
		"headers": single(headers, true), "requestContext": requestContext, "isBase64Encoded": encoded,
	}
	optional := map[string]any{
		"cookies": cookies, "queryStringParameters": single(r.Query, false),
		"pathParameters": orNil(pathParameters), "stageVariables": orNil(stageVariables), "body": text,
	}
	for k, v := range optional {
		if !isNil(v) {
			event[k] = v
		}
	}

	return json.Marshal(event)
}

func isNil(v any) bool {
	switch v := v.(type) {
	case []string:
		return v == nil
	case map[string]string:
		return v == nil
	case *string:
		return v == nil
	}

	return v == nil
}

// EventV1 builds payload format 1.0's event, the one REST APIs send and HTTP
// APIs send for 1.0 integrations.
func EventV1(r *execute.Request, c *Context, pathParameters, stageVariables map[string]string) ([]byte, error) {
	text, encoded := body(r)
	requestContext := map[string]any{
		keyAccountID: c.AccountID, "apiId": c.APIID, "domainName": c.DomainName,
		"domainPrefix": strings.Split(c.DomainName, ".")[0], "extendedRequestId": c.RequestID,
		"httpMethod": r.Method, keyPath: c.Path, "protocol": r.Protocol, "requestId": c.RequestID,
		"requestTime": r.Time.Format(timeText), "requestTimeEpoch": r.Time.UnixMilli(),
		"resourcePath": c.Resource, "stage": c.Stage, "resourceId": c.RouteKey,
		"identity": map[string]any{"sourceIp": r.SourceIP, "userAgent": r.Header.Get("User-Agent")},
	}
	if c.Authorizer != nil {
		requestContext["authorizer"] = c.Authorizer
	}

	if c.IAM && r.Caller != nil && r.Caller.Principal != nil && r.Caller.Principal.ARN != "" {
		identity := iamIdentity(r.Caller)
		identity["sourceIp"], identity["userAgent"] = r.SourceIP, r.Header.Get("User-Agent")
		requestContext["identity"] = identity
	}

	resource := c.Resource
	if resource == "" {
		resource = c.RouteKey
	}

	path := c.Path
	if c.ResourcePath != "" {
		path = c.ResourcePath
	}

	event := map[string]any{
		"version": "1.0", "resource": resource, keyPath: path, "httpMethod": r.Method,
		"headers": last(r.Header), "multiValueHeaders": orValues(r.Header),
		"queryStringParameters": last(r.Query), "multiValueQueryStringParameters": orValues(r.Query),
		"requestContext": requestContext, "pathParameters": orNil(pathParameters),
		"stageVariables": orNil(stageVariables), "body": text, "isBase64Encoded": encoded,
	}

	return json.Marshal(event)
}

func orValues(values map[string][]string) map[string][]string {
	if len(values) == 0 {
		return nil
	}

	return values
}

// response is a Lambda proxy integration's structured response.
type response struct {
	StatusCode        any                 `json:"statusCode"`
	Headers           map[string]any      `json:"headers"`
	MultiValueHeaders map[string][]string `json:"multiValueHeaders"`
	Body              *string             `json:"body"`
	Cookies           []string            `json:"cookies"`
	IsBase64Encoded   bool                `json:"isBase64Encoded"`
}

// status reads a statusCode, which functions return as a number or a string.
func status(v any) (int, bool) {
	switch v := v.(type) {
	case float64:
		return int(v), v >= 100 && v <= 599 && v == float64(int(v))
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil && n >= 100 && n <= 599
	}

	return 0, false
}

// Structured decodes a function's structured response (statusCode, headers,
// multiValueHeaders, cookies, body, and isBase64Encoded) and applies it.
func Structured(payload []byte) (*execute.Response, bool) {
	var r response
	if json.Unmarshal(payload, &r) != nil {
		return nil, false
	}

	code, ok := status(r.StatusCode)
	if !ok {
		return nil, false
	}

	out := &execute.Response{Status: code, Header: http.Header{}}
	for k, values := range r.MultiValueHeaders {
		for _, v := range values {
			out.Header.Add(k, v)
		}
	}

	for k, v := range r.Headers {
		switch v := v.(type) {
		case string:
			out.Header.Set(k, v)
		case float64, bool:
			b, _ := json.Marshal(v)
			out.Header.Set(k, string(b))
		}
	}

	for _, c := range r.Cookies {
		out.Header.Add("Set-Cookie", c)
	}

	if r.Body != nil {
		out.Body = []byte(*r.Body)
		if r.IsBase64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(*r.Body)
			if err != nil {
				return nil, false
			}

			out.Body = decoded
		}
	}

	if out.Header.Get("Content-Type") == "" && len(out.Body) > 0 {
		out.Header.Set("Content-Type", "application/json")
	}

	return out, true
}

// ResponseV2 maps a function's payload format 2.0 response: a structured
// response when it has a statusCode, or else a 200 whose JSON body is the
// function's result.
func ResponseV2(payload []byte, functionError bool) *execute.Response {
	if functionError {
		return execute.Message(http.StatusInternalServerError, "Internal Server Error")
	}

	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var probe map[string]json.RawMessage
		if json.Unmarshal(trimmed, &probe) == nil && probe["statusCode"] != nil {
			if out, ok := Structured(trimmed); ok {
				return out
			}

			return execute.Message(http.StatusInternalServerError, "Internal Server Error")
		}
	}

	return &execute.Response{
		Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: trimmed,
	}
}

// ResponseV1 maps a function's payload format 1.0 response, which must be
// structured. REST APIs report a failed or malformed one as 502.
func ResponseV1(payload []byte, functionError bool, failure int) *execute.Response {
	message := "Internal Server Error"
	if failure == http.StatusBadGateway {
		message = "Internal server error"
	}

	if functionError {
		return execute.Message(failure, message)
	}

	if out, ok := Structured(payload); ok {
		return out
	}

	return execute.Message(failure, message)
}

// LambdaARN returns the function ARN an integration URI names: a function
// ARN, or an API Gateway invocation URI
// (arn:aws:apigateway:<region>:lambda:path/2015-03-31/functions/<ARN>/invocations).
func LambdaARN(uri string) (string, bool) {
	if rest, ok := strings.CutPrefix(uri, "arn:"); ok {
		parts := strings.SplitN(rest, ":", 5)
		if len(parts) == 5 && parts[1] == "apigateway" && strings.HasPrefix(parts[4], "path/2015-03-31/functions/") {
			fn := strings.TrimPrefix(parts[4], "path/2015-03-31/functions/")
			fn, ok = strings.CutSuffix(fn, "/invocations")
			return fn, ok && strings.HasPrefix(fn, "arn:") && strings.Contains(fn, ":lambda:")
		}

		return uri, len(parts) >= 5 && parts[1] == "lambda" && strings.Contains(parts[4], "function:")
	}

	return "", false
}

// hopHeaders are not forwarded to or from an HTTP backend.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
	"Transfer-Encoding", "Upgrade", "Host", "Content-Length",
}

// maxBackendBody bounds a backend's response, as API Gateway's 10 MB payload
// limit does.
const maxBackendBody = 10 << 20

// Forward sends a request to an HTTP backend and returns its response. A
// backend that cannot be reached is a 500, and one that does not answer in
// time a 504, as API Gateway reports them.
func Forward(
	ctx context.Context,
	method, url string,
	header http.Header,
	payload []byte,
	timeout time.Duration,
) *execute.Response {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return execute.Message(http.StatusInternalServerError, "Internal Server Error")
	}

	req.Header = header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}

	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- the API's owner chose the backend URL.
	if errors.Is(err, context.DeadlineExceeded) {
		return execute.Message(http.StatusGatewayTimeout, "Service Unavailable")
	}

	if err != nil {
		return execute.Message(http.StatusInternalServerError, "Internal Server Error")
	}

	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return execute.Message(http.StatusBadGateway, "Bad Gateway")
	}

	out := &execute.Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: data}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}

	return out
}

// Substitute replaces ${stageVariables.<name>} in s with the stage's
// variables.
func Substitute(s string, stageVariables map[string]string) string {
	for _, name := range slices.Sorted(maps.Keys(stageVariables)) {
		s = strings.ReplaceAll(s, "${stageVariables."+name+"}", stageVariables[name])
	}

	return s
}

// iamIdentity carries resolved IAM IDs, rather than treating the access key as a user.
func iamIdentity(caller *execute.Caller) map[string]any {
	return map[string]any{
		keyAccountID: caller.AccountID, "accessKey": caller.AccessKey,
		"caller": caller.Principal.UserID, "user": caller.Principal.UserID, "userArn": caller.Principal.ARN,
	}
}

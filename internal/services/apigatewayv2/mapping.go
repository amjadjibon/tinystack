package apigatewayv2

import (
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy/jsonpath"
)

const (
	mappingStageVariables = "stageVariables"
	mappingAppend         = "append"
	mappingOverwrite      = "overwrite"
	mappingRemove         = "remove"
	mappingHeader         = "header"
	mappingQuery          = "querystring"
	mappingPath           = "path"
	mappingStatus         = "statuscode"
	mappingBodyLimit      = 100 * 1024
)

var (
	parameterName   = regexp.MustCompile(`^[^\s,{}]+$`)
	mappingVariable = regexp.MustCompile(`^[\w.-]+$`)
	mappingTemplate = regexp.MustCompile(`\$\{([^{}]+)\}`)
)

func checkMappings(i *api.Integration) error {
	if err := checkParameterMap(i.RequestParameters, false, common.Value(i.IntegrationType) == typeLambda); err != nil {
		return err
	}

	for _, status := range slices.Sorted(maps.Keys(i.ResponseParameters)) {
		if _, ok := mappingStatusCode(status); !ok {
			return badRequest(
				"Invalid response parameter selection key %q. Expected an HTTP status code from 200 to 599.",
				status,
			)
		}

		if err := checkParameterMap(i.ResponseParameters[status], true, false); err != nil {
			return err
		}
	}

	return nil
}

func checkParameterMap(parameters map[string]string, response, lambda bool) error {
	for _, key := range slices.Sorted(maps.Keys(parameters)) {
		action, target, name := mappingTarget(key)
		value := parameters[key]
		if action != mappingAppend && action != mappingOverwrite && action != mappingRemove {
			return invalidMapping(key)
		}

		switch target {
		case mappingHeader:
			if !httpguts.ValidHeaderFieldName(name) {
				return invalidMapping(key)
			}

			if reservedMappingHeader(name) {
				return badRequest("Invalid mapping expression specified: Header %s is reserved.", name)
			}
		case mappingQuery:
			if response || !parameterName.MatchString(name) {
				return invalidMapping(key)
			}
		case mappingPath:
			if response || action != mappingOverwrite || name != "" || strings.Contains(key, ".") {
				return invalidMapping(key)
			}

			if lambda {
				return badRequest(
					"Invalid mapping expression specified: overwrite:path is only supported for HTTP_PROXY integrations.",
				)
			}
		case mappingStatus:
			if !response || action != mappingOverwrite || name != "" || strings.Contains(key, ".") {
				return invalidMapping(key)
			}
		default:
			return invalidMapping(key)
		}

		if action == mappingRemove {
			if value != "" {
				return invalidMapping(value)
			}

			continue
		}

		_, valid := evaluateMapping(value, response, nil)
		if !valid || len(value) > 512 || (target == mappingHeader && !httpguts.ValidHeaderFieldValue(value)) {
			return invalidMapping(value)
		}

		if strings.HasPrefix(value, "$") || strings.Contains(value, "${") {
			continue
		}

		switch target {
		case mappingPath:
			if !strings.HasPrefix(value, "/") {
				return invalidMapping(value)
			}
		case mappingStatus:
			if _, ok := mappingStatusCode(value); !ok {
				return invalidMapping(value)
			}
		}
	}

	return nil
}

func invalidMapping(value string) error {
	return badRequest("Invalid mapping expression specified: Validation Result: warnings : [], errors : "+
		"[Invalid mapping expression specified: %s]", value)
}

func mappingTarget(key string) (action, target, name string) {
	action, destination, _ := strings.Cut(key, ":")
	target, name, _ = strings.Cut(destination, ".")
	return action, target, name
}

func reservedMappingHeader(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range []string{"access-control-", "apigw-", "x-amz-", "x-amzn-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	switch name {
	case "authorization", "connection", "content-encoding", "content-length", "content-location", "forwarded",
		"keep-alive", "origin", "proxy-authenticate", "proxy-authorization", "te", "trailer", "trailers",
		"transfer-encoding", "upgrade", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "via":
		return true
	}

	return false
}

func mappingStatusCode(value string) (int, bool) {
	n, err := strconv.Atoi(value)
	return n, err == nil && len(value) == 3 && n >= 200 && n <= 599
}

// evaluateMapping validates the expression when resolve is nil, or substitutes its variable references.
func evaluateMapping(value string, response bool, resolve func(string) string) (string, bool) {
	substitute := func(source string) (string, bool) {
		if !validMappingSource(source, response) {
			return "", false
		}

		if resolve == nil {
			return "", true
		}

		return resolve(source), true
	}

	if !strings.Contains(value, "${") {
		if strings.HasPrefix(value, "$") {
			return substitute(value[1:])
		}

		return value, true
	}

	valid := true
	if strings.Contains(mappingTemplate.ReplaceAllString(value, ""), "$") {
		return "", false
	}

	result := mappingTemplate.ReplaceAllStringFunc(value, func(match string) string {
		replacement, ok := substitute(match[2 : len(match)-1])
		valid = valid && ok
		return replacement
	})
	return result, valid
}

func validMappingSource(source string, response bool) bool {
	scope, rest, ok := strings.Cut(source, ".")
	if !ok {
		return false
	}

	switch scope {
	case "context":
		return validMappingContext(rest)
	case mappingStageVariables:
		return stageVariable.MatchString(rest)
	case "request":
		if response {
			return false
		}
	case "response":
		if !response {
			return false
		}
	default:
		return false
	}

	switch location, name, _ := strings.Cut(rest, "."); location {
	case mappingHeader:
		return httpguts.ValidHeaderFieldName(name)
	case mappingQuery:
		return !response && parameterName.MatchString(name)
	case mappingPath:
		return !response && (rest == mappingPath || parameterName.MatchString(name))
	default:
		path, ok := mappingBodyPath(rest)
		return ok && jsonpath.Validate(path) == nil
	}
}

// mappingBodyPath accepts body, body.name, and body[index] selections.
func mappingBodyPath(source string) (string, bool) {
	tail, ok := strings.CutPrefix(source, "body")
	if !ok || (tail != "" && tail[0] != '.' && tail[0] != '[') {
		return "", false
	}

	return "$" + tail, true
}

func validMappingContext(name string) bool {
	switch name {
	case contextRequestID,
		"extendedRequestId",
		contextAPIID,
		"accountId",
		contextStage,
		contextDomainName,
		"domainPrefix",
		contextHTTPMethod,
		"path",
		"protocol",
		contextRouteKey,
		contextRequestTime,
		contextRequestTimeEpoch,
		contextSourceIP,
		contextUserAgent,
		"authorizer.error",
		"integrationErrorMessage",
		"integration.error",
		"integrationStatus",
		"integration.status",
		"status",
		"responseLength":
		return true
	}

	field, ok := strings.CutPrefix(name, "authorizer.")
	return ok && mappingVariable.MatchString(field)
}

func (c *call) mappingValue(source string, resp *execute.Response, authorizer map[string]any) string {
	scope, rest, _ := strings.Cut(source, ".")
	switch scope {
	case "context":
		if key, ok := strings.CutPrefix(rest, "authorizer."); ok && key != "error" {
			return authorizerVariable(authorizer, key)
		}

		return contextVariable(c, resp, rest)
	case mappingStageVariables:
		return c.stage.StageVariables[rest]
	}

	header, body := c.req.Header, c.req.Body
	if resp != nil {
		header, body = resp.Header, resp.Body
	}

	location, name, _ := strings.Cut(rest, ".")
	switch location {
	case mappingHeader:
		return strings.Join(header.Values(name), ",")
	case mappingQuery:
		return strings.Join(c.req.Query[name], ",")
	case mappingPath:
		if name == "" {
			return c.path
		}

		return c.params[name]
	default:
		path, _ := mappingBodyPath(rest)
		if path == "$" {
			return string(body)
		}

		value, _, _ := jsonpath.Text(body[:min(len(body), mappingBodyLimit)], path)
		return value
	}
}

func authorizerVariable(authorizer map[string]any, key string) string {
	switch {
	case strings.HasPrefix(key, "claims."):
		claims, _ := authorizer["claims"].(map[string]string)
		return claims[strings.TrimPrefix(key, "claims.")]
	default:
		context, _ := authorizer["lambda"].(map[string]any)
		switch value := context[key].(type) {
		case string:
			return value
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(value)
		}
	}

	return ""
}

func editParameter(values map[string][]string, action, name, value string) {
	switch action {
	case mappingAppend:
		values[name] = append(values[name], value)
	case mappingOverwrite:
		values[name] = []string{value}
	case mappingRemove:
		delete(values, name)
	}
}

// mapRequest evaluates all sources on the incoming request, before applying any changes.
func (c *call) mapRequest(parameters map[string]string, authorizer map[string]any) (*execute.Request, *string, error) {
	r := *c.req
	var path *string
	if len(parameters) == 0 {
		return &r, nil, nil
	}

	r.Header = c.req.Header.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}

	// Query slices are copied before appending so a mapping cannot change its sources.
	r.Query = maps.Clone(c.req.Query)
	if r.Query == nil {
		r.Query = map[string][]string{}
	}

	queryChanged := false
	for _, key := range slices.Sorted(maps.Keys(parameters)) {
		action, target, name := mappingTarget(key)
		value, _ := evaluateMapping(parameters[key], false, func(source string) string {
			return c.mappingValue(source, nil, authorizer)
		})
		switch target {
		case mappingHeader:
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, nil, invalidMapping(key)
			}

			editParameter(r.Header, action, http.CanonicalHeaderKey(name), value)
		case mappingQuery:
			r.Query[name] = slices.Clone(r.Query[name])
			editParameter(r.Query, action, name, value)
			queryChanged = true
		case mappingPath:
			if !strings.HasPrefix(value, "/") {
				return nil, nil, invalidMapping(key)
			}

			path = &value
		}
	}

	if queryChanged {
		r.RawQuery = r.Query.Encode()
	}

	return &r, path, nil
}

func (c *call) mapResponse(resp *execute.Response, parameters map[string]map[string]string,
	authorizer map[string]any,
) *execute.Response {
	rules := parameters[strconv.Itoa(resp.Status)]
	if len(rules) == 0 {
		return resp
	}

	out := *resp
	out.Header = resp.Header.Clone()
	for _, key := range slices.Sorted(maps.Keys(rules)) {
		action, target, name := mappingTarget(key)
		value, _ := evaluateMapping(rules[key], true, func(source string) string {
			return c.mappingValue(source, resp, authorizer)
		})
		if target == mappingStatus {
			if status, ok := mappingStatusCode(value); ok {
				out.Status = status
				continue
			}
		} else if httpguts.ValidHeaderFieldValue(value) {
			editParameter(out.Header, action, http.CanonicalHeaderKey(name), value)
			continue
		}

		c.integrationError = invalidMapping(key).Error()
		return execute.Message(http.StatusInternalServerError, "Internal Server Error")
	}

	return &out
}

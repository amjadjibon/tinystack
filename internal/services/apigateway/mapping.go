package apigateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy/jsonpath"
	"github.com/amjadjibon/tinystack/internal/services/vtl"
)

const (
	integrationRequest  = "integration.request."
	methodRequest       = "method.request."
	methodHeader        = "method.response.header."
	responseHeader      = "integration.response.header."
	responseBody        = "integration.response.body"
	stageVariablePrefix = "stageVariables."
	contextPrefix       = "context."
	whenNoMatch         = "WHEN_NO_MATCH"
	whenNoTemplates     = "WHEN_NO_TEMPLATES"
	convertToText       = "CONVERT_TO_TEXT"
	convertToBinary     = "CONVERT_TO_BINARY"
	mappingHeader       = "header"
	locationPath        = "path"
	contentTypeHeader   = "Content-Type"
)

func parseTemplateJSON(data []byte) (any, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}

	return vtl.FromJSON(value), nil
}

func stringsForTemplate(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}

	return out
}

func (c *call) parameter(location, name string) (string, bool) {
	switch location {
	case locationPath:
		v, ok := c.params[name]
		return v, ok
	case locationQuery:
		return lastValue(c.req.Query[name])
	case mappingHeader:
		return lastValue(c.req.Header.Values(name))
	}

	return "", false
}

func lastValue(values []string) (string, bool) {
	if len(values) > 0 {
		return values[len(values)-1], true
	}

	return "", false
}

func (c *call) inputParams(args []any) (any, error) {
	if len(args) == 1 {
		for _, location := range []string{locationPath, locationQuery, mappingHeader} {
			if value, ok := c.parameter(location, vtl.String(args[0])); ok {
				return value, nil
			}
		}

		return "", nil
	}

	if len(args) != 0 {
		return nil, errors.New("$input.params expects zero or one argument")
	}

	groups := map[string]any{locationPath: stringsForTemplate(c.params)}
	for location, values := range map[string]map[string][]string{
		mappingHeader: c.req.Header, locationQuery: c.req.Query,
	} {
		g := map[string]any{}
		for name, vs := range values {
			if v, ok := lastValue(vs); ok {
				g[name] = v
			}
		}

		groups[location] = g
	}

	return groups, nil
}

func bodyLookup(body []byte, asJSON bool) vtl.Func {
	return func(args []any) (any, error) {
		if len(args) != 1 {
			return nil, errors.New("$input.json and $input.path expect one JSONPath argument")
		}

		value, err := jsonpath.Select(body, vtl.String(args[0]))
		if err != nil {
			return nil, err
		}

		if value == nil {
			value = json.RawMessage("null")
		}

		if asJSON {
			var compact bytes.Buffer
			err := json.Compact(&compact, value)
			return compact.String(), err
		}

		return parseTemplateJSON(value)
	}
}

func templateString(fn func(string) (any, error)) vtl.Func {
	return func(args []any) (any, error) {
		if len(args) != 1 {
			return nil, errors.New("$util method expects one argument")
		}

		return fn(vtl.String(args[0]))
	}
}

var templateUtilities = map[string]any{
	"escapeJavaScript": templateString(func(s string) (any, error) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			return nil, err
		}

		quoted := strings.TrimSuffix(b.String(), "\n")
		return strings.ReplaceAll(quoted[1:len(quoted)-1], "'", `\'`), nil
	}),
	"parseJson": templateString(func(s string) (any, error) { return parseTemplateJSON([]byte(s)) }),
	"urlEncode": templateString(func(s string) (any, error) { return url.QueryEscape(s), nil }),
	"urlDecode": templateString(func(s string) (any, error) { return url.QueryUnescape(s) }),
	"base64Encode": templateString(func(s string) (any, error) {
		return base64.StdEncoding.EncodeToString([]byte(s)), nil
	}),
	"base64Decode": templateString(func(s string) (any, error) {
		data, err := base64.StdEncoding.DecodeString(s)
		return string(data), err
	}),
}

func (c *call) renderMapping(source string, body []byte, context map[string]any) ([]byte, error) {
	template, err := vtl.ParseStrict(source)
	if err != nil {
		return nil, err
	}

	out, _, _, err := template.Render(map[string]any{
		"input": map[string]any{
			"body": string(body), "json": bodyLookup(body, true), locationPath: bodyLookup(body, false),
			"params": vtl.Func(c.inputParams),
		},
		"util": maps.Clone(
			templateUtilities,
		),
		"context":        vtl.FromJSON(context),
		"stageVariables": stringsForTemplate(c.stage.Variables),
	})
	return []byte(out), err
}

func mediaType(value string) string {
	kind, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.TrimSpace(strings.ToLower(value))
	}

	return kind
}

func templateFor(templates map[string]string, contentType string) (string, bool) {
	if source, ok := templates[contentType]; ok {
		return source, true
	}

	for _, key := range slices.Sorted(maps.Keys(templates)) {
		if mediaType(key) == contentType {
			return templates[key], true
		}
	}

	return "", false
}

func responseTemplate(r *api.IntegrationResponse, accept, backendType string) (source, kind string, found bool) {
	first, _, _ := strings.Cut(accept, ",")
	for _, requested := range []string{mediaType(first), mediaType(backendType), mediaJSON} {
		if source, ok := templateFor(r.ResponseTemplates, requested); ok {
			return source, requested, true
		}
	}

	return "", "", false
}

func convertBody(body []byte, handling string) ([]byte, error) {
	switch handling {
	case convertToBinary:
		return base64.StdEncoding.DecodeString(string(body))
	case convertToText:
		return []byte(base64.StdEncoding.EncodeToString(body)), nil
	}

	return body, nil
}

func mappingParameter(expr, prefix string) (location, name string, ok bool) {
	suffix, ok := strings.CutPrefix(expr, prefix)
	if !ok {
		return "", "", false
	}

	location, name, ok = strings.Cut(suffix, ".")
	if !ok || name == "" || strings.ContainsAny(name, " \t\r\n") {
		return "", "", false
	}

	switch location {
	case mappingHeader, locationPath, locationQuery:
		return location, name, true
	}

	return "", "", false
}

func staticMapping(expr string) (string, bool) {
	if len(expr) >= 2 && expr[0] == '\'' && expr[len(expr)-1] == '\'' {
		return expr[1 : len(expr)-1], true
	}

	return "", false
}

// bodyMapping extracts the JSONPath used by a parameter expression. AWS omits the leading $.
func bodyMapping(expr, prefix string) (string, bool) {
	if expr == prefix {
		return "$", true
	}

	if suffix, ok := strings.CutPrefix(expr, prefix); ok && strings.HasPrefix(suffix, "[") {
		return "$" + suffix, true
	}

	suffix, ok := strings.CutPrefix(expr, prefix+".")
	if !ok || suffix == "" {
		return "", false
	}

	if strings.HasPrefix(suffix, "$") {
		return suffix, true
	}

	return "$." + suffix, true
}

func (c *call) mappingContext(authorizer map[string]any) map[string]any {
	context := proxy.RequestContextV1(c.req, c.proxyContext(authorizer))
	if _, ok := context["authorizer"]; !ok {
		context["authorizer"] = map[string]any{}
	}

	return context
}

func variableSourceValid(expr string) bool {
	if _, ok := staticMapping(expr); ok {
		return true
	}

	if name, ok := strings.CutPrefix(expr, stageVariablePrefix); ok {
		return stageVariable.MatchString(name)
	}

	if name, ok := strings.CutPrefix(expr, contextPrefix); ok {
		switch name {
		case "accountId", "apiId", "domainName", "domainPrefix", "extendedRequestId",
			"httpMethod", "path", "protocol", "requestId", "requestTime", "requestTimeEpoch",
			"resourceId", "resourcePath", "stage",
			"identity.sourceIp", "identity.userAgent", "identity.accountId", "identity.accessKey",
			"identity.caller", "identity.user", "identity.userArn":
			return true
		}

		return strings.HasPrefix(name, "authorizer.") && len(name) > len("authorizer.")
	}

	return false
}

func bodySourceValid(expr, prefix string) bool {
	if path, ok := bodyMapping(expr, prefix); ok {
		return jsonpath.Validate(path) == nil
	}

	return false
}

func requestSourceValid(expr string) bool {
	_, _, parameter := mappingParameter(expr, methodRequest)
	return parameter || variableSourceValid(expr) || bodySourceValid(expr, methodRequest+"body")
}

func (c *call) mappingVariable(expr string, context map[string]any) (string, bool) {
	if literal, ok := staticMapping(expr); ok {
		return literal, true
	}

	if name, ok := strings.CutPrefix(expr, stageVariablePrefix); ok {
		v, found := c.stage.Variables[name]
		return v, found
	}

	var value any = context
	for part := range strings.SplitSeq(strings.TrimPrefix(expr, contextPrefix), ".") {
		m, _ := value.(map[string]any)
		value = m[part]
	}

	return vtl.String(value), value != nil
}

func (c *call) mappedValue(expr string, context map[string]any) (text string, found bool, err error) {
	if location, name, ok := mappingParameter(expr, methodRequest); ok {
		v, present := c.parameter(location, name)
		return v, present, nil
	}

	if expr == methodRequest+"body" {
		return string(c.req.Body), true, nil
	}

	if path, ok := bodyMapping(expr, methodRequest+"body"); ok {
		return jsonpath.Text(c.req.Body, path)
	}

	v, found := c.mappingVariable(expr, context)
	return v, found, nil
}

func responseSourceValid(expr string) bool {
	if name, ok := strings.CutPrefix(expr, responseHeader); ok {
		return name != ""
	}

	return variableSourceValid(expr) || bodySourceValid(expr, responseBody)
}

func (c *call) responseValue(
	expr string,
	backendHeader http.Header,
	body []byte,
	context map[string]any,
) (text string, found bool, err error) {
	if name, ok := strings.CutPrefix(expr, responseHeader); ok {
		v, present := lastValue(backendHeader.Values(name))
		return v, present, nil
	}

	if expr == responseBody {
		return string(body), true, nil
	}

	if path, ok := bodyMapping(expr, responseBody); ok {
		return jsonpath.Text(body, path)
	}

	v, found := c.mappingVariable(expr, context)
	return v, found, nil
}

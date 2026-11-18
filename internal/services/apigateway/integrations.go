package apigateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
)

const (
	typeHTTPMapped = "HTTP"
	typeAWSMapped  = "AWS"
)

func mappedIntegration(i *api.Integration) bool {
	switch common.Value(i.Type) {
	case typeHTTPMapped, typeAWSMapped:
		return true
	}

	return false
}

func checkMappedIntegration(i *api.Integration) error {
	switch common.Value(i.PassthroughBehavior) {
	case "", whenNoMatch, whenNoTemplates, "NEVER":
	default:
		return badRequest("Invalid passthrough behavior %q", common.Value(i.PassthroughBehavior))
	}

	if err := checkContentHandling(i.ContentHandling); err != nil {
		return err
	}

	for target, source := range i.RequestParameters {
		loc, _, ok := mappingParameter(target, integrationRequest)
		if !ok || !requestSourceValid(source) {
			return badRequest("Unsupported integration request mapping %q = %q", target, source)
		}

		if common.Value(i.Type) == typeAWSMapped && loc == mappingHeader &&
			!strings.EqualFold(target, integrationRequest+"header."+contentTypeHeader) {
			return badRequest("AWS integrations only support mapping the Content-Type header")
		}
	}

	return nil
}

func checkContentHandling(handling *string) error {
	switch common.Value(handling) {
	case "", convertToBinary, convertToText:
		return nil
	}

	return badRequest("Unsupported contentHandling %q", *handling)
}

func (s *Service) integrateMapped(
	ctx context.Context,
	c *call,
	i *api.Integration,
	uri string,
	authorizer map[string]any,
	timeout time.Duration,
) *execute.Response {
	kind := mediaType(c.req.Header.Get(contentTypeHeader))
	if kind == "" {
		kind = mediaJSON
	}

	template, matched := templateFor(i.RequestTemplates, kind)
	behavior := common.Value(i.PassthroughBehavior)
	if !matched && (behavior == "NEVER" || (behavior == whenNoTemplates && len(i.RequestTemplates) > 0)) {
		return execute.Message(http.StatusUnsupportedMediaType, "Unsupported Media Type")
	}

	variables := c.mappingContext(authorizer)
	body, err := convertBody(c.req.Body, common.Value(i.ContentHandling))
	if err == nil && matched {
		body, err = c.renderMapping(template, body, variables)
	}

	if err != nil {
		return c.mappingFailure(err)
	}

	header := http.Header{contentTypeHeader: {c.req.Header.Get(contentTypeHeader)}}
	if header.Get(contentTypeHeader) == "" {
		header.Set(contentTypeHeader, kind)
	}

	query, path := url.Values{}, map[string]string{}
	for _, target := range slices.Sorted(maps.Keys(i.RequestParameters)) {
		value, found, err := c.mappedValue(i.RequestParameters[target], variables)
		if err != nil {
			return c.mappingFailure(err)
		}

		if !found {
			continue
		}

		loc, name, _ := mappingParameter(target, integrationRequest)
		switch loc {
		case mappingHeader:
			header.Set(name, value)
		case locationQuery:
			query.Set(name, value)
		case locationPath:
			path[name] = value
		}
	}

	if common.Value(i.Type) == typeHTTPMapped {
		return s.mappedHTTP(ctx, c, i, uri, header, query, path, body, variables, timeout)
	}

	return s.mappedAWS(ctx, c, i, uri, header, query, path, body, variables, timeout)
}

var pathPlaceholder = regexp.MustCompile(`\{([^{}]+)\}`)

func bindIntegrationPath(path string, values map[string]string) (string, error) {
	unused := maps.Clone(values)
	var missing string
	bound := pathPlaceholder.ReplaceAllStringFunc(path, func(placeholder string) string {
		name := strings.TrimSuffix(placeholder[1:len(placeholder)-1], "+")
		value, ok := values[name]
		if !ok {
			missing = name
		}

		delete(unused, name)
		if strings.HasSuffix(placeholder, "+}") {
			parts := strings.Split(value, "/")
			for idx, part := range parts {
				parts[idx] = url.PathEscape(part)
			}

			return strings.Join(parts, "/")
		}

		return url.PathEscape(value)
	})
	if missing != "" {
		return "", fmt.Errorf("missing integration path parameter %q", missing)
	}

	if len(unused) > 0 {
		return "", fmt.Errorf("unused integration path parameters %v", slices.Sorted(maps.Keys(unused)))
	}

	return bound, nil
}

func (s *Service) mappedHTTP(
	ctx context.Context,
	c *call,
	i *api.Integration,
	uri string,
	header http.Header,
	query url.Values,
	path map[string]string,
	body []byte,
	variables map[string]any,
	timeout time.Duration,
) *execute.Response {
	u, err := url.Parse(uri)
	if err != nil {
		return c.mappingFailure(err)
	}

	// Work on the escaped path, preserving encoded separators in the configured URI.
	rawPath := u.RawPath
	if rawPath == "" {
		rawPath = u.EscapedPath()
	}

	raw, err := bindIntegrationPath(rawPath, path)
	if err != nil {
		return c.mappingFailure(err)
	}

	u.RawPath = raw
	u.Path, err = url.PathUnescape(raw)
	if err != nil {
		return c.mappingFailure(err)
	}

	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return c.mappingFailure(err)
	}

	maps.Copy(params, query)
	u.RawQuery = params.Encode()
	method := common.Value(i.HttpMethod)
	if method == methodAny {
		method = c.req.Method
	}

	backend, err := proxy.RoundTrip(ctx, method, u.String(), header, body, timeout)
	if err != nil {
		c.integrationError = err.Error()
		return backend
	}

	return c.mapIntegrationResponse(i, backend, new(strconv.Itoa(backend.Status)), variables)
}

func (c *call) mappingFailure(err error) *execute.Response {
	c.integrationError = err.Error()
	return execute.Message(http.StatusInternalServerError, err.Error())
}

func (c *call) mapIntegrationResponse(
	i *api.Integration,
	backend *execute.Response,
	match *string,
	variables map[string]any,
) *execute.Response {
	c.integrationCode = backend.Status
	chosen := selectIntegrationResponse(i, match)
	if chosen == nil {
		return c.mappingFailure(errors.New("no integration response matched and no default response is configured"))
	}

	code := common.Value(chosen.StatusCode)
	if _, ok := c.method.MethodResponses[code]; !ok || !statusCode.MatchString(code) {
		return c.mappingFailure(fmt.Errorf("integration response has no method response for status %s", code))
	}

	status, _ := strconv.Atoi(code)
	out := &execute.Response{Status: status, Header: http.Header{}, Body: backend.Body}
	accept := c.req.Header.Get("Accept")
	backendType := backend.Header.Get(contentTypeHeader)
	if source, kind, ok := responseTemplate(chosen, accept, backendType); ok {
		body, err := c.renderMapping(source, backend.Body, variables)
		if err != nil {
			return c.mappingFailure(err)
		}

		out.Body = body
		out.Header.Set(contentTypeHeader, kind)
	} else if backendType != "" {
		out.Header.Set(contentTypeHeader, backendType)
	}

	for target, expr := range chosen.ResponseParameters {
		value, found, err := c.responseValue(expr, backend.Header, backend.Body, variables)
		if err != nil {
			return c.mappingFailure(err)
		}

		if found {
			out.Header.Set(strings.TrimPrefix(target, methodHeader), value)
		}
	}

	var err error
	out.Body, err = convertBody(out.Body, common.Value(chosen.ContentHandling))
	if err != nil {
		return c.mappingFailure(err)
	}

	return out
}

func selectIntegrationResponse(i *api.Integration, match *string) *api.IntegrationResponse {
	var fallback *api.IntegrationResponse
	for _, code := range slices.Sorted(maps.Keys(i.IntegrationResponses)) {
		r := i.IntegrationResponses[code]
		pattern := common.Value(r.SelectionPattern)
		if pattern == "" {
			if fallback == nil {
				fallback = &r
			}

			continue
		}

		if match != nil && regexp.MustCompile("^(?:"+pattern+")$").MatchString(*match) {
			return &r
		}
	}

	return fallback
}

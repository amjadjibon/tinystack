package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const consoleAPIResponseLimit = 1 << 20

var consoleAPIHeaderName = regexp.MustCompile(`^[!#$%&'*+.^_\x60|~0-9A-Za-z-]+$`)

type consoleAPIRequest struct {
	Headers map[string]string
	ApiId   string
	Stage   string
	Method  string
	Path    string
	Body    string
}

func (s *Server) consoleInvokeAPI(r *http.Request, c *call, caller *catalog.Caller, in consoleAction) (any, error) {
	raw, err := json.Marshal(in.Input)
	if err != nil {
		return nil, awserror.Validation("Invalid API request: %s", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input consoleAPIRequest
	if err = decoder.Decode(&input); err != nil {
		return nil, awserror.Validation("Invalid API request: %s", err)
	}

	if input.ApiId == "" || input.Stage == "" || !slices.Contains(
		[]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}, input.Method,
	) {
		return nil, awserror.Validation("An API ID, stage and concrete HTTP method are required.")
	}

	parsed, err := url.ParseRequestURI(input.Path)
	if err != nil || !strings.HasPrefix(input.Path, "/") || strings.HasPrefix(input.Path, "//") ||
		parsed.IsAbs() || parsed.Fragment != "" || strings.Contains(input.Path, "#") {
		return nil, awserror.Validation("Path must start with / and may include a query string.")
	}

	if len(input.Body) > maxExecuteBody {
		return nil, awserror.Validation("Request body exceeds the 10 MiB API payload limit.")
	}

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, awserror.Validation("Invalid query string: %s", err)
	}

	header := http.Header{}
	for name, value := range input.Headers {
		if !consoleAPIHeaderName.MatchString(name) || strings.ContainsFunc(value, func(r rune) bool {
			return (r < 32 && r != '\t') || r == 127
		}) {
			return nil, awserror.Validation("Invalid request header %s.", name)
		}

		header.Set(name, value)
	}

	// Test requests do not impersonate an IAM caller or accept unverified SigV4 headers.
	if strings.HasPrefix(header.Get("Authorization"), "AWS4-") || query.Has("X-Amz-Signature") {
		return nil, awserror.Validation("Use a signed execute-api CLI/SDK request to test AWS_IAM authorization.")
	}

	ctx := request.WithMetadata(r.Context(), c.meta)
	operation, key := "GetApi", "ApiId"
	if in.Service == consoleAPIGateway {
		operation, key = "GetRestApi", "RestApiId"
	}

	// IDs have global routing indexes. First read through the scoped management API
	// so an invocation cannot escape the console's selected account and Region.
	if _, err = caller.Call(ctx, in.Service, operation, map[string]any{key: input.ApiId}); err != nil {
		return nil, err
	}

	if _, err = caller.Call(
		ctx,
		in.Service,
		"GetStage",
		map[string]any{key: input.ApiId, "StageName": input.Stage},
	); err != nil {
		return nil, err
	}

	handler, ok := c.service.Inventory.(execute.Handler)
	if !ok {
		return nil, awserror.New("NotImplemented", "API invocation is not available for this service.")
	}

	path := "/" + input.Stage + parsed.Path
	if in.Service == consoleHTTPAPI && input.Stage == "$default" {
		path = parsed.Path
		first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if first != "" && first != "$default" {
			_, stageErr := caller.Call(ctx, in.Service, "GetStage", map[string]any{
				key: input.ApiId, "StageName": first,
			})
			if stageErr == nil {
				return nil, awserror.Validation("Path names stage %s; select that stage and remove its prefix.", first)
			}

			if awserror.From(stageErr).Code != "NotFoundException" {
				return nil, stageErr
			}
		}
	}

	source := r.RemoteAddr
	if host, _, splitErr := net.SplitHostPort(source); splitErr == nil {
		source = host
	}

	response, found, err := handler.Execute(ctx, input.ApiId, &execute.Request{
		Time: time.Now().UTC(), Method: input.Method, Path: path, RawQuery: parsed.RawQuery,
		Query: query, Header: header, Body: []byte(input.Body), SourceIP: source,
		Host:     strings.TrimPrefix(strings.TrimPrefix(c.meta.Endpoint, "https://"), "http://"),
		Endpoint: c.meta.Endpoint + executePath + input.ApiId, Protocol: "HTTP/1.1",
	})
	if err != nil {
		return nil, err
	}

	if !found || response == nil {
		return nil, awserror.New("ResourceNotFoundException", "API not found.")
	}

	body, truncated := response.Body, len(response.Body) > consoleAPIResponseLimit
	if truncated {
		body = body[:consoleAPIResponseLimit]
	}

	encoded := !utf8.Valid(body)
	text := string(body)
	if encoded {
		text = base64.StdEncoding.EncodeToString(body)
	}

	return map[string]any{
		"StatusCode": response.Status, "Headers": response.Header, "Body": text,
		"IsBase64Encoded": encoded, "BodyTruncated": truncated,
	}, nil
}

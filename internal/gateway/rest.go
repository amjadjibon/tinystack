package gateway

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restjson"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restxml"
)

// restMaxBody bounds REST payloads such as S3 objects, which are held in memory.
const restMaxBody = 64 << 20

// restProtocol returns the REST protocol a service speaks, or "".
func restProtocol(service *catalog.Service) string {
	for _, protocol := range []string{catalog.RESTXML, catalog.RESTJSON} {
		if service.Supports(protocol) {
			return protocol
		}
	}

	return ""
}

// route is a parsed REST URI template such as "/{Bucket}/{Key+}?x-id=GetObject".
type route struct {
	literals map[string]string
	allowed  map[string]bool
	required []string
	name     string
	method   string
	segments []string
	greedy   bool
}

// routes parses the REST bindings of a service.
func routes(service *catalog.Service) []route {
	var out []route
	protocol := restProtocol(service)
	for name := range service.Operations {
		b, ok := service.Operations[name].Bindings[protocol]
		if !ok {
			continue
		}

		path, rawQuery, _ := strings.Cut(b.URI, "?")
		rt := route{
			name:     name,
			method:   b.Method,
			literals: map[string]string{},
			allowed:  map[string]bool{"x-id": true},
		}

		if trimmed := strings.Trim(path, "/"); trimmed != "" {
			rt.segments = strings.Split(trimmed, "/")
		}

		if n := len(rt.segments); n > 0 && strings.HasSuffix(rt.segments[n-1], "+}") {
			rt.greedy = true
		}

		for _, pair := range strings.Split(rawQuery, "&") {
			if k, v, _ := strings.Cut(pair, "="); k != "" && k != "x-id" {
				rt.literals[k] = v
				rt.allowed[k] = true
			}
		}

		for _, mb := range b.Input {
			if strings.HasSuffix(mb.Location, "query") {
				rt.allowed[mb.Name] = true
			}
		}
		// S3's multipart routes share their method and object path with
		// ordinary object operations. Their upload query parameters distinguish
		// the operations even when the optional x-id parameter is absent.
		switch name {
		case "UploadPart":
			rt.required = []string{"uploadId", "partNumber"}
		case "ListParts", "CompleteMultipartUpload", "AbortMultipartUpload":
			rt.required = []string{"uploadId"}
		}

		out = append(out, rt)
	}

	return out
}

// match binds the request path segments to the route's labels.
func (rt *route) match(method string, segments []string, query url.Values) (map[string]string, bool) {
	if method != rt.method || len(segments) < len(rt.segments) || (!rt.greedy && len(segments) != len(rt.segments)) {
		return nil, false
	}

	for k, v := range rt.literals {
		if got, ok := query[k]; !ok || (v != "" && (len(got) != 1 || got[0] != v)) {
			return nil, false
		}
	}
	for _, k := range rt.required {
		if len(query[k]) != 1 || query.Get(k) == "" {
			return nil, false
		}
	}

	for k := range query {
		if !rt.allowed[k] {
			return nil, false
		}
	}

	labels := map[string]string{}
	for i, seg := range rt.segments {
		label, isLabel := strings.CutPrefix(seg, "{")
		if !isLabel {
			if segments[i] != seg {
				return nil, false
			}

			continue
		}

		label = strings.TrimSuffix(label, "}")
		value := segments[i]
		if greedy := strings.TrimSuffix(label, "+"); greedy != label {
			label, value = greedy, strings.Join(segments[i:], "/")
		}

		if value == "" {
			return nil, false
		}

		labels[label] = value
	}

	return labels, true
}

// virtualBucket extracts the bucket from S3 virtual-hosted addressing:
// "<bucket>.localhost" or "<bucket>.s3[.<region>].amazonaws.com".
func virtualBucket(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if bucket, ok := strings.CutSuffix(host, ".localhost"); ok {
		return bucket
	}

	if !strings.HasSuffix(host, ".amazonaws.com") {
		return ""
	}

	if i := strings.LastIndex(host, ".s3."); i > 0 {
		return host[:i]
	}

	return ""
}

// restOperation resolves the REST operation and its URI labels.
func (s *Server) restOperation(r *http.Request, c *call) (map[string]string, error) {
	var segments []string
	if trimmed := strings.TrimPrefix(r.URL.EscapedPath(), "/"); trimmed != "" {
		for _, seg := range strings.Split(trimmed, "/") {
			decoded, err := url.PathUnescape(seg)
			if err != nil {
				return nil, rest.NotImplemented("Invalid request path")
			}

			segments = append(segments, decoded)
		}
	}
	// S3 treats "/bucket/" as the bucket itself.
	if len(segments) == 2 && segments[1] == "" {
		segments = segments[:1]
	}

	if c.service.SigningName == "s3" {
		if bucket := virtualBucket(r.Host); bucket != "" {
			segments = append([]string{bucket}, segments...)
		}
	}

	query := r.URL.Query()
	best, bestLiterals := -1, -1
	var labels map[string]string
	routes := s.routes[c.service.Name]
	for i := range routes {
		if found, ok := routes[i].match(r.Method, segments, query); ok && len(routes[i].literals) > bestLiterals {
			best, bestLiterals, labels = i, len(routes[i].literals), found
		}
	}

	if best < 0 {
		return nil, rest.NotImplemented("TinyStack does not implement this " + c.service.Name + " request.")
	}

	c.meta.Operation = routes[best].name
	return labels, nil
}

// decodeREST reads and decodes a REST request into the operation input.
func decodeREST(w http.ResponseWriter, r *http.Request, c *call, op *catalog.Operation) (any, error) {
	binding := op.Bindings[c.meta.Protocol]
	body, err := rest.ReadBody(w, r, restMaxBody)
	if err != nil {
		if _, ok := err.(*awserror.Error); ok { //nolint:errorlint // ReadBody returns awserror values directly.
			return nil, err
		}

		return nil, awserror.New("IncompleteBody", "Cannot read request body")
	}

	input := op.NewInput()
	if c.meta.Protocol == catalog.RESTJSON {
		return input, restjson.Decode(r, &binding, c.labels, body, input)
	}

	return input, restxml.Decode(r, &binding, c.labels, body, input)
}

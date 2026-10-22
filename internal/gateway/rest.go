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
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

// restMaxBody bounds REST document payloads and non-S3 services.
const (
	restMaxBody  = 64 << 20
	opPutObject  = "PutObject"
	opUploadPart = "UploadPart"
	opGetObject  = "GetObject"
)

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
	// id is the route's x-id literal; a request that sends another x-id does
	// not match.
	id string
	// header is a request header the operation needs, such as the copy source
	// that tells CopyObject from PutObject; a route that needs one outranks
	// routes that do not.
	header   string
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
			switch k, v, _ := strings.Cut(pair, "="); {
			case k == "x-id":
				rt.id = v
			case k != "":
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
		case opUploadPart:
			rt.required = []string{"uploadId", "partNumber"}
		case "ListParts", "CompleteMultipartUpload", "AbortMultipartUpload":
			rt.required = []string{"uploadId"}
		case "CopyObject":
			rt.header = "X-Amz-Copy-Source"
		}

		out = append(out, rt)
	}

	return out
}

// score ranks a matching route: routes that need a header and then routes with
// more query literals are more specific.
func (rt *route) score() int {
	n := len(rt.literals)
	if rt.header != "" {
		n += 100
	}

	return n
}

// match binds the request path segments to the route's labels.
func (rt *route) match(
	method string,
	segments []string,
	query url.Values,
	header http.Header,
) (map[string]string, bool) {
	if method != rt.method || len(segments) < len(rt.segments) || (!rt.greedy && len(segments) != len(rt.segments)) {
		return nil, false
	}

	if id := query.Get("x-id"); id != "" && rt.id != "" && id != rt.id {
		return nil, false
	}

	if rt.header != "" && header.Get(rt.header) == "" {
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

	if !strings.HasSuffix(host, ".amazonaws.com") && !strings.HasSuffix(host, ".amazonaws.com.cn") &&
		!strings.HasSuffix(host, ".api.aws") {
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

	// A presigned URL's signature parameters belong to no operation.
	query := r.URL.Query()
	if c.service.SigningName == "s3" && r.Method == http.MethodPost &&
		len(segments) == 1 && len(query) == 0 &&
		strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		c.meta.Operation = opPostObject
		return map[string]string{"Bucket": segments[0]}, nil
	}
	for k := range query {
		if sigv4.QueryParameter(k) {
			delete(query, k)
		}
	}

	best, bestScore := -1, -1
	var labels map[string]string
	routes := s.routes[c.service.Name]
	for i := range routes {
		found, ok := routes[i].match(r.Method, segments, query, r.Header)
		if ok && routes[i].score() > bestScore {
			best, bestScore, labels = i, routes[i].score(), found
		}
	}

	if best < 0 {
		return nil, rest.NotImplemented("TinyStack does not implement this " + c.service.Name + " request.")
	}

	c.meta.Operation = routes[best].name
	return labels, nil
}

// decodeREST reads and decodes a REST request into the operation input.
func (s *Server) decodeREST(w http.ResponseWriter, r *http.Request, c *call, op *catalog.Operation) (any, error) {
	binding := op.Bindings[c.meta.Protocol]
	if c.service.SigningName == "s3" && (c.meta.Operation == opPutObject || c.meta.Operation == opUploadPart) {
		stream, trailers, streamErr := rest.StreamBody(w, r, s.cfg.Storage.MaxObjectBytes)
		if streamErr != nil {
			return nil, streamErr
		}
		if c.payload == nil {
			c.payload = &request.Payload{}
		}
		c.payload.Stream = stream
		c.payload.Trailers = trailers

		input := op.NewInput()
		return input, rest.Decode(r, &binding, c.labels, nil, input, nil)
	}

	body, err := rest.ReadBody(w, r, s.restBodyLimit(c))
	if err != nil {
		if _, ok := err.(*awserror.Error); ok { //nolint:errorlint // ReadBody returns awserror values directly.
			return nil, err
		}

		return nil, awserror.New("IncompleteBody", "Cannot read request body")
	}

	input := op.NewInput()
	if c.meta.Protocol == catalog.RESTJSON {
		if decodeErr := restjson.Decode(r, &binding, c.labels, body, input); decodeErr != nil {
			return input, decodeErr
		}
		if c.service.Name == authorizationBatch {
			return input, batchUniqueJSON(body)
		}
		return input, nil
	}

	return input, restxml.Decode(r, &binding, c.labels, body, input)
}

// restBodyLimit lifts the cap only for S3 object and multipart part uploads.
func (s *Server) restBodyLimit(c *call) int64 {
	if c.service.SigningName == "s3" && (c.meta.Operation == opPutObject || c.meta.Operation == opUploadPart) {
		return s.cfg.Storage.MaxObjectBytes
	}
	return restMaxBody
}

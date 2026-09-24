package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/awsjson"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/query"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restjson"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restxml"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	maxBody         = 2 << 20
	formContentType = "application/x-www-form-urlencoded"
)

type Server struct {
	services map[string]*catalog.Service
	routes   map[string][]route
	logger   *slog.Logger
	health   []byte
	cfg      config.Config
}

// call is a request classified to a protocol and, once known, a service.
type call struct {
	service *catalog.Service
	form    url.Values
	labels  map[string]string
	meta    request.Metadata
	head    bool
}

//nolint:gocritic // Snapshot scalar server configuration instead of sharing mutable caller fields.
func New(cfg config.Config, logger *slog.Logger, services ...catalog.Service) (*Server, error) {
	if guardErr := cfg.Validate(); guardErr != nil {
		return nil, guardErr
	}

	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{cfg: cfg, logger: logger, services: map[string]*catalog.Service{}, routes: map[string][]route{}}
	names := make([]string, 0, len(services))
	for i := range services {
		service := &services[i]
		if _, exists := s.services[service.Name]; exists {
			return nil, fmt.Errorf("duplicate service %s", service.Name)
		}

		s.services[service.Name] = service
		s.routes[service.Name] = routes(service)
		names = append(names, service.Name)
	}

	sort.Strings(names)
	health, err := json.Marshal(map[string]any{"status": "ok", "services": names, "storage": cfg.Storage.Mode})
	if err != nil {
		return nil, err
	}

	s.health = health
	return s, nil
}

// identify resolves the caller scope from the host and SigV4 credential scope,
// returning the signed service name. Signatures are not verified.
func (s *Server) identify(r *http.Request, m *request.Metadata) (string, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// "<service>.<region>.amazonaws.com", or "<bucket>.s3.<region>.amazonaws.com"
	// for S3 virtual-hosted addressing.
	if trimmed, ok := strings.CutSuffix(host, ".amazonaws.com"); ok {
		if parts := strings.Split(trimmed, "."); len(parts) >= 2 {
			m.Service, m.Region = parts[len(parts)-2], parts[len(parts)-1]
		}
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", nil
	}

	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return "", awserror.New("IncompleteSignatureException", "Expected AWS4-HMAC-SHA256")
	}

	credential := ""
	for _, field := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(field), "=")
		if ok && k == "Credential" {
			credential = v
		}
	}

	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[0] == "" || len(scope[1]) != 8 || scope[2] == "" || scope[3] == "" ||
		scope[4] != "aws4_request" {
		return "", awserror.New("IncompleteSignatureException", "Malformed credential scope")
	}

	m.AccessKey = scope[0]
	m.Region = scope[2]
	m.Service = scope[3]
	if account, ok := s.cfg.Identity.Accounts[m.AccessKey]; ok {
		m.AccountID = account
	}

	return scope[3], nil
}

// classify selects the protocol from headers alone, so early errors use the
// client's envelope, then resolves the service and operation.
func (s *Server) classify(w http.ResponseWriter, r *http.Request, contentType string) (*call, error) {
	c := &call{meta: request.Metadata{
		Scope:    request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
		Protocol: catalog.AWSJSON10,
	}}
	target := r.Header.Get("X-Amz-Target")
	if target == "" && contentType == formContentType {
		c.meta.Protocol = catalog.AWSQuery
	}

	if target == "" {
		// REST services are chosen by the signed or host service name; unsigned
		// requests without a JSON target or form body fall back to S3.
		if rest := s.restService(r); rest != nil {
			c.meta.Protocol = restProtocol(rest)
		}
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return c, err
	}

	c.meta.RequestID = hex.EncodeToString(random[:])
	host := r.Host
	if host == "" {
		host = net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(s.cfg.Server.Port))
	}

	c.meta.Endpoint = "http://" + host
	signed, err := s.identify(r, &c.meta)
	if err != nil {
		return c, err
	}

	switch c.meta.Protocol {
	case catalog.AWSQuery:
		return c, s.classifyQuery(w, r, c, signed)
	case catalog.RESTXML, catalog.RESTJSON:
		c.service = s.restService(r)
		if signed != "" && signed != c.service.SigningName {
			return c, awserror.New("IncompleteSignatureException", "Signing service does not match the request")
		}

		c.meta.Service = c.service.Name
		c.labels, err = s.restOperation(r, c)
		return c, err
	}

	prefix, op, ok := strings.Cut(target, ".")
	if !ok || op == "" {
		return c, awserror.New("UnknownOperationException", "X-Amz-Target must identify an operation")
	}

	for _, service := range s.services {
		if service.TargetPrefix == prefix && service.Supports(catalog.AWSJSON10) {
			c.service = service
		}
	}

	if c.service == nil {
		return c, awserror.New("UnknownOperationException", "Unknown target prefix")
	}

	if signed != "" && signed != c.service.SigningName {
		return c, awserror.Validation("Signing service conflicts with X-Amz-Target")
	}

	c.meta.Service = c.service.Name
	c.meta.Operation = op
	return c, nil
}

// classifyQuery reads the form once; decoding later reuses the parsed values.
func (s *Server) classifyQuery(w http.ResponseWriter, r *http.Request, c *call, signed string) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return awserror.New("InvalidParameterValue", "Cannot read request body: "+err.Error())
	}

	if c.form, err = url.ParseQuery(string(body)); err != nil {
		return awserror.New("MalformedQueryString", "Request body is not valid form encoding")
	}

	actions := c.form["Action"]
	if len(actions) != 1 || actions[0] == "" {
		return awserror.New("MissingAction", "The request must contain exactly one Action parameter")
	}

	c.meta.Operation = actions[0]
	// Signed and host-derived service names win; unsigned local requests fall
	// back to the only Query service that implements the action.
	name := signed
	if name == "" {
		name = c.meta.Service
	}

	var candidates []*catalog.Service
	for _, service := range s.services {
		if !service.Supports(catalog.AWSQuery) {
			continue
		}

		if name != "" && service.SigningName == name {
			candidates = []*catalog.Service{service}
			break
		}

		if _, ok := service.Operations[c.meta.Operation]; ok && name == "" {
			candidates = append(candidates, service)
		}
	}

	if len(candidates) != 1 {
		return awserror.New("InvalidAction", "Cannot resolve a service for action "+c.meta.Operation)
	}

	c.service = candidates[0]
	c.meta.Service = c.service.Name
	return nil
}

// restService selects the REST service for a request: the one named by the
// signed scope or regional host, or S3 for unsigned requests that are neither
// AWS JSON nor AWS Query, since S3 is the REST service that serves anonymous
// requests such as presigned URLs.
func (s *Server) restService(r *http.Request) *catalog.Service {
	var m request.Metadata
	signed, err := s.identify(r, &m)
	name := signed
	if name == "" {
		name = m.Service
	}

	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if name == "" && (err != nil || contentType == formContentType) {
		return nil
	}

	if name == "" {
		name = "s3"
	}

	for _, service := range s.services {
		if restProtocol(service) != "" && service.SigningName == name {
			return service
		}
	}

	return nil
}

func (s *Server) decode(
	w http.ResponseWriter,
	r *http.Request,
	c *call,
	contentType string,
) (catalog.Operation, any, error) {
	op, ok := c.service.Operations[c.meta.Operation]
	if restProtocol(c.service) == c.meta.Protocol {
		input, err := decodeREST(w, r, c, &op)
		return op, input, err
	}

	if !ok {
		if c.meta.Protocol == catalog.AWSQuery {
			return op, nil, awserror.New(
				"InvalidAction",
				"The action "+c.meta.Operation+" is not valid for this endpoint",
			)
		}

		return op, nil, awserror.New("UnknownOperationException", "Operation is not implemented")
	}

	binding, supported := op.Bindings[c.meta.Protocol]
	if !supported || contentType != binding.ContentType {
		return op, nil, awserror.Validation("Unsupported content type for this operation")
	}
	// Query clients may post to a resource URL such as an SQS queue URL.
	pathOK := c.meta.Protocol == catalog.AWSQuery || r.URL.Path == binding.URI
	if r.Method != binding.Method || !pathOK || r.URL.RawQuery != "" {
		return op, nil, awserror.Validation("Unexpected method, path, or query parameters")
	}

	input := op.NewInput()
	if c.meta.Protocol == catalog.AWSQuery {
		if versions := c.form["Version"]; len(versions) > 0 && (len(versions) != 1 || versions[0] != binding.Version) {
			return op, nil, awserror.New("NoSuchVersion", "Version must be "+binding.Version)
		}

		return op, input, query.Decode(c.form, input)
	}

	return op, input, awsjson.Decode(http.MaxBytesReader(w, r.Body, maxBody), input)
}

func (s *Server) fail(w http.ResponseWriter, c *call, err error) {
	e := awserror.From(err)
	namespace, messageMember := "", ""
	if c.service != nil {
		if binding, ok := c.service.Errors[e.Code]; ok {
			e.StatusCode = binding.StatusCode
			e.QueryCode = binding.QueryCode
			messageMember = binding.MessageMember
		}

		if c.service.QueryCompatible && e.QueryCode == "" {
			e.QueryCode = e.Code
		}

		namespace = c.service.XMLNamespace
	}

	switch c.meta.Protocol {
	case catalog.AWSQuery:
		query.WriteError(w, namespace, c.meta.RequestID, e)
	case catalog.RESTXML:
		restxml.WriteError(w, e, c.meta.RequestID, c.head)
	case catalog.RESTJSON:
		restjson.WriteError(w, e, messageMember)
	default:
		awsjson.WriteError(w, e)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_tinystack/health" {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.health)
		return
	}

	start := time.Now()
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	c, err := s.classify(w, r, contentType)
	c.head = r.Method == http.MethodHead
	m := &c.meta
	w.Header().Set("x-amzn-RequestId", m.RequestID)
	if m.Protocol == catalog.RESTXML {
		w.Header().Set("x-amz-request-id", m.RequestID)
	}

	defer func() {
		s.logger.Info(
			"request",
			"request_id",
			m.RequestID,
			"service",
			m.Service,
			"operation",
			m.Operation,
			"protocol",
			m.Protocol,
			"account",
			m.AccountID,
			"region",
			m.Region,
			"duration_ms",
			float64(time.Since(start).Microseconds())/1000,
		)
	}()

	if err != nil {
		s.fail(w, c, err)
		return
	}

	op, input, err := s.decode(w, r, c, contentType)
	if err != nil {
		s.fail(w, c, err)
		return
	}

	output, err := op.Invoke(request.WithMetadata(r.Context(), c.meta), input)
	if err != nil {
		s.fail(w, c, err)
		return
	}

	binding := op.Bindings[m.Protocol]
	status := binding.StatusCode
	switch m.Protocol {
	case catalog.RESTXML:
		restxml.Write(w, &binding, output, c.head)
		return
	case catalog.RESTJSON:
		restjson.Write(w, &binding, output)
		return
	}

	if m.Protocol == catalog.AWSQuery {
		query.Write(w, status, m.Operation, c.service.XMLNamespace, m.RequestID, output)
		return
	}

	awsjson.Write(w, status, output)
}

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/awsjson"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/query"
	restbindings "github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restjson"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/restxml"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rpcv2cbor"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/recent"
)

const (
	validationException = "ValidationException"
	maxBody             = 2 << 20
	formContentType     = "application/x-www-form-urlencoded"
)

type Server struct {
	managementEvents func(context.Context, *catalog.ManagementEvent) error
	services         map[string]*catalog.Service
	consoleCaller    *catalog.Caller
	routes           map[string][]route
	logger           *slog.Logger
	requests         *recent.Log[Record]
	metrics          *metrics
	// internal serves TinyStack's own endpoints under /_tinystack/.
	internal    map[string]http.Handler
	credentials CredentialLookup
	documents   Documents
	tokens      Tokens
	registry    Registry
	cors        CORS
	apis        []execute.Handler
	balancers   execute.Handler
	// functionURLs serves Lambda function URLs; see UseFunctionURLs.
	functionURLs execute.Handler
	// graphQL serves AppSync GraphQL APIs; see UseGraphQL.
	graphQL GraphQL
	// mqtt serves MQTT over WebSocket; see UseMQTT.
	mqtt execute.MQTT
	// domains serves API Gateway custom domain names; see UseDomains.
	domains Domains
	health  []byte
	// localStackHealth answers LocalStack's /_localstack/health.
	localStackHealth []byte
	faults           faultState
	cfg              config.Config
}

// CredentialLookup returns the credentials of an access key TinyStack issued,
// such as an IAM user's key or an STS session's, if any.
type CredentialLookup func(ctx context.Context, accessKey string) (request.Credentials, bool, error)

// Documents returns a token issuer's discovery document, such as a Cognito user
// pool's "jwks.json" or "openid-configuration", and whether it has one.
// endpoint is the base URL the client used.
type Documents func(ctx context.Context, issuer, name, endpoint string) ([]byte, bool, error)

// Tokens serves an issuer's OAuth 2.0 token endpoint: it takes the issuer,
// the base URL the client used, the Authorization header, and the form, and
// returns the status and JSON body.
type Tokens func(ctx context.Context, issuer, endpoint, authorization string, form url.Values) (int, []byte, error)

// call is a request classified to a protocol and, once known, a service.
type call struct {
	service *catalog.Service
	// credentials are those of an access key TinyStack issued, if the request's is one.
	credentials *request.Credentials
	form        url.Values
	labels      map[string]string
	// failure is the error the response reports, if any.
	failure *awserror.Error
	meta    request.Metadata
	// body is the raw AWS Query form, kept to hash it for authentication.
	payload    *request.Payload
	cleanups   []func() error
	body       []byte
	head       bool
	injected   bool
	faultError bool
}

func (c *call) addCleanup(fn func() error) {
	c.cleanups = append(c.cleanups, fn)
}

func (c *call) close() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		_ = c.cleanups[i]()
	}
	if c.payload != nil {
		if c.payload.Body != nil {
			_ = c.payload.Body.Close()
		}
		if c.payload.Stream != nil {
			_ = c.payload.Stream.Close()
		}
	}
}

//nolint:gocritic // Snapshot scalar server configuration instead of sharing mutable caller fields.
func New(cfg config.Config, logger *slog.Logger, services ...catalog.Service) (*Server, error) {
	if guardErr := cfg.Validate(); guardErr != nil {
		return nil, guardErr
	}

	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		cfg:      cfg,
		logger:   logger,
		requests: recent.New[Record](maxRecords, "requests"),
		metrics:  &metrics{},
		services: map[string]*catalog.Service{},
		routes:   map[string][]route{},
	}
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
	states := map[string]string{}
	for _, name := range names {
		states[name] = "running"
	}

	// The shape of LocalStack's health endpoint, which tools poll to wait for it.
	s.localStackHealth, err = json.Marshal(map[string]any{
		"services": states, "edition": "community", "version": "tinystack",
	})
	if err != nil {
		return nil, err
	}

	s.faults.reset(cfg.Faults)
	s.consoleCaller = catalog.NewCaller(services...)
	s.internal = map[string]http.Handler{
		"/_tinystack/health":         http.HandlerFunc(s.serveHealth),
		"/_tinystack/faults":         http.HandlerFunc(s.serveFaults),
		"/_localstack/chaos/faults":  http.HandlerFunc(s.serveFaults),
		"/_localstack/chaos/effects": http.HandlerFunc(s.serveEffects),
		"/_tinystack/requests":       s.requests,
		"/_tinystack/resources":      http.HandlerFunc(s.serveResources),
		"/_tinystack/metrics":        http.HandlerFunc(s.serveMetrics),
		"/_tinystack/actions":        http.HandlerFunc(s.serveConsoleActions),
		"/_localstack/health":        http.HandlerFunc(s.serveLocalStackHealth),
	}

	return s, nil
}

// UseManagementEvents observes completed external AWS requests only. Console and
// catalog.Caller operations bypass ServeHTTP and cannot enter this hook.
func (s *Server) UseManagementEvents(record func(context.Context, *catalog.ManagementEvent) error) {
	s.managementEvents = record
}

// UseCredentials resolves access keys that are not configured, such as IAM
// users' keys and the temporary keys STS issues, through lookup. Call it
// before serving requests.
func (s *Server) UseCredentials(lookup CredentialLookup) {
	s.credentials = lookup
}

// UseDocuments serves GET /<issuer>/.well-known/<name> through documents.
// Issuers such as Cognito pool IDs contain "_", which no S3 bucket name does,
// so these paths never name an object. Call it before serving requests.
func (s *Server) UseDocuments(documents Documents) {
	s.documents = documents
}

// UseTokens serves POST /<issuer>/oauth2/token through tokens.
func (s *Server) UseTokens(tokens Tokens) {
	s.tokens = tokens
}

// serveToken serves an issuer's token endpoint, reporting whether the request
// asked for it.
func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) bool {
	issuer, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/oauth2/token")
	if s.tokens == nil || !ok || !strings.Contains(issuer, "_") || strings.Contains(issuer, "/") {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
		return true
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
		return true
	}

	status, body, err := s.tokens(r.Context(), issuer, "http://"+r.Host, r.Header.Get("Authorization"), r.PostForm)
	if err != nil {
		s.logger.Error("serve token", "issuer", issuer, "error", err)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true
}

// serveDocument serves an issuer's well-known document, reporting whether
// the request asked for one.
func (s *Server) serveDocument(w http.ResponseWriter, r *http.Request) bool {
	issuer, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/.well-known/")
	if s.documents == nil || r.Method != http.MethodGet || !ok || !strings.Contains(issuer, "_") ||
		strings.ContainsAny(issuer+name, "/") {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	doc, found, err := s.documents(r.Context(), issuer, name, "http://"+r.Host)
	switch {
	case err != nil:
		s.logger.Error("serve document", "issuer", issuer, "name", name, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal error"}`))
	case !found:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	default:
		_, _ = w.Write(doc)
	}

	return true
}

// Handle serves h at /_tinystack/<name>. Call it before serving requests.
func (s *Server) Handle(name string, h http.Handler) {
	s.HandlePath("/_tinystack/"+name, h)
}

// HandlePath serves h at path, which starts with "/_" so that it never names an
// S3 bucket, such as LocalStack's /_localstack/ paths. Call it before serving
// requests.
func (s *Server) HandlePath(path string, h http.Handler) {
	s.internal[path] = h
}

// ClearRequests forgets the requests the inspector kept.
func (s *Server) ClearRequests() {
	s.requests.Clear()
}

// Internal serves only TinyStack's own /_tinystack/ endpoints, answering 404
// for every other path, for the console's listener.
func (s *Server) Internal() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := s.internal[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// identify resolves the caller scope from the host and SigV4 credential scope,
// returning the signed service name. authenticate verifies the signature.
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

	a, signed, err := signature(r)
	if err != nil || !signed {
		return "", err
	}

	m.AccessKey = a.AccessKey
	m.Region = a.Region
	m.Service = a.Service
	if account, ok := s.cfg.Identity.Accounts[m.AccessKey]; ok {
		m.AccountID = account
	}

	return a.Service, nil
}

// resolveCredentials maps an access key TinyStack issued to its account.
// Configured keys win; authenticate checks the secret, token, and expiry.
func (s *Server) resolveCredentials(ctx context.Context, c *call) error {
	key := c.meta.AccessKey
	if s.credentials == nil || key == "" {
		return nil
	}

	_, mapped := s.cfg.Identity.Accounts[key]
	_, secret := s.cfg.Identity.SecretKeys[key]
	if mapped || secret {
		return nil
	}

	credentials, ok, err := s.credentials(ctx, key)
	if err != nil || !ok {
		return err
	}

	c.credentials = &credentials
	c.meta.AccountID = credentials.AccountID
	return nil
}

// classify selects the protocol from headers alone, so early errors use the
// client's envelope, then resolves the service and operation.
func (s *Server) classify(w http.ResponseWriter, r *http.Request, contentType string) (*call, error) {
	c := &call{meta: request.Metadata{
		Scope:    request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
		Protocol: catalog.AWSJSON10,
	}}
	target := r.Header.Get("X-Amz-Target")
	cbor := r.Header.Get(rpcv2cbor.Header) == rpcv2cbor.Protocol
	switch {
	case cbor:
		c.meta.Protocol = catalog.RPCV2CBOR
	case target == "" && (contentType == formContentType || queryGET(r)):
		c.meta.Protocol = catalog.AWSQuery
	}

	if target == "" && !cbor {
		// REST services are chosen by the signed or host service name; unsigned
		// requests without a JSON target or form body fall back to S3.
		if rest := s.restServices(r); len(rest) > 0 {
			c.meta.Protocol = restProtocol(rest[0])
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

	if guardErr := s.resolveCredentials(r.Context(), c); guardErr != nil {
		return c, guardErr
	}

	switch c.meta.Protocol {
	case catalog.AWSQuery:
		// EC2 Query is AWS Query's form request with other names and XML.
		return c, s.classifyQuery(w, r, c, signed)
	case catalog.RPCV2CBOR:
		return c, s.classifyCBOR(r, c, signed)
	case catalog.RESTXML, catalog.RESTJSON:
		// Services that share a signing name, such as API Gateway's REST and
		// HTTP API management APIs, are told apart by their routes.
		for _, service := range s.restServices(r) {
			c.service = service
			if signed != "" && signed != c.service.SigningName {
				return c, awserror.New("IncompleteSignatureException", "Signing service does not match the request")
			}

			c.meta.Service = c.service.Name
			if c.labels, err = s.restOperation(r, c); err == nil {
				return c, nil
			}
		}

		return c, err
	}

	prefix, op, ok := strings.Cut(target, ".")
	if !ok || op == "" {
		return c, awserror.New("UnknownOperationException", "X-Amz-Target must identify an operation")
	}

	for _, service := range s.services {
		if service.TargetPrefix == prefix && jsonProtocol(service) != "" {
			c.service = service
		}
	}

	if c.service == nil {
		return c, awserror.New("UnknownOperationException", "Unknown target prefix")
	}

	c.meta.Protocol = jsonProtocol(c.service)

	if signed != "" && signed != c.service.SigningName {
		return c, awserror.Validation("Signing service conflicts with X-Amz-Target")
	}

	c.meta.Service = c.service.Name
	c.meta.Operation = op
	return c, nil
}

// classifyCBOR resolves an RPC v2 CBOR request's service and operation from
// its path, /service/<service shape>/operation/<operation>.
func (s *Server) classifyCBOR(r *http.Request, c *call, signed string) error {
	rest, ok := strings.CutPrefix(r.URL.Path, "/service/")
	name, op, split := strings.Cut(rest, "/operation/")
	if !ok || !split || name == "" || op == "" {
		return awserror.New("UnknownOperationException", "The path must be /service/<service>/operation/<operation>")
	}

	for _, service := range s.services {
		if service.TargetPrefix == name && service.Supports(catalog.RPCV2CBOR) {
			c.service = service
		}
	}

	switch {
	case c.service == nil:
		return awserror.New("UnknownOperationException", "Unknown service "+name)
	case signed != "" && signed != c.service.SigningName:
		return awserror.Validation("Signing service conflicts with the request path")
	}

	c.meta.Service, c.meta.Operation = c.service.Name, op
	return nil
}

// jsonProtocol returns the AWS JSON version a service speaks, or "".
func jsonProtocol(service *catalog.Service) string {
	for _, protocol := range []string{catalog.AWSJSON10, catalog.AWSJSON11} {
		if service.Supports(protocol) {
			return protocol
		}
	}

	return ""
}

// classifyQuery reads the form once; decoding later reuses the parsed values.
func (s *Server) classifyQuery(w http.ResponseWriter, r *http.Request, c *call, signed string) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return awserror.New("InvalidParameterValue", "Cannot read request body: "+err.Error())
	}

	c.body = body
	form, err := inflate(r, bytes.NewReader(body))
	if err != nil {
		return err
	}

	if queryGET(r) {
		form = []byte(r.URL.RawQuery)
	}

	if c.form, err = url.ParseQuery(string(form)); err != nil {
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
		if !isQuery(service) {
			continue
		}

		if name != "" && service.SigningName == name {
			candidates = append(candidates, service)
			continue
		}

		if _, ok := service.Operations[c.meta.Operation]; ok && name == "" {
			candidates = append(candidates, service)
		}
	}

	// Services that share a signing name and an action, such as Classic and
	// v2 Elastic Load Balancing, are told apart by the request's API Version.
	if len(candidates) > 1 {
		candidates = slices.DeleteFunc(candidates, func(service *catalog.Service) bool {
			return !servesVersion(service, c.meta.Operation, c.form.Get("Version"))
		})
	}

	if len(candidates) != 1 {
		return awserror.New("InvalidAction", "Cannot resolve a service for action "+c.meta.Operation)
	}

	c.service = candidates[0]
	c.meta.Service = c.service.Name
	if c.service.Supports(catalog.EC2Query) {
		c.meta.Protocol = catalog.EC2Query
	}

	return nil
}

// servesVersion reports whether a Query service has the operation at the API
// version; a request without a Version matches any service that has it.
func servesVersion(service *catalog.Service, operation, version string) bool {
	op, ok := service.Operations[operation]
	if !ok {
		return false
	}

	for protocol, binding := range op.Bindings {
		if formProtocol(protocol) && (version == "" || binding.Version == version) {
			return true
		}
	}

	return false
}

// queryGET reports whether a request is an AWS Query request sent as a GET to
// "/" with its parameters in the URL, such as the SubscribeURL in an SNS
// subscription confirmation.
func queryGET(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Get("Action") != ""
}

// isQuery reports whether a service takes form requests: AWS Query or EC2
// Query.
func isQuery(service *catalog.Service) bool {
	return service.Supports(catalog.AWSQuery) || service.Supports(catalog.EC2Query)
}

// formProtocol reports whether protocol sends form requests.
func formProtocol(protocol string) bool {
	return protocol == catalog.AWSQuery || protocol == catalog.EC2Query
}

// restServices selects the REST services for a request, in name order: those
// named by the signed scope or regional host, or S3 for unsigned requests that
// are neither AWS JSON nor AWS Query, since S3 is the REST service that serves
// anonymous requests such as presigned URLs.
func (s *Server) restServices(r *http.Request) []*catalog.Service {
	var m request.Metadata
	signed, err := s.identify(r, &m)
	name := signed
	if name == "" {
		name = m.Service
	}

	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if name == "" && (err != nil || contentType == formContentType || queryGET(r)) {
		return nil
	}

	if name == "" {
		name = "s3"
	}

	// A form request to a name that a Query service also signs with, such as
	// SES v1 beside the REST JSON SES v2, is that Query service's.
	if (contentType == formContentType || queryGET(r)) && s.signsQuery(name) {
		return nil
	}

	var out []*catalog.Service
	for _, service := range s.services {
		if restProtocol(service) != "" && service.SigningName == name {
			out = append(out, service)
		}
	}

	slices.SortFunc(out, func(a, b *catalog.Service) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// signsQuery reports whether a Query service signs with name.
func (s *Server) signsQuery(name string) bool {
	for _, service := range s.services {
		if isQuery(service) && service.SigningName == name {
			return true
		}
	}

	return false
}

func (s *Server) decode(
	w http.ResponseWriter,
	r *http.Request,
	c *call,
	contentType string,
) (catalog.Operation, any, error) {
	op, ok := c.service.Operations[c.meta.Operation]
	if restProtocol(c.service) == c.meta.Protocol {
		input, err := s.decodeREST(w, r, c, &op)
		return op, input, err
	}

	if !ok {
		if formProtocol(c.meta.Protocol) {
			return op, nil, awserror.New(
				"InvalidAction",
				"The action "+c.meta.Operation+" is not valid for this endpoint",
			)
		}

		return op, nil, awserror.New("UnknownOperationException", "Operation is not implemented")
	}

	binding, supported := op.Bindings[c.meta.Protocol]
	// An AWS Query request may also be a GET with its parameters in the URL.
	get := c.meta.Protocol == catalog.AWSQuery && queryGET(r)
	if !supported || (contentType != binding.ContentType && !get) {
		return op, nil, awserror.Validation("Unsupported content type for this operation")
	}
	// Query clients may post to a resource URL such as an SQS queue URL.
	pathOK := formProtocol(c.meta.Protocol) || r.URL.Path == binding.URI
	if !get && (r.Method != binding.Method || !pathOK || r.URL.RawQuery != "") {
		return op, nil, awserror.Validation("Unexpected method, path, or query parameters")
	}

	input := op.NewInput()
	if formProtocol(c.meta.Protocol) {
		if versions := c.form["Version"]; len(versions) > 0 && (len(versions) != 1 || versions[0] != binding.Version) {
			return op, nil, awserror.New("NoSuchVersion", "Version must be "+binding.Version)
		}

		if c.meta.Protocol == catalog.EC2Query {
			return op, input, query.DecodeEC2(c.form, input)
		}

		return op, input, query.Decode(c.form, input)
	}

	body, err := inflate(r, http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return op, nil, err
	}

	if c.meta.Protocol == catalog.RPCV2CBOR {
		return op, input, rpcv2cbor.Decode(bytes.NewReader(body), input)
	}

	return op, input, awsjson.Decode(bytes.NewReader(body), input)
}

// inflate reads a request body, undoing the gzip Content-Encoding that
// clients apply to operations with Smithy's requestCompression trait (such as
// CloudWatch's PutMetricData) after signing, so signatures cover the
// compressed bytes. The decompressed body is limited to maxBody.
func inflate(r *http.Request, body io.Reader) ([]byte, error) {
	var err error
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		if body, err = gzip.NewReader(body); err != nil {
			return nil, awserror.Validation("The gzip request body cannot be read: %v", err)
		}

		body = io.LimitReader(body, maxBody+1)
	}

	data, err := io.ReadAll(body)
	switch {
	case err != nil:
		return nil, awserror.Validation("Cannot read request body: %v", err)
	case len(data) > maxBody:
		return nil, awserror.Validation("The decompressed request body is larger than %d bytes", maxBody)
	}

	return data, nil
}

func (s *Server) fail(w http.ResponseWriter, c *call, err error) {
	e := awserror.From(err)
	// WAF uses its prefixed modeled parameter error for malformed/unsupported
	// JSON members, including fields rejected before the service is invoked.
	if c.service != nil && c.service.Name == "wafv2" && e.Code == validationException {
		e.Code = "WAFInvalidParameterException"
	}
	if c.service != nil && c.service.Name == "efs" && e.Code == validationException {
		e.Code = "BadRequest"
		e.Fields = map[string]any{"ErrorCode": "BadRequest"}
	}
	if c.service != nil && c.service.Name == "batch" && e.Code == validationException {
		e.Code = "ClientException"
	}
	if c.service != nil && c.service.Name == "transfer" && e.Code == validationException {
		e.Code = "InvalidRequestException"
	}
	c.failure = e
	for name, value := range e.Headers {
		w.Header().Set(name, value)
	}

	namespace, messageMember := "", ""
	if c.service != nil {
		if binding, ok := c.service.Errors[e.Code]; ok {
			if !c.faultError {
				e.StatusCode = binding.StatusCode
			}

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
	case catalog.EC2Query:
		query.WriteEC2Error(w, c.meta.RequestID, e)
	case catalog.RESTXML:
		wrapped := ""
		if c.service != nil && c.service.WrappedErrors {
			wrapped = namespace
		}

		restxml.WriteError(w, e, c.meta.RequestID, wrapped, c.head)
	case catalog.RESTJSON:
		restjson.WriteError(w, e, messageMember)
	case catalog.RPCV2CBOR:
		rpcv2cbor.WriteError(w, e)
	default:
		awsjson.WriteError(w, c.meta.Protocol, e, messageMember)
	}
}

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.health)
}

func (s *Server) serveLocalStackHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_, _ = w.Write(s.localStackHealth)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// "_" is not valid in an S3 bucket name, so these paths never name one.
	if h, ok := s.internal[r.URL.Path]; ok {
		h.ServeHTTP(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/_tinystack/") || strings.HasPrefix(r.URL.Path, "/_localstack/") {
		http.NotFound(w, r)
		return
	}

	if s.serveDocument(w, r) || s.serveToken(w, r) || s.serveRegistry(w, r) || s.serveMQTT(w, r) || s.serveAPI(w, r) ||
		s.servePreflight(w, r) {
		return
	}

	start := time.Now()
	body := &capturingBody{ReadCloser: r.Body}
	r.Body = body
	cw := &capturingWriter{ResponseWriter: w}
	w = cw
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	c, err := s.classify(w, r, contentType)
	c.head = r.Method == http.MethodHead
	m := &c.meta
	w.Header().Set("x-amzn-RequestId", m.RequestID)
	if m.Protocol == catalog.RESTXML {
		w.Header().Set("x-amz-request-id", m.RequestID)
	}

	defer func() {
		s.record(r, c, start, body, cw)
		if c != nil {
			c.close()
		}
	}()

	if err == nil {
		s.addCORS(w, r, c)
		err = s.authenticate(w, r, c)
	}

	if err != nil {
		s.fail(w, c, err)
		return
	}

	op, input, err := s.decode(w, r, c, contentType)
	if err != nil {
		s.fail(w, c, err)
		return
	}

	if faultErr := s.inject(r.Context(), c); faultErr != nil {
		s.fail(w, c, faultErr)
		return
	}

	ctx := request.WithMetadata(r.Context(), c.meta)
	if c.service.SigningName == "s3" {
		switch c.meta.Operation {
		case opGetObject, opPutObject, opUploadPart:
			if c.payload == nil {
				c.payload = &request.Payload{}
			}
			ctx = request.WithPayload(ctx, c.payload)
		}
	}
	output, err := op.Invoke(ctx, input)
	if err != nil {
		s.fail(w, c, err)
		return
	}

	binding := op.Bindings[m.Protocol]
	status := binding.StatusCode
	switch m.Protocol {
	case catalog.RESTXML:
		if c.payload != nil && c.payload.Body != nil {
			response := restbindings.Encode(w, &binding, output)
			w.WriteHeader(response.Status)
			if !c.head {
				_, _ = io.Copy(w, c.payload.Body)
			}
		} else {
			restxml.Write(w, &binding, output, c.head)
		}
		return
	case catalog.RESTJSON:
		restjson.Write(w, &binding, output)
		return
	case catalog.RPCV2CBOR:
		rpcv2cbor.Write(w, status, output)
		return
	}

	if formProtocol(m.Protocol) {
		if op.UnitOutput {
			output = nil
		}

		if m.Protocol == catalog.EC2Query {
			query.WriteEC2(w, status, m.Operation, c.service.XMLNamespace, m.RequestID, output)
			return
		}

		query.Write(w, status, m.Operation, c.service.XMLNamespace, m.RequestID, output)
		return
	}

	awsjson.Write(w, m.Protocol, status, output)
}

// record logs a finished request and adds it to the request inspector.
func (s *Server) record(r *http.Request, c *call, start time.Time, body *capturingBody, w *capturingWriter) {
	m := &c.meta
	rec := Record{
		Injected:     c.injected,
		Time:         start.UTC(),
		RequestID:    m.RequestID,
		Method:       r.Method,
		Path:         r.URL.Path,
		Service:      m.Service,
		Operation:    m.Operation,
		Protocol:     m.Protocol,
		Account:      m.AccountID,
		Region:       m.Region,
		Status:       w.status,
		RequestBody:  body.text(),
		RequestSize:  body.size,
		ResponseBody: w.text(),
		ResponseSize: w.size,
		DurationMS:   float64(time.Since(start).Microseconds()) / 1000,
	}

	if rec.Status == 0 {
		rec.Status = http.StatusOK
	}

	if c.failure != nil {
		rec.ErrorCode, rec.ErrorMessage = c.failure.Code, c.failure.Message
	}

	if c.service != nil && c.service.Operations[m.Operation].Sensitive {
		rec.RequestBody, rec.ResponseBody = redacted, redacted
	}

	if s.managementEvents != nil && c.service != nil && m.Operation != "" {
		source := r.RemoteAddr
		if host, _, err := net.SplitHostPort(source); err == nil {
			source = host
		}
		event := &catalog.ManagementEvent{
			Metadata: *m, Time: rec.Time, SourceIPAddress: source,
			UserAgent: r.UserAgent(), ErrorCode: rec.ErrorCode, ErrorMessage: rec.ErrorMessage,
		}
		if c.service.Operations[m.Operation].Sensitive && event.ErrorMessage != "" {
			event.ErrorMessage = redacted
		}
		if m.Service == "s3" && c.labels["Bucket"] != "" {
			event.Resources = []catalog.EventResource{{Type: "AWS::S3::Bucket", Name: c.labels["Bucket"]}}
		}
		if err := s.managementEvents(context.WithoutCancel(r.Context()), event); err != nil {
			s.logger.Error("record management event", "error", err)
		}
	}

	s.requests.Add(rec)
	s.metrics.add(c, rec.Status, rec.DurationMS/1000)
	s.logger.Info(
		"request",
		"request_id", rec.RequestID,
		"service", rec.Service,
		"operation", rec.Operation,
		"protocol", rec.Protocol,
		"account", rec.Account,
		"region", rec.Region,
		"status", rec.Status,
		"error_code", rec.ErrorCode,
		"injected", rec.Injected,
		"duration_ms", rec.DurationMS,
	)
}

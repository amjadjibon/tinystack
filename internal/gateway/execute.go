package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

// maxExecuteBody bounds request bodies to APIs, as API Gateway's 10 MB payload
// limit does.
const maxExecuteBody = 10 << 20

// executePath is the path prefix that addresses an API by ID on TinyStack's
// own host: /_aws/execute-api/<API ID>/<stage>/... ("_" is not valid in an S3
// bucket name, so the path never names one).
const executePath = "/_aws/execute-api/"

const executeService = "execute-api"

// UseAPIs serves the APIs that API Gateway services run: requests to
// <API ID>.execute-api.<host> (such as abc123.execute-api.localhost:4566 or
// abc123.execute-api.us-east-1.amazonaws.com), or to
// /_aws/execute-api/<API ID>/... on any host. Call it before serving requests.
func (s *Server) UseAPIs(handlers ...execute.Handler) {
	s.apis = append(s.apis, handlers...)
}

// elbPath is the path prefix that addresses a load balancer by the first label
// of its DNS name: /_aws/elb/<label>/..., or /_aws/elb/<label>:<port>/... for
// the listener on that port.
const elbPath = "/_aws/elb/"

// UseLoadBalancers serves the load balancers Elastic Load Balancing runs:
// requests to their DNS names (<label>.<region>.elb.amazonaws.com,
// <label>.elb.<region>.amazonaws.com, or <label>.elb.localhost:4566), or to
// /_aws/elb/<label>/... on any host. Call it before serving requests.
func (s *Server) UseLoadBalancers(handler execute.Handler) {
	s.balancers = handler
}

// domainPath is the path prefix that addresses an API Gateway custom domain
// name: /_aws/domain/<domain name>/...
const domainPath = "/_aws/domain/"

// Domains serves custom domain names, which Known recognizes by host.
type Domains interface {
	execute.Handler
	Known(ctx context.Context, host string) bool
}

// UseDomains serves API Gateway custom domain names: unsigned requests whose
// host is a domain name, or requests to /_aws/domain/<domain name>/... on any
// host. Call it before serving requests.
func (s *Server) UseDomains(domains Domains) {
	s.domains = domains
}

// domainTarget returns the custom domain a request addresses and the path
// after it, if the request is to one.
func (s *Server) domainTarget(r *http.Request) (name, path, endpoint string, ok bool) {
	if rest, found := strings.CutPrefix(r.URL.Path, domainPath); found {
		name, path, _ = strings.Cut(rest, "/")
		return name, "/" + path, "http://" + r.Host + domainPath + name, name != ""
	}

	host, ok := s.domainHost(r)
	return host, r.URL.Path, "http://" + r.Host, ok
}

// domainHost returns the custom domain a request's host is, if it is one.
// SigV4 requests are AWS's, so they never pay for the lookup.
func (s *Server) domainHost(r *http.Request) (string, bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if s.domains == nil || strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-") ||
		sigv4.Presigned(r.URL.Query()) || !strings.Contains(host, ".") || !s.domains.Known(r.Context(), host) {
		return "", false
	}

	return host, true
}

// lambdaURLPath is the path prefix that addresses a Lambda function URL by
// its ID: /_aws/lambda-url/<URL ID>/...
const lambdaURLPath = "/_aws/lambda-url/"

// UseFunctionURLs serves Lambda function URLs: requests to
// <URL ID>.lambda-url.<region>.<host> (such as
// abc.lambda-url.us-east-1.localhost:4566 or abc.lambda-url.us-east-1.on.aws),
// or to /_aws/lambda-url/<URL ID>/... on any host. Call it before serving
// requests.
func (s *Server) UseFunctionURLs(handler execute.Handler) {
	s.functionURLs = handler
}

// graphQLPath is the path prefix that addresses an AppSync GraphQL API by its
// ID: /_aws/appsync-api/<API ID>/graphql.
const graphQLPath = "/_aws/appsync-api/"

// GraphQL serves GraphQL APIs over HTTP and their real-time WebSocket
// connections.
type GraphQL interface {
	execute.Handler
	execute.SocketHandler
}

// UseGraphQL serves AppSync GraphQL APIs: requests to
// <API ID>.appsync-api.<host> (such as abc.appsync-api.localhost:4566 or
// abc.appsync-api.us-east-1.amazonaws.com), or to
// /_aws/appsync-api/<API ID>/... on any host, and WebSocket connections to
// those or to <API ID>.appsync-realtime-api.<host>. Call it before serving
// requests.
func (s *Server) UseGraphQL(handler GraphQL) {
	s.graphQL = handler
}

// labelTarget returns the ID a request addresses and the path after it, if
// the request is to <ID><marker>... or to <prefix><ID>/... on any host, as
// requests to function URLs and GraphQL APIs are.
func labelTarget(r *http.Request, marker, prefix string) (id, path, endpoint string, ok bool) {
	if sub, _, found := strings.Cut(r.Host, marker); found && sub != "" && !strings.Contains(sub, ".") {
		return sub, r.URL.Path, "http://" + r.Host, true
	}

	rest, found := strings.CutPrefix(r.URL.Path, prefix)
	if !found {
		return "", "", "", false
	}

	id, path, _ = strings.Cut(rest, "/")
	return id, "/" + path, "http://" + r.Host + prefix + id, id != ""
}

// elbTarget returns the load balancer a request addresses and the path after
// it, if the request is to one.
func elbTarget(r *http.Request) (label, path, endpoint string, ok bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if sub, rest, found := strings.Cut(host, "."); found && sub != "" &&
		(strings.HasPrefix(rest, "elb.") || strings.Contains(rest, ".elb.")) {
		return sub, r.URL.Path, "http://" + r.Host, true
	}

	rest, found := strings.CutPrefix(r.URL.Path, elbPath)
	if !found {
		return "", "", "", false
	}

	label, path, _ = strings.Cut(rest, "/")
	return label, "/" + path, "http://" + r.Host + elbPath + label, label != ""
}

// executeTarget returns the API a request addresses and the path after it,
// if the request is to an API.
func executeTarget(r *http.Request) (id, path, endpoint string, ok bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if sub, _, found := strings.Cut(host, ".execute-api."); found && sub != "" && !strings.Contains(sub, ".") {
		return sub, r.URL.Path, "http://" + r.Host, true
	}

	rest, found := strings.CutPrefix(r.URL.Path, executePath)
	if !found {
		return "", "", "", false
	}

	id, path, _ = strings.Cut(rest, "/")
	return id, "/" + path, "http://" + r.Host + executePath + id, id != ""
}

// serveAPI runs a request to an API, a load balancer, a function URL, or a
// GraphQL API, reporting whether the request was one.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) bool {
	id, path, endpoint, ok := executeTarget(r)
	handlers := s.apis
	if !ok && s.balancers != nil {
		id, path, endpoint, ok = elbTarget(r)
		handlers = []execute.Handler{s.balancers}
	}

	if !ok && s.functionURLs != nil {
		id, path, endpoint, ok = labelTarget(r, ".lambda-url.", lambdaURLPath)
		handlers = []execute.Handler{s.functionURLs}
	}

	graphQL := false
	if !ok && s.graphQL != nil {
		id, path, endpoint, ok = labelTarget(r, ".appsync-api.", graphQLPath)
		if !ok {
			id, path, endpoint, ok = labelTarget(r, ".appsync-realtime-api.", graphQLPath)
		}

		handlers, graphQL = []execute.Handler{s.graphQL}, ok
	}

	if !ok && s.domains != nil {
		id, path, endpoint, ok = s.domainTarget(r)
		handlers = []execute.Handler{s.domains}
	}

	if !ok || len(handlers) == 0 {
		return false
	}

	sourceIP := r.RemoteAddr
	if h, _, splitErr := net.SplitHostPort(sourceIP); splitErr == nil {
		sourceIP = h
	}

	req := &execute.Request{
		Time: time.Now().UTC(), Header: r.Header.Clone(), Query: r.URL.Query(), Method: r.Method, Path: path,
		RawQuery: r.URL.RawQuery, Host: r.Host, Endpoint: endpoint, SourceIP: sourceIP, Protocol: r.Proto,
	}
	if graphQL && upgrade(r) {
		s.serveSocket(w, r, id, req)
		return true
	}

	start := time.Now()
	cw := &capturingWriter{ResponseWriter: w}
	w = cw
	var executionCall *call
	defer func() {
		if executionCall != nil {
			executionCall.meta.RequestID = cw.Header().Get("X-Amzn-Requestid")
			if executionCall.meta.RequestID == "" {
				executionCall.meta.RequestID = cw.Header().Get("Apigw-Requestid")
			}

			body := &capturingBody{}
			body.keep(req.Body)
			s.record(r, executionCall, start, body, cw)
		}
	}()

	req.Caller = s.caller(w, r)
	req.AuthorizeTarget = func(
		ctx context.Context,
		identity execute.Identity,
		targets ...catalog.AuthorizationResource,
	) error {
		service, operation, _ := strings.Cut(targets[0].Action, ":")
		c := &call{
			identity: identity,
			service: &catalog.Service{
				Name: service, SigningName: service,
				Operations: map[string]catalog.Operation{operation: {Name: operation}},
			},
			meta: request.Metadata{
				Scope:   request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
				Service: service, Operation: operation, Protocol: catalog.RESTJSON,
			},
		}
		executionCall = c
		if identity == execute.Signed {
			if _, err := s.identify(r, &c.meta); err != nil {
				return err
			}

			c.meta.Service = service
			if err := s.resolveCredentials(ctx, c); err != nil {
				return err
			}
		}

		return s.authorizeTargets(ctx, r, c, &catalog.Operation{
			Name: operation, IAMAction: targets[0].Action,
		}, nil, targets)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExecuteBody))
	if err != nil {
		writeExecute(w, execute.Message(http.StatusRequestEntityTooLarge, "Request Entity Too Large"))
		return true
	}

	req.Body = body
	for _, h := range handlers {
		resp, found, execErr := h.Execute(r.Context(), id, req)
		if execErr != nil {
			s.logger.Warn("api request failed", "api", id, "path", path, "error", execErr)
			resp, found = execute.Message(http.StatusInternalServerError, "Internal Server Error"), true
		}

		if found {
			writeExecute(w, resp)
			return true
		}
	}

	writeExecute(w, execute.Message(http.StatusNotFound, "Not Found"))
	return true
}

// caller returns the account and access key that signed r, or nil when r is
// unsigned or, in identity.mode verify, its signature does not verify.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) *execute.Caller {
	c := &call{
		service: &catalog.Service{Protocols: []string{catalog.RESTJSON}},
		meta: request.Metadata{
			Scope:    request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
			Protocol: catalog.RESTJSON,
		},
	}
	signed, err := s.identify(r, &c.meta)
	if err != nil || signed == "" {
		return nil
	}

	c.service.SigningName = signed
	if s.resolveCredentials(r.Context(), c) != nil || s.authenticate(w, r, c) != nil {
		return nil
	}

	caller := &execute.Caller{AccountID: c.meta.AccountID, Region: c.meta.Region, AccessKey: c.meta.AccessKey}
	if c.credentials != nil {
		caller.CognitoIdentity = c.credentials.CognitoIdentity
		caller.Principal = &c.credentials.Principal
	}

	if caller.Principal == nil || caller.Principal.ARN == "" {
		arn := "arn:aws:iam::" + caller.AccountID + ":root"
		caller.Principal = &request.Principal{
			Kind: request.PrincipalRoot, Account: caller.AccountID, UserID: caller.AccountID, ARN: arn, PolicyARN: arn,
		}
	}

	return caller
}

func writeExecute(w http.ResponseWriter, resp *execute.Response) {
	for k, v := range resp.Header {
		w.Header()[k] = v
	}

	w.WriteHeader(resp.Status)
	// #nosec G705 -- the API or load balancer's response, sent as its integration returned it.
	_, _ = w.Write(resp.Body)
}

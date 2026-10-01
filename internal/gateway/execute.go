package gateway

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
)

// maxExecuteBody bounds request bodies to APIs, as API Gateway's 10 MB payload
// limit does.
const maxExecuteBody = 10 << 20

// executePath is the path prefix that addresses an API by ID on TinyStack's
// own host: /_aws/execute-api/<API ID>/<stage>/... ("_" is not valid in an S3
// bucket name, so the path never names one).
const executePath = "/_aws/execute-api/"

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

// serveAPI runs a request to an API or a load balancer, reporting whether the
// request was one.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) bool {
	id, path, endpoint, ok := executeTarget(r)
	handlers := s.apis
	if !ok && s.balancers != nil {
		id, path, endpoint, ok = elbTarget(r)
		handlers = []execute.Handler{s.balancers}
	}

	if !ok || len(handlers) == 0 {
		return false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExecuteBody))
	if err != nil {
		writeExecute(w, execute.Message(http.StatusRequestEntityTooLarge, "Request Entity Too Large"))
		return true
	}

	sourceIP := r.RemoteAddr
	if h, _, splitErr := net.SplitHostPort(sourceIP); splitErr == nil {
		sourceIP = h
	}

	req := &execute.Request{
		Time: time.Now().UTC(), Header: r.Header.Clone(), Query: r.URL.Query(), Method: r.Method, Path: path,
		RawQuery: r.URL.RawQuery, Host: r.Host, Endpoint: endpoint, SourceIP: sourceIP, Protocol: r.Proto,
		Body: body,
	}
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

func writeExecute(w http.ResponseWriter, resp *execute.Response) {
	for k, v := range resp.Header {
		w.Header()[k] = v
	}

	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

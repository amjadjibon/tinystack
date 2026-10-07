package gateway

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	// mqttPath is where IoT Core serves MQTT over WebSocket on every host.
	mqttPath = "/mqtt"
	// mqttExpires is how long a presigned MQTT URL without X-Amz-Expires
	// stays valid after its X-Amz-Date.
	mqttExpires = 15 * time.Minute
)

// UseMQTT serves MQTT over WebSocket: GET /mqtt upgrade requests with the
// mqtt subprotocol, on any host, as IoT Core's data endpoints serve it. In
// identity.mode verify a presigned request must verify; an unsigned one is
// handed over without a caller, for a custom authorizer to decide. Call it
// before serving requests.
func (s *Server) UseMQTT(handler execute.MQTT) {
	s.mqtt = handler
}

// serveMQTT runs an MQTT connection over WebSocket, reporting whether the
// request was one.
func (s *Server) serveMQTT(w http.ResponseWriter, r *http.Request) bool {
	if s.mqtt == nil || r.URL.Path != mqttPath || !upgrade(r) {
		return false
	}

	sourceIP := r.RemoteAddr
	if h, _, err := net.SplitHostPort(sourceIP); err == nil {
		sourceIP = h
	}

	req := &execute.Request{
		Time: time.Now().UTC(), Header: r.Header.Clone(), Query: r.URL.Query(), Method: r.Method, Path: r.URL.Path,
		RawQuery: r.URL.RawQuery, Host: r.Host, Endpoint: "http://" + r.Host, SourceIP: sourceIP, Protocol: r.Proto,
		Caller: s.mqttCaller(w, r),
	}
	if req.Caller == nil && s.cfg.Identity.Mode == config.IdentityVerify && signed(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return true
	}

	// The server's read and write timeouts would end a long-lived connection.
	rc := http.NewResponseController(w)
	if err := errors.Join(rc.SetReadDeadline(time.Time{}), rc.SetWriteDeadline(time.Time{})); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		s.logger.Warn("mqtt connection failed", "error", err)
		return true
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"mqtt"}, InsecureSkipVerify: true,
	})
	if err != nil {
		return true
	}

	nc := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
	if err = s.mqtt.ServeMQTT(r.Context(), req, nc); err != nil {
		s.logger.Warn("mqtt connection failed", "error", err)
	}

	_ = nc.Close()
	return true
}

// signed reports whether a request carries a SigV4 signature, in its
// Authorization header or presigned query.
func signed(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.URL.Query().Get("X-Amz-Signature") != ""
}

// mqttCaller returns who presigned an MQTT connection request. AWS IoT device
// SDKs append the session token after signing, so a signature that does not
// verify with the token in the query is tried without it.
func (s *Server) mqttCaller(w http.ResponseWriter, r *http.Request) *execute.Caller {
	if c := s.caller(w, r); c != nil {
		return c
	}

	query := r.URL.Query()
	token := query.Get(sigv4.QuerySecurityToken)
	if token == "" {
		return nil
	}

	query.Del(sigv4.QuerySecurityToken)
	unsigned := r.Clone(r.Context())
	unsigned.URL.RawQuery = query.Encode()
	unsigned.Header.Set("X-Amz-Security-Token", token)
	return s.caller(w, unsigned)
}

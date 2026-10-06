package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
)

// socket is a WebSocket connection to an API, accepted when its handler asks.
type socket struct {
	server *Server
	w      http.ResponseWriter
	r      *http.Request
	conn   *websocket.Conn
	// prefix is the path of the API's endpoint, which requests described in
	// messages are relative to.
	prefix string
}

// upgrade reports whether r asks for a WebSocket connection.
func upgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// serveSocket runs a WebSocket connection to a GraphQL API.
func (s *Server) serveSocket(w http.ResponseWriter, r *http.Request, id string, req *execute.Request) {
	u, err := url.Parse(req.Endpoint)
	if err != nil {
		writeExecute(w, execute.Message(http.StatusBadRequest, "Bad Request"))
		return
	}

	sock := &socket{server: s, w: w, r: r, prefix: u.Path}
	found, err := s.graphQL.Connect(r.Context(), id, req, sock)
	if sock.conn != nil {
		if err != nil {
			s.logger.Warn("api connection failed", "api", id, "error", err)
			_ = sock.conn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		_ = sock.conn.Close(websocket.StatusNormalClosure, "")
		return
	}

	switch {
	case err != nil:
		s.logger.Warn("api connection failed", "api", id, "error", err)
		writeExecute(w, execute.Message(http.StatusInternalServerError, "Internal Server Error"))
	case !found:
		writeExecute(w, execute.Message(http.StatusNotFound, "Not Found"))
	}
}

func (k *socket) Accept(subprotocol string) error {
	// The server's read and write timeouts would end a long-lived connection.
	rc := http.NewResponseController(k.w)
	if err := errors.Join(rc.SetReadDeadline(time.Time{}), rc.SetWriteDeadline(time.Time{})); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return err
	}

	// AppSync accepts connections from any origin.
	conn, err := websocket.Accept(k.w, k.r, &websocket.AcceptOptions{
		Subprotocols: []string{subprotocol}, InsecureSkipVerify: true,
	})
	if err != nil {
		return err
	}

	conn.SetReadLimit(maxExecuteBody)
	k.conn = conn
	return nil
}

func (k *socket) Receive(ctx context.Context) ([]byte, error) {
	_, message, err := k.conn.Read(ctx)
	return message, err
}

func (k *socket) Send(ctx context.Context, message []byte) error {
	return k.conn.Write(ctx, websocket.MessageText, message)
}

func (k *socket) Caller(ctx context.Context, header http.Header, path string, body []byte) *execute.Caller {
	host := header.Get("Host")
	if host == "" {
		host = k.r.Host
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+k.prefix+path, bytes.NewReader(body))
	if err != nil {
		return nil
	}

	r.Header = header.Clone()
	r.Host = host
	return k.server.caller(nil, r)
}

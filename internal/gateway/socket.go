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

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
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

// serveSocket dispatches a WebSocket connection to a matching API handler.
func (s *Server) serveSocket(
	w http.ResponseWriter,
	r *http.Request,
	id string,
	req *execute.Request,
	handlers ...execute.SocketHandler,
) {
	u, err := url.Parse(req.Endpoint)
	if err != nil {
		writeExecute(w, execute.Message(http.StatusBadRequest, "Bad Request"))
		return
	}

	sock := &socket{server: s, w: w, r: r, prefix: u.Path}
	found := false
	for _, handler := range handlers {
		found, err = handler.Connect(r.Context(), id, req, sock)
		if found || err != nil {
			break
		}
	}
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
		e := awserror.From(err)
		writeExecute(w, execute.Message(e.StatusCode, e.Message))
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

	// Local APIs accept connections from any origin.
	conn, err := websocket.Accept(k.w, k.r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		Subprotocols:       socketProtocols(subprotocol),
	})
	if err != nil {
		return err
	}

	conn.SetReadLimit(maxExecuteBody)
	k.conn = conn
	return nil
}

func (k *socket) Receive(ctx context.Context) ([]byte, error) {
	kind, message, err := k.conn.Read(ctx)
	if err == nil && kind != websocket.MessageText {
		_ = k.conn.Close(websocket.StatusUnsupportedData, "binary messages are not supported")
		return nil, errors.New("binary messages are not supported")
	}

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

func socketProtocols(protocol string) []string {
	if protocol == "" {
		return nil
	}

	return []string{protocol}
}

func (k *socket) SetReadLimit(limit int64) { k.conn.SetReadLimit(limit) }

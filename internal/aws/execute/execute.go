// Package execute carries requests to the APIs that API Gateway serves, the
// execute-api endpoint, between the gateway and the services that run them.
package execute

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Request is one request to an API.
type Request struct {
	// Caller is who signed the request with SigV4, if anyone did.
	Caller *Caller
	Time   time.Time
	Header http.Header
	Query  url.Values
	// Method is the HTTP method.
	Method string
	// Path is the decoded path after the API: "/<stage>/..." or, for an
	// HTTP API's $default stage, the route's path. It starts with "/".
	Path string
	// RawQuery is the query string without "?".
	RawQuery string
	// Host is the host the client addressed.
	Host string
	// Endpoint is the base URL of the API as the client addressed it.
	Endpoint string
	SourceIP string
	// Protocol is the HTTP version, such as "HTTP/1.1".
	Protocol string
	Body     []byte
}

// Caller is the account and access key that signed a request. In
// identity.mode verify the gateway passes a caller only once its signature
// verifies.
type Caller struct {
	AccountID string
	// Region is the Region the request was signed for.
	Region    string
	AccessKey string
	// CognitoIdentity is the Cognito identity ID the caller's credentials
	// were issued for, if they came from an identity pool.
	CognitoIdentity string
}

// Response is an API's answer.
type Response struct {
	Header http.Header
	Body   []byte
	Status int
}

// Handler runs requests to the APIs of one API Gateway service.
type Handler interface {
	// Execute runs r against the API id names. It reports false when the
	// service has no API with that ID.
	Execute(ctx context.Context, id string, r *Request) (*Response, bool, error)
}

// Message is API Gateway's JSON body for a failure it answers itself, such as
// {"message":"Not Found"}.
func Message(status int, message string) *Response {
	body, _ := json.Marshal(map[string]string{"message": message})
	return &Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}
}

// Socket is a WebSocket connection to an API. The gateway completes the
// handshake when the handler accepts it.
type Socket interface {
	// Accept completes the handshake, choosing subprotocol if the client
	// offered it. Call it before Receive and Send.
	Accept(subprotocol string) error
	// Receive returns the next message.
	Receive(ctx context.Context) ([]byte, error)
	// Send sends a text message. It may be called concurrently with Receive.
	Send(ctx context.Context, message []byte) error
	// Caller returns who signed a request the client describes inside a
	// message, a POST of body to path (relative to the API's endpoint) with
	// header, or nil when it is unsigned or, in identity.mode verify, its
	// signature does not verify.
	Caller(ctx context.Context, header http.Header, path string, body []byte) *Caller
}

// SocketHandler serves WebSocket connections to the APIs of one service.
type SocketHandler interface {
	// Connect serves socket, a connection to the API id names, until it
	// closes. It reports false, without accepting socket, when id names no
	// API or r.Path no endpoint of one.
	Connect(ctx context.Context, id string, r *Request, socket Socket) (bool, error)
}

// MQTT serves MQTT connections that arrive over WebSocket.
type MQTT interface {
	// ServeMQTT serves conn until it closes. r.Caller is who signed the
	// connection request, or nil when it is unsigned.
	ServeMQTT(ctx context.Context, r *Request, conn net.Conn) error
}

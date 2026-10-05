// Package execute carries requests to the APIs that API Gateway serves, the
// execute-api endpoint, between the gateway and the services that run them.
package execute

import (
	"context"
	"encoding/json"
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
	AccessKey string
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

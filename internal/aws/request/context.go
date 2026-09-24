package request

import (
	"context"
	"time"
)

type Scope struct{ AccountID, Region string }

type Metadata struct {
	Scope
	RequestID, Service, Operation, Protocol, AccessKey string
	// Endpoint is the base URL the client used, for resource URLs such as SQS queue URLs.
	Endpoint string
}

// Session is a temporary credential that STS issued. Requests signed with its
// access key act in AccountID; verified requests must also carry Token.
type Session struct {
	Expires   time.Time
	AccountID string
	Secret    string
	Token     string
}

type contextKey struct{}

//nolint:gocritic // copy metadata to keep request identity immutable
func WithMetadata(ctx context.Context, m Metadata) context.Context {
	return context.WithValue(ctx, contextKey{}, m)
}

func FromContext(ctx context.Context) (Metadata, bool) {
	m, ok := ctx.Value(contextKey{}).(Metadata)
	return m, ok
}

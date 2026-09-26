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

// Credentials are what an access key that TinyStack issued stands for:
// requests signed with it act in AccountID and verify with Secret. An IAM
// user's key has no Token and never expires; an STS session's key expires, and
// verified requests must also carry its Token.
type Credentials struct {
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

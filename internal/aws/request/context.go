package request

import "context"

type Scope struct{ AccountID, Region string }

type Metadata struct {
	Scope
	RequestID, Service, Operation, Protocol, AccessKey string
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

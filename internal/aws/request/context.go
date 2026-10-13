package request

import (
	"context"
	"errors"
	"time"
)

type Scope struct{ AccountID, Region string }

// Prefix is the storage key prefix of a service's records of one kind in the scope:
// "<account>/<region>/<service>/<kind>/".
func (s Scope) Prefix(service, kind string) string {
	return s.AccountID + "/" + s.Region + "/" + service + "/" + kind + "/"
}

// Key is the storage key of a service's record of one kind in the scope.
func (s Scope) Key(service, kind, name string) string { return s.Prefix(service, kind) + name }

type Metadata struct {
	// Principal is set for permission-controlled external requests.
	Principal *Principal
	Scope
	RequestID, Service, Operation, Protocol, AccessKey string
	// Endpoint is the base URL the client used, for resource URLs such as SQS queue URLs.
	Endpoint string
}

// Principal identifies an IAM identity independently of credential secrets.
// ARN is the caller ARN; PolicyARN is the underlying user/role ARN. ID is
// the stable IAM ID, without a role session suffix.
type Principal struct {
	Kind      string
	Account   string
	Name      string
	ID        string
	UserID    string
	ARN       string
	PolicyARN string
}

const (
	PrincipalUser = "user"
	PrincipalRole = "role"
	PrincipalRoot = "root"
)

// Credentials are what an access key that TinyStack issued stands for:
// requests signed with it act in AccountID and verify with Secret. An IAM
// user's key has no Token and never expires; an STS session's key expires, and
// verified requests must also carry its Token.
type Credentials struct {
	Principal Principal
	Expires   time.Time
	AccountID string
	Secret    string
	Token     string
	// CognitoIdentity is the Cognito identity ID the credentials were issued
	// for, if they came from an identity pool.
	CognitoIdentity string
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

var errNoIdentity = errors.New("missing request identity")

// Regional returns the request's metadata, failing when it names no account or Region.
func Regional(ctx context.Context) (Metadata, error) {
	m, ok := FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return Metadata{}, errNoIdentity
	}

	return m, nil
}

// Global returns the request's metadata, failing when it names no account. Account-wide
// services such as IAM use it.
func Global(ctx context.Context) (Metadata, error) {
	m, ok := FromContext(ctx)
	if !ok || m.AccountID == "" {
		return Metadata{}, errNoIdentity
	}

	return m, nil
}

// RegionalScope is Regional for callers that need only the account and Region.
func RegionalScope(ctx context.Context) (Scope, error) {
	m, err := Regional(ctx)
	return m.Scope, err
}

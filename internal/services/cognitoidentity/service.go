// Package cognitoidentity implements Amazon Cognito identity pools without
// HTTP dependencies. Pools and their identities belong to an account and
// region:
//
//	<scope>/cognitoidentity/pool/<pool ID>                       (with its roles and token secret)
//	<scope>/cognitoidentity/identity/<pool ID>/<identity ID>
//	<scope>/cognitoidentity/login/<pool ID>/<provider>/<subject> (the identity a login links to)
//	aws/cognitoidentity/pool/<pool ID>                           (the pool's account)
//	aws/cognitoidentity/identity/<identity ID>                   (the identity's account and pool)
//
// GetId, GetOpenIdToken, GetCredentialsForIdentity, and UnlinkIdentity are
// called unsigned, so they find a pool or identity from the partition-wide
// indexes. Logins are user pool ID tokens (cognito-idp.<region>.amazonaws.com/
// <pool ID>), tokens of the account's IAM OpenID Connect providers, and the
// pool's own developer tokens (cognito-identity.amazonaws.com). The pool's
// OpenID tokens are HS256 JWTs signed with its secret, and
// GetCredentialsForIdentity exchanges one for a role's credentials through
// STS's AssumeRoleWithWebIdentity, so the role's trust policy must trust
// cognito-identity.amazonaws.com.
package cognitoidentity

import (
	"fmt"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service       = "cognitoidentity"
	kindPool      = "pool"
	kindIdentity  = "identity"
	kindLogin     = "login"
	indexPrefix   = "aws/" + service + "/"
	roleAuth      = "authenticated"
	roleUnauth    = "unauthenticated"
	maxListResult = 60
)

var _ api.API = (*Service)(nil)

type Service struct {
	store    storage.Store
	pools    *cognitoidp.Service
	roles    *iam.Service
	verifier *proxy.Verifier
	// caller reaches STS, which issues identities' credentials.
	caller *catalog.Caller
	now    func() time.Time
}

// New returns the service, which verifies user pool tokens with pools and
// OpenID Connect tokens against the IAM providers roles holds.
func New(store storage.Store, pools *cognitoidp.Service, roles *iam.Service) *Service {
	return &Service{store: store, pools: pools, roles: roles, verifier: proxy.NewVerifier(), now: time.Now}
}

// UseCaller lets GetCredentialsForIdentity call STS. Call it before serving
// requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func notAuthorized(format string, args ...any) error {
	return awserror.New("NotAuthorizedException", fmt.Sprintf(format, args...))
}

// errRoles is Cognito's answer when a pool's roles cannot be assumed.
var errRoles = awserror.New("InvalidIdentityPoolConfigurationException",
	"Invalid identity pool configuration. Check assigned IAM roles for this pool.")

// poolIDPattern is what pool and identity IDs are: <region>:<UUID>.
var poolIDPattern = regexp.MustCompile(`^([a-z]{2}(-[a-z]+)+-\d):[0-9a-f-]{36}$`)

func poolKey(scope request.Scope, id string) string { return scope.Key(service, kindPool, id) }

func identityKey(scope request.Scope, pool, id string) string {
	return scope.Key(service, kindIdentity, pool+"/"+id)
}

func poolARN(scope request.Scope, id string) string {
	return "arn:aws:cognito-identity:" + scope.Region + ":" + scope.AccountID + ":identitypool/" + id
}

// list returns at most limit items (1 to 60) after the one token names.
func list[T any](items []T, id func(T) string, token *string, limit *int64) (
	selected []T, next *string, err error,
) {
	n := int(common.Value(limit))
	if n < 1 || n > maxListResult {
		return nil, nil, invalid("MaxResults must be between 1 and %d.", maxListResult)
	}

	return common.Page(items, id, token, n, invalid("Invalid NextToken."))
}

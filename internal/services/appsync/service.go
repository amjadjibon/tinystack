// Package appsync implements AWS AppSync GraphQL APIs without HTTP
// dependencies: the management API, and the APIs themselves, which the
// gateway's GraphQL endpoint runs through Execute. Resources belong to an
// account and region:
//
//	<scope>/appsync/api/<API ID>
//	<scope>/appsync/schema/<API ID>
//	<scope>/appsync/<kind>/<API ID>/<ID>   (apikey, datasource, function)
//	<scope>/appsync/resolver/<API ID>/<type>/<field>
//
// The schema record holds the SDL as StartSchemaCreation, CreateType,
// UpdateType, and DeleteType leave it; types are read from it. The
// partition-wide aws/appsync/api/<API ID> finds an API's account and region
// when it is called.
package appsync

import (
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service        = "appsync"
	kindAPI        = "api"
	kindSchema     = "schema"
	kindAPIKey     = "apikey"
	kindDataSource = "datasource"
	kindFunction   = "function"
	kindResolver   = "resolver"
	defaultLimit   = 25
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	functions *lambda.Service
	pools     *cognitoidp.Service
	verifier  *proxy.Verifier
	now       func() time.Time
}

// New returns the service. Resolvers and Lambda authorizers invoke functions,
// and Cognito user pool authorization verifies tokens with pools.
func New(store storage.Store, functions *lambda.Service, pools *cognitoidp.Service) *Service {
	return &Service{store: store, functions: functions, pools: pools, verifier: proxy.NewVerifier(), now: time.Now}
}

func badRequest(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("NotFoundException", fmt.Sprintf(format, args...))
}

// owned is the key of a record of kind that an API owns.
func owned(scope request.Scope, kind, apiID, id string) string {
	return scope.Key(service, kind, apiID+"/"+id)
}

// indexKey finds an API's account and region from its ID.
func indexKey(id string) string { return "aws/" + service + "/api/" + id }

type location struct {
	Account string
	Region  string
}

func apiARN(scope request.Scope, id string) string {
	return "arn:aws:appsync:" + scope.Region + ":" + scope.AccountID + ":apis/" + id
}

// host is the host an API is called at: a subdomain of the host the client
// used, <ID>.appsync-api.<host>, where an IP address becomes "localhost",
// whose subdomains resolve to the loopback address.
func host(base, id string) (scheme, h string) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "http", id + ".appsync-api.localhost"
	}

	name, port := u.Hostname(), u.Port()
	if net.ParseIP(name) != nil {
		name = "localhost"
	}

	h = id + ".appsync-api." + name
	if port != "" {
		h = net.JoinHostPort(h, port)
	}

	return u.Scheme, h
}

// page returns the items after the one token names, at most limit of them
// (1 to 25, 25 by default), and the token for the rest, if any.
func page[T any](items []T, id func(T) string, token *string, limit *int64) (selected []T, next *string, err error) {
	n := common.Value(limit)
	if n < 0 || n > defaultLimit {
		return nil, nil, badRequest("maxResults must be between 0 and %d.", defaultLimit)
	}

	if n == 0 {
		n = defaultLimit
	}

	return common.Page(items, id, token, int(n), badRequest("Invalid pagination token."))
}

// loadAPI reads the API id names, which every operation on its parts needs.
func loadAPI(tx storage.ReadTx, scope request.Scope, id *string) (*api.GraphqlApi, error) {
	a, ok, err := storage.Read[api.GraphqlApi](tx, scope.Key(service, kindAPI, common.Value(id)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("GraphQL API %s not found.", common.Value(id)))
	}

	return a, nil
}

// Package apigatewayv2 implements API Gateway HTTP APIs without HTTP
// dependencies: the management API, and the APIs themselves, which the
// gateway's execute-api endpoint runs through Execute. Resources belong to an
// account and region:
//
//	<scope>/apigatewayv2/api/<API ID>
//	<scope>/apigatewayv2/<kind>/<API ID>/<ID>   (route, integration, authorizer, deployment)
//	<scope>/apigatewayv2/stage/<API ID>/<stage name>
//
// A deployment is a snapshot of the API's routes, integrations, and
// authorizers; a stage serves its deployment. The partition-wide
// aws/apigatewayv2/api/<API ID> finds an API's account and region when it is
// called. Lambda proxy integrations and authorizers run through
// lambda.Service.Invoke, and access logs go to CloudWatch Logs through
// logs.Service.Append.
package apigatewayv2

import (
	"fmt"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service         = "apigatewayv2"
	kindAPI         = "api"
	kindRoute       = "route"
	kindIntegration = "integration"
	kindAuthorizer  = "authorizer"
	kindDeployment  = "deployment"
	kindStage       = "stage"
	defaultStage    = "$default"
	defaultRoute    = "$default"
	protocolHTTP    = "HTTP"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	functions *lambda.Service
	logs      *logs.Service
	verifier  *proxy.Verifier
	now       func() time.Time
	// authorizer checks Lambda proxy integrations against the function's resource policy;
	// see UseServiceAuthorizer.
	authorizer catalog.ServiceAuthorizer
}

func New(store storage.Store, functions *lambda.Service, logGroups *logs.Service) *Service {
	return &Service{
		store: store, functions: functions, logs: logGroups, verifier: proxy.NewVerifier(), now: time.Now,
	}
}

// UseServiceAuthorizer makes Lambda proxy integrations require the function's resource policy to allow
// apigateway.amazonaws.com. Call it before serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

func badRequest(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("NotFoundException", fmt.Sprintf(format, args...))
}

func conflict(format string, args ...any) error {
	return awserror.New("ConflictException", fmt.Sprintf(format, args...))
}

func apiKey(scope request.Scope, id string) string { return scope.Prefix(service, kindAPI) + id }

// owned is the key of a record of kind that an API owns.
func owned(scope request.Scope, kind, apiID, id string) string {
	return scope.Prefix(service, kind) + apiID + "/" + id
}

// indexKey finds an API's account and region from its ID.
func indexKey(id string) string { return "aws/" + service + "/api/" + id }

type location struct {
	Account string
	Region  string
}

// apiARN is the ARN API Gateway tags an API by.
func apiARN(scope request.Scope, id string) string {
	return "arn:aws:apigateway:" + scope.Region + "::/apis/" + id
}

// page returns the items after the one token names, at most limit of them
// (1 to 500, all by default), and the token for the rest, if any.
func page[T any](items []T, id func(T) string, token, limit *string) (selected []T, next *string, err error) {
	n := len(items)
	if limit != nil {
		parsed, parseErr := strconv.Atoi(*limit)
		if parseErr != nil || parsed < 1 || parsed > 500 {
			return nil, nil, badRequest("MaxResults must be between 1 and 500.")
		}

		n = parsed
	}

	return common.Page(items, id, token, n, badRequest("Invalid NextToken."))
}

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return badRequest("%s", err.Error())
	}

	return nil
}

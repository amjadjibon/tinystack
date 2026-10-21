// Package apigateway implements API Gateway REST APIs without HTTP
// dependencies: the management API, and the APIs themselves, which the
// gateway's execute-api endpoint runs through Execute. Resources belong to an
// account and region:
//
//	<scope>/apigateway/restapi/<API ID>   (the API with its resources, methods, integrations, and authorizers)
//	<scope>/apigateway/deployment/<API ID>/<deployment ID>
//	<scope>/apigateway/stage/<API ID>/<stage name>
//
// A deployment is a snapshot of the API's resource tree and authorizers; a
// stage serves its deployment. The partition-wide aws/apigateway/api/<API ID>
// finds an API's account and region when it is called. Lambda proxy
// integrations and authorizers run through lambda.Service.Invoke, Cognito
// authorizers check tokens through cognitoidp.Service.Verify, and access logs
// go to CloudWatch Logs through logs.Service.Append.
package apigateway

import (
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service        = "apigateway"
	kindAPI        = "restapi"
	kindDeployment = "deployment"
	kindStage      = "stage"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	functions *lambda.Service
	logs      *logs.Service
	pools     *cognitoidp.Service
	now       func() time.Time
	// authorizer checks Lambda proxy integrations against the function's resource policy;
	// see UseServiceAuthorizer.
	authorizer catalog.ServiceAuthorizer
	metrics    awsmetrics.Meter
}

func New(store storage.Store, functions *lambda.Service, logGroups *logs.Service, pools *cognitoidp.Service) *Service {
	return &Service{store: store, functions: functions, logs: logGroups, pools: pools, now: time.Now}
}

// UseServiceAuthorizer makes Lambda proxy integrations without a role require the function's resource policy to allow
// apigateway.amazonaws.com. Call it before serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// restAPI is a REST API's record: the API, its resource tree (each resource
// holds its methods, and each method its responses and integration), and its
// authorizers.
type restAPI struct {
	Resources   map[string]*api.Resource
	Authorizers map[string]*api.Authorizer
	API         api.RestApi
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

func owned(scope request.Scope, kind, apiID, id string) string {
	return scope.Prefix(service, kind) + apiID + "/" + id
}

// indexKey finds an API's account and region from its ID.
func indexKey(id string) string { return "aws/" + service + "/api/" + id }

type location struct {
	Account string
	Region  string
}

// apiARN is the ARN API Gateway tags a REST API by.
func apiARN(scope request.Scope, id string) string {
	return "arn:aws:apigateway:" + scope.Region + "::/restapis/" + id
}

// page returns the items after the one position names, at most limit of them
// (1 to 500, 25 by default), and the position of the rest, if any.
func page[T any](items []T, id func(T) string, position *string, limit *int64) (selected []T, next *string, err error) {
	n := 25
	if limit != nil {
		if *limit < 1 || *limit > 500 {
			return nil, nil, badRequest("limit must be between 1 and 500.")
		}

		n = int(*limit)
	}

	return common.Page(items, id, position, n, badRequest("Invalid position."))
}

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return badRequest("%s", err.Error())
	}

	return nil
}

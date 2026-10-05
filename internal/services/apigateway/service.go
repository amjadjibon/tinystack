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
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
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
}

func New(store storage.Store, functions *lambda.Service, logGroups *logs.Service, pools *cognitoidp.Service) *Service {
	return &Service{store: store, functions: functions, logs: logGroups, pools: pools, now: time.Now}
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

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func prefix(scope request.Scope, kind string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/"
}

func apiKey(scope request.Scope, id string) string { return prefix(scope, kindAPI) + id }

func owned(scope request.Scope, kind, apiID, id string) string {
	return prefix(scope, kind) + apiID + "/" + id
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

	start := 0
	if position != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*position)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, badRequest("Invalid position.")
		}

		start = len(items)
		for i, item := range items {
			if id(item) > string(after) {
				start = i
				break
			}
		}
	}

	items = items[start:]
	if len(items) <= n {
		return items, nil, nil
	}

	next = new(base64.RawURLEncoding.EncodeToString([]byte(id(items[n-1]))))
	return items[:n], next, nil
}

func checkTags(tags map[string]string) error {
	for k := range tags {
		if strings.HasPrefix(strings.ToLower(k), "aws:") {
			return badRequest("Tag keys cannot start with aws:")
		}
	}

	if err := tagging.Check(tags); err != nil {
		return badRequest("%s", err.Error())
	}

	return nil
}

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
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
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
}

func New(store storage.Store, functions *lambda.Service, logGroups *logs.Service) *Service {
	return &Service{
		store: store, functions: functions, logs: logGroups, verifier: proxy.NewVerifier(), now: time.Now,
	}
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

// owned is the key of a record of kind that an API owns.
func owned(scope request.Scope, kind, apiID, id string) string {
	return prefix(scope, kind) + apiID + "/" + id
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

	start := 0
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, badRequest("Invalid NextToken.")
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

package apigateway

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	authNone    = "NONE"
	authIAM     = "AWS_IAM"
	authCustom  = "CUSTOM"
	authCognito = "COGNITO_USER_POOLS"
	typeLambda  = "AWS_PROXY"
	typeHTTP    = "HTTP_PROXY"
	typeMock    = "MOCK"
	methodAny   = "ANY"
	maxTimeout  = 29000
	minTimeout  = 50
)

var methods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
	methodAny: true,
}

// method is a request's method: the resource, the method, and the API they
// belong to.
type method struct {
	api      *restAPI
	resource *api.Resource
	method   *api.Method
}

// loadMethod finds a resource's method.
func loadMethod(a *restAPI, resourceID, httpMethod *string) (*method, error) {
	r, err := loadResource(a, resourceID)
	if err != nil {
		return nil, err
	}

	m, ok := r.ResourceMethods[value(httpMethod)]
	if !ok {
		return nil, notFound("Invalid Method identifier specified")
	}

	return &method{api: a, resource: r, method: &m}, nil
}

// save stores the method back in its resource.
func (m *method) save() {
	m.resource.ResourceMethods[*m.method.HttpMethod] = *m.method
}

// checkMethod validates a method's authorization against the API's
// authorizers.
func checkMethod(a *restAPI, m *api.Method) error {
	switch t := value(m.AuthorizationType); t {
	case authNone, authIAM:
		if m.AuthorizerId != nil {
			return badRequest("AuthorizerId can only be set with CUSTOM or COGNITO_USER_POOLS authorization.")
		}
	case authCustom, authCognito:
		authorizer, ok := a.Authorizers[value(m.AuthorizerId)]
		if !ok || (value(authorizer.Type) == typeCognito) != (t == authCognito) {
			return badRequest("Invalid authorizer ID specified. Setting the authorization type to %s requires a "+
				"valid authorizer of the matching type.", t)
		}
	default:
		return badRequest("AuthorizationType must be NONE, AWS_IAM, CUSTOM, or COGNITO_USER_POOLS.")
	}

	if len(m.AuthorizationScopes) > 0 && value(m.AuthorizationType) != authCognito {
		return badRequest("AuthorizationScopes are only for COGNITO_USER_POOLS authorization.")
	}

	return nil
}

// changeMethod runs fn on a method of the API's resource and stores the API.
func (s *Service) changeMethod(
	ctx context.Context,
	apiID, resourceID, httpMethod *string,
	fn func(m *method) error,
) error {
	return s.changeAPI(ctx, apiID, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		m, err := loadMethod(a, resourceID, httpMethod)
		if err != nil {
			return err
		}

		if fnErr := fn(m); fnErr != nil {
			return fnErr
		}

		m.save()
		return nil
	})
}

// viewMethod runs fn on a method of the API's resource.
func (s *Service) viewMethod(
	ctx context.Context,
	apiID, resourceID, httpMethod *string,
	fn func(m *method) error,
) error {
	return s.viewAPI(ctx, apiID, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		m, err := loadMethod(a, resourceID, httpMethod)
		if err != nil {
			return err
		}

		return fn(m)
	})
}

func (s *Service) PutMethod(ctx context.Context, in *api.PutMethodRequest) (*api.Method, error) {
	if !methods[value(in.HttpMethod)] {
		return nil, badRequest("Invalid HttpMethod specified. Valid options are GET,PUT,POST,DELETE,PATCH,OPTIONS," +
			"HEAD,ANY")
	}

	out := &api.Method{
		HttpMethod: in.HttpMethod, AuthorizationType: in.AuthorizationType, AuthorizerId: in.AuthorizerId,
		ApiKeyRequired: new(value(in.ApiKeyRequired)), OperationName: in.OperationName,
		RequestParameters: in.RequestParameters, RequestModels: in.RequestModels,
		AuthorizationScopes: in.AuthorizationScopes,
	}
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		r, loadErr := loadResource(a, in.ResourceId)
		if loadErr != nil {
			return loadErr
		}

		if _, exists := r.ResourceMethods[*in.HttpMethod]; exists {
			return conflict("Method already exists for this resource")
		}

		if checkErr := checkMethod(a, out); checkErr != nil {
			return checkErr
		}

		if r.ResourceMethods == nil {
			r.ResourceMethods = map[string]api.Method{}
		}

		r.ResourceMethods[*in.HttpMethod] = *out
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetMethod(ctx context.Context, in *api.GetMethodRequest) (*api.Method, error) {
	var out *api.Method
	err := s.viewMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		out = m.method
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateMethod(ctx context.Context, in *api.UpdateMethodRequest) (*api.Method, error) {
	var out *api.Method
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		if err := patch(m.method, in.PatchOperations, "authorizationType", "authorizerId", "apiKeyRequired",
			"operationName", "requestParameters", "requestModels", "authorizationScopes"); err != nil {
			return err
		}

		// Dropping an authorizer drops what only it used.
		if t := value(m.method.AuthorizationType); t == authNone || t == authIAM {
			m.method.AuthorizerId = nil
		}

		if value(m.method.AuthorizationType) != authCognito {
			m.method.AuthorizationScopes = nil
		}

		out = m.method
		return checkMethod(m.api, m.method)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteMethod(ctx context.Context, in *api.DeleteMethodRequest) (*api.DeleteMethodOutput, error) {
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		m, err := loadMethod(a, in.ResourceId, in.HttpMethod)
		if err != nil {
			return err
		}

		delete(m.resource.ResourceMethods, *in.HttpMethod)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteMethodOutput{}, nil
}

var statusCode = regexp.MustCompile(`^[1-5]\d\d$`)

func (s *Service) PutMethodResponse(
	ctx context.Context,
	in *api.PutMethodResponseRequest,
) (*api.MethodResponse, error) {
	if !statusCode.MatchString(value(in.StatusCode)) {
		return nil, badRequest("Invalid status code specified: %s", value(in.StatusCode))
	}

	out := &api.MethodResponse{
		StatusCode: in.StatusCode, ResponseParameters: in.ResponseParameters, ResponseModels: in.ResponseModels,
	}
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		if _, exists := m.method.MethodResponses[*in.StatusCode]; exists {
			return conflict("Response already exists for this resource")
		}

		if m.method.MethodResponses == nil {
			m.method.MethodResponses = map[string]api.MethodResponse{}
		}

		m.method.MethodResponses[*in.StatusCode] = *out
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func methodResponse(m *method, code *string) (*api.MethodResponse, error) {
	r, ok := m.method.MethodResponses[value(code)]
	if !ok {
		return nil, notFound("Invalid Response status code specified")
	}

	return &r, nil
}

func (s *Service) GetMethodResponse(
	ctx context.Context,
	in *api.GetMethodResponseRequest,
) (*api.MethodResponse, error) {
	var out *api.MethodResponse
	err := s.viewMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		var err error
		out, err = methodResponse(m, in.StatusCode)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateMethodResponse(
	ctx context.Context,
	in *api.UpdateMethodResponseRequest,
) (*api.MethodResponse, error) {
	var out *api.MethodResponse
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		r, err := methodResponse(m, in.StatusCode)
		if err != nil {
			return err
		}

		if changeErr := patch(r, in.PatchOperations, "responseParameters", "responseModels"); changeErr != nil {
			return changeErr
		}

		m.method.MethodResponses[*in.StatusCode], out = *r, r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteMethodResponse(
	ctx context.Context,
	in *api.DeleteMethodResponseRequest,
) (*api.DeleteMethodResponseOutput, error) {
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		if _, err := methodResponse(m, in.StatusCode); err != nil {
			return err
		}

		delete(m.method.MethodResponses, *in.StatusCode)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteMethodResponseOutput{}, nil
}

// checkIntegration validates a Lambda proxy, HTTP proxy, or mock integration.
func checkIntegration(i *api.Integration) error {
	switch value(i.Type) {
	case typeLambda:
		if _, ok := proxy.LambdaARN(value(i.Uri)); !ok || !strings.HasPrefix(value(i.Uri), "arn:aws:apigateway:") {
			return badRequest("AWS ARN for integration must contain path or action: the URI of an AWS_PROXY " +
				"integration must be a Lambda function's invocation URI.")
		}

		if value(i.HttpMethod) != "POST" {
			return badRequest("Integrations of type 'AWS_PROXY' currently only support POST for integration " +
				"HTTP method.")
		}
	case typeHTTP:
		u, err := url.Parse(strings.NewReplacer("${", "", "}", "", "{", "", "}", "").Replace(value(i.Uri)))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return badRequest("Invalid HTTP endpoint specified for URI")
		}

		if !methods[value(i.HttpMethod)] {
			return badRequest("Enumeration value for HttpMethod must be non-empty")
		}
	case typeMock:
	default:
		return badRequest("TinyStack supports AWS_PROXY, HTTP_PROXY, and MOCK integrations for REST APIs.")
	}

	switch {
	case value(i.ConnectionType) != "INTERNET":
		return badRequest("TinyStack supports INTERNET connections only.")
	case value(i.TimeoutInMillis) < minTimeout || value(i.TimeoutInMillis) > maxTimeout:
		return badRequest("Timeout should be between %d ms and %d ms", minTimeout, maxTimeout)
	}

	return nil
}

func (s *Service) PutIntegration(ctx context.Context, in *api.PutIntegrationRequest) (*api.Integration, error) {
	var out *api.Integration
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		i := &api.Integration{
			Type: in.Type, HttpMethod: in.IntegrationHttpMethod, Uri: in.Uri, Credentials: in.Credentials,
			ConnectionType: orElse(in.ConnectionType, new("INTERNET")), RequestParameters: in.RequestParameters,
			RequestTemplates: in.RequestTemplates, ContentHandling: in.ContentHandling,
			PassthroughBehavior: orElse(in.PassthroughBehavior, new("WHEN_NO_MATCH")),
			TimeoutInMillis:     orElse(in.TimeoutInMillis, new(int64(maxTimeout))),
			CacheNamespace:      orElse(in.CacheNamespace, m.resource.Id),
			CacheKeyParameters:  in.CacheKeyParameters,
		}
		if i.CacheKeyParameters == nil {
			i.CacheKeyParameters = []string{}
		}

		if err := checkIntegration(i); err != nil {
			return err
		}

		// Responses outlive a replaced integration.
		if previous := m.method.MethodIntegration; previous != nil {
			i.IntegrationResponses = previous.IntegrationResponses
		}

		m.method.MethodIntegration, out = i, i
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func integration(m *method) (*api.Integration, error) {
	if m.method.MethodIntegration == nil {
		return nil, notFound("No integration defined for method")
	}

	return m.method.MethodIntegration, nil
}

func (s *Service) GetIntegration(ctx context.Context, in *api.GetIntegrationRequest) (*api.Integration, error) {
	var out *api.Integration
	err := s.viewMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		var err error
		out, err = integration(m)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateIntegration(ctx context.Context, in *api.UpdateIntegrationRequest) (*api.Integration, error) {
	var out *api.Integration
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		i, err := integration(m)
		if err != nil {
			return err
		}

		if changeErr := patch(i, in.PatchOperations, "uri", "httpMethod", "credentials", "requestParameters",
			"requestTemplates", "passthroughBehavior", "contentHandling", "timeoutInMillis", "cacheNamespace",
			"cacheKeyParameters", "connectionType"); changeErr != nil {
			return changeErr
		}

		out = i
		return checkIntegration(i)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteIntegration(
	ctx context.Context,
	in *api.DeleteIntegrationRequest,
) (*api.DeleteIntegrationOutput, error) {
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		if _, err := integration(m); err != nil {
			return err
		}

		m.method.MethodIntegration = nil
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteIntegrationOutput{}, nil
}

// checkIntegrationResponse requires the method response the integration
// response maps to, with every header it sets.
func checkIntegrationResponse(m *method, r *api.IntegrationResponse) error {
	declared, ok := m.method.MethodResponses[value(r.StatusCode)]
	if !ok {
		return notFound("Invalid Response status code specified")
	}

	for k := range r.ResponseParameters {
		if _, found := declared.ResponseParameters[k]; !found {
			return badRequest("Invalid mapping expression specified: Validation Result: warnings : [], errors : "+
				"[Invalid mapping expression parameter specified: %s]", k)
		}
	}

	if p := value(r.SelectionPattern); p != "" {
		if _, err := regexp.Compile(p); err != nil {
			return badRequest("Invalid selection pattern specified: %s", p)
		}
	}

	return nil
}

func (s *Service) PutIntegrationResponse(
	ctx context.Context,
	in *api.PutIntegrationResponseRequest,
) (*api.IntegrationResponse, error) {
	out := &api.IntegrationResponse{
		StatusCode: in.StatusCode, SelectionPattern: in.SelectionPattern, ResponseParameters: in.ResponseParameters,
		ResponseTemplates: in.ResponseTemplates, ContentHandling: in.ContentHandling,
	}
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		i, err := integration(m)
		if err != nil {
			return err
		}

		if changeErr := checkIntegrationResponse(m, out); changeErr != nil {
			return changeErr
		}

		if i.IntegrationResponses == nil {
			i.IntegrationResponses = map[string]api.IntegrationResponse{}
		}

		i.IntegrationResponses[*in.StatusCode] = *out
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func integrationResponse(m *method, code *string) (*api.Integration, *api.IntegrationResponse, error) {
	i, err := integration(m)
	if err != nil {
		return nil, nil, err
	}

	r, ok := i.IntegrationResponses[value(code)]
	if !ok {
		return nil, nil, notFound("Invalid Response status code specified")
	}

	return i, &r, nil
}

func (s *Service) GetIntegrationResponse(
	ctx context.Context,
	in *api.GetIntegrationResponseRequest,
) (*api.IntegrationResponse, error) {
	var out *api.IntegrationResponse
	err := s.viewMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		var err error
		_, out, err = integrationResponse(m, in.StatusCode)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateIntegrationResponse(
	ctx context.Context,
	in *api.UpdateIntegrationResponseRequest,
) (*api.IntegrationResponse, error) {
	var out *api.IntegrationResponse
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		i, r, err := integrationResponse(m, in.StatusCode)
		if err != nil {
			return err
		}

		if changeErr := patch(r, in.PatchOperations, "selectionPattern", "responseParameters", "responseTemplates",
			"contentHandling"); changeErr != nil {
			return changeErr
		}

		if changeErr := checkIntegrationResponse(m, r); changeErr != nil {
			return changeErr
		}

		i.IntegrationResponses[*in.StatusCode], out = *r, r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteIntegrationResponse(
	ctx context.Context,
	in *api.DeleteIntegrationResponseRequest,
) (*api.DeleteIntegrationResponseOutput, error) {
	err := s.changeMethod(ctx, in.RestApiId, in.ResourceId, in.HttpMethod, func(m *method) error {
		i, _, err := integrationResponse(m, in.StatusCode)
		if err != nil {
			return err
		}

		delete(i.IntegrationResponses, *in.StatusCode)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteIntegrationResponseOutput{}, nil
}

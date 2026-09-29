package apigateway

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	typeToken   = "TOKEN"
	typeRequest = "REQUEST"
	typeCognito = "COGNITO_USER_POOLS"
	defaultTTL  = 300
	maxTTL      = 3600
)

var (
	identitySource = regexp.MustCompile(
		`^method\.request\.(header|querystring)\.[^\s,]+$|^context\.[\w.]+$|^stageVariables\.\w+$`,
	)
	userPoolARN = regexp.MustCompile(`^arn:aws[a-z-]*:cognito-idp:[a-z0-9-]+:\d{12}:userpool/[\w-]+_[0-9a-zA-Z]+$`)
)

// sources splits an authorizer's comma-separated identity sources.
func sources(a *api.Authorizer) []string {
	var out []string
	for s := range strings.SplitSeq(value(a.IdentitySource), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}

	return out
}

// checkAuthorizer validates a Lambda TOKEN or REQUEST authorizer or a
// Cognito user pool authorizer.
func checkAuthorizer(a *api.Authorizer) error {
	if n := len(value(a.Name)); n < 1 || n > 128 {
		return badRequest("Name must be 1 to 128 characters.")
	}

	for _, s := range sources(a) {
		if !identitySource.MatchString(s) {
			return badRequest("Invalid identity source %s.", s)
		}
	}

	if ttl := value(a.AuthorizerResultTtlInSeconds); ttl < 0 || ttl > maxTTL {
		return badRequest("AuthorizerResultTtlInSeconds must be between 0 and %d.", maxTTL)
	}

	switch value(a.Type) {
	case typeToken, typeRequest:
		if _, ok := proxy.LambdaARN(value(a.AuthorizerUri)); !ok {
			return badRequest("Invalid Authorizer URI: it must be a Lambda function's invocation URI.")
		}

		if len(a.ProviderARNs) > 0 {
			return badRequest("ProviderARNs are only for COGNITO_USER_POOLS authorizers.")
		}

		token := value(a.Type) == typeToken
		if token && (len(sources(a)) != 1 || !strings.HasPrefix(value(a.IdentitySource), "method.request.header.")) {
			return badRequest("A TOKEN authorizer needs one identity source: method.request.header.<name>.")
		}

		if !token && len(sources(a)) == 0 && value(a.AuthorizerResultTtlInSeconds) > 0 {
			return badRequest("Caching a REQUEST authorizer's results needs an identity source.")
		}

		if p := value(a.IdentityValidationExpression); p != "" {
			if _, err := regexp.Compile(p); err != nil || !token {
				return badRequest("Invalid identity validation expression %s.", p)
			}
		}
	case typeCognito:
		if len(a.ProviderARNs) == 0 {
			return badRequest("A COGNITO_USER_POOLS authorizer needs the ARNs of its user pools.")
		}

		for _, arn := range a.ProviderARNs {
			if !userPoolARN.MatchString(arn) {
				return badRequest("Invalid provider ARN %s.", arn)
			}
		}

		if a.AuthorizerUri != nil {
			return badRequest("AuthorizerUri is only for TOKEN and REQUEST authorizers.")
		}
	default:
		return badRequest("Type must be TOKEN, REQUEST, or COGNITO_USER_POOLS.")
	}

	return nil
}

func (s *Service) CreateAuthorizer(ctx context.Context, in *api.CreateAuthorizerRequest) (*api.Authorizer, error) {
	a := &api.Authorizer{
		Id: new(newID(6)), Name: in.Name, Type: in.Type, ProviderARNs: in.ProviderARNs, AuthType: in.AuthType,
		AuthorizerUri: in.AuthorizerUri, AuthorizerCredentials: in.AuthorizerCredentials,
		IdentitySource: in.IdentitySource, IdentityValidationExpression: in.IdentityValidationExpression,
		AuthorizerResultTtlInSeconds: orElse(in.AuthorizerResultTtlInSeconds, new(int64(defaultTTL))),
	}
	if value(a.Type) == typeCognito && a.IdentitySource == nil {
		a.IdentitySource = new("method.request.header.Authorization")
	}

	if err := checkAuthorizer(a); err != nil {
		return nil, err
	}

	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, r *restAPI) error {
		r.Authorizers[*a.Id] = a
		return nil
	})
	if err != nil {
		return nil, err
	}

	return a, nil
}

func loadAuthorizer(r *restAPI, id *string) (*api.Authorizer, error) {
	a, ok := r.Authorizers[value(id)]
	if !ok {
		return nil, notFound("Invalid Authorizer identifier specified")
	}

	return a, nil
}

func (s *Service) GetAuthorizer(ctx context.Context, in *api.GetAuthorizerRequest) (*api.Authorizer, error) {
	var out *api.Authorizer
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, r *restAPI) error {
		var err error
		out, err = loadAuthorizer(r, in.AuthorizerId)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetAuthorizers(ctx context.Context, in *api.GetAuthorizersRequest) (*api.Authorizers, error) {
	out := &api.Authorizers{Items: []api.Authorizer{}}
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, r *restAPI) error {
		items := make([]api.Authorizer, 0, len(r.Authorizers))
		for _, id := range slices.Sorted(maps.Keys(r.Authorizers)) {
			items = append(items, *r.Authorizers[id])
		}

		var err error
		items, out.Position, err = page(items, func(a api.Authorizer) string { return *a.Id }, in.Position, in.Limit)
		out.Items = append(out.Items, items...)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateAuthorizer(ctx context.Context, in *api.UpdateAuthorizerRequest) (*api.Authorizer, error) {
	var out *api.Authorizer
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, r *restAPI) error {
		a, err := loadAuthorizer(r, in.AuthorizerId)
		if err != nil {
			return err
		}

		changed := clone(a)
		if changeErr := patch(changed, in.PatchOperations, "name", "type", "authorizerUri", "identitySource",
			"identityValidationExpression", "authorizerResultTtlInSeconds", "providerARNs", "authType",
			"authorizerCredentials"); changeErr != nil {
			return changeErr
		}

		if changeErr := checkAuthorizer(changed); changeErr != nil {
			return changeErr
		}

		// Methods using the authorizer must still match its type.
		for _, res := range r.Resources {
			for _, m := range res.ResourceMethods {
				if value(m.AuthorizerId) == *a.Id {
					if changeErr := checkMethod(
						&restAPI{Authorizers: map[string]*api.Authorizer{*a.Id: changed}},
						&m,
					); changeErr != nil {
						return changeErr
					}
				}
			}
		}

		r.Authorizers[*a.Id], out = changed, changed
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteAuthorizer(
	ctx context.Context,
	in *api.DeleteAuthorizerRequest,
) (*api.DeleteAuthorizerOutput, error) {
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, r *restAPI) error {
		a, err := loadAuthorizer(r, in.AuthorizerId)
		if err != nil {
			return err
		}

		for _, res := range r.Resources {
			for name, m := range res.ResourceMethods {
				if value(m.AuthorizerId) == *a.Id {
					return conflict("Cannot delete authorizer '%s', is referenced in method: %s:%s", value(a.Name),
						name, value(res.Path))
				}
			}
		}

		delete(r.Authorizers, *a.Id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAuthorizerOutput{}, nil
}

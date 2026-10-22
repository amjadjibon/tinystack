package apigatewayv2

import (
	"context"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	typeJWT     = "JWT"
	typeRequest = "REQUEST"
	maxTTL      = 3600
)

var identitySource = regexp.MustCompile(
	`^\$(request\.(header|querystring)\.[^\s,]+|context\.[\w.]+|stageVariables\.\w+)$`,
)

// checkAuthorizer validates a JWT authorizer (an issuer, audiences, and one
// token source) or a Lambda REQUEST authorizer.
func checkAuthorizer(a *api.Authorizer) error {
	if n := len(common.Value(a.Name)); n < 1 || n > 128 {
		return badRequest("Name must be 1 to 128 characters.")
	}

	for _, source := range a.IdentitySource {
		if !identitySource.MatchString(source) {
			return badRequest("Invalid identity source %s.", source)
		}
	}

	if ttl := common.Value(a.AuthorizerResultTtlInSeconds); ttl < 0 || ttl > maxTTL {
		return badRequest("AuthorizerResultTtlInSeconds must be between 0 and %d.", maxTTL)
	}

	switch common.Value(a.AuthorizerType) {
	case typeJWT:
		switch {
		case a.JwtConfiguration == nil || !isURL(common.Value(a.JwtConfiguration.Issuer)):
			return badRequest("A JWT authorizer needs a JwtConfiguration with an issuer URL.")
		case len(a.IdentitySource) != 1 || !strings.HasPrefix(a.IdentitySource[0], "$request."):
			return badRequest("A JWT authorizer needs one identity source: a request header or query string.")
		case a.AuthorizerUri != nil || a.AuthorizerPayloadFormatVersion != nil || a.EnableSimpleResponses != nil:
			return badRequest("AuthorizerUri, AuthorizerPayloadFormatVersion, and EnableSimpleResponses are " +
				"only for REQUEST authorizers.")
		}
	case typeRequest:
		switch format := common.Value(a.AuthorizerPayloadFormatVersion); {
		case !isFunction(common.Value(a.AuthorizerUri)):
			return badRequest("AuthorizerUri must be a Lambda function's invocation URI.")
		case format != payload1 && format != payload2:
			return badRequest("AuthorizerPayloadFormatVersion must be 1.0 or 2.0.")
		case common.Value(a.EnableSimpleResponses) && format != payload2:
			return badRequest("Simple responses need payload format 2.0.")
		case len(a.IdentitySource) == 0 && common.Value(a.AuthorizerResultTtlInSeconds) > 0:
			return badRequest("Caching an authorizer's results needs an identity source.")
		case a.JwtConfiguration != nil:
			return badRequest("JwtConfiguration is only for JWT authorizers.")
		}
	default:
		return badRequest("AuthorizerType must be JWT or REQUEST.")
	}

	return nil
}

func (s *Service) CreateAuthorizer(
	ctx context.Context,
	in *api.CreateAuthorizerRequest,
) (*api.CreateAuthorizerResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	a := &api.Authorizer{
		AuthorizerId: new(
			common.RandomID(6),
		),
		Name:                           in.Name,
		AuthorizerType:                 in.AuthorizerType,
		AuthorizerUri:                  in.AuthorizerUri,
		AuthorizerPayloadFormatVersion: in.AuthorizerPayloadFormatVersion,
		IdentitySource:                 in.IdentitySource,
		AuthorizerResultTtlInSeconds:   in.AuthorizerResultTtlInSeconds,
		EnableSimpleResponses:          in.EnableSimpleResponses,
		JwtConfiguration:               in.JwtConfiguration,
	}
	if checkErr := checkAuthorizer(a); checkErr != nil {
		return nil, checkErr
	}

	if common.Value(a.AuthorizerType) == typeRequest {
		a.EnableSimpleResponses = new(common.Value(a.EnableSimpleResponses))
		a.AuthorizerResultTtlInSeconds = new(common.Value(a.AuthorizerResultTtlInSeconds))
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		api, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if common.Value(api.ProtocolType) == protocolWebSocket {
			return badRequest("WebSocket authorizers are not supported.")
		}

		if writeErr := storage.Write(
			tx,
			owned(m.Scope, kindAuthorizer, *api.ApiId, *a.AuthorizerId),
			a,
		); writeErr != nil {
			return writeErr
		}

		return s.deploy(tx, m.Scope, *api.ApiId, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateAuthorizerResponse(*a)
	return &out, nil
}

// changeAuthorizer runs fn on an authorizer and stores it, or, when fn
// returns false, deletes it; then it deploys the change.
func (s *Service) changeAuthorizer(
	ctx context.Context,
	apiID, id *string,
	fn func(tx storage.WriteTx, scope request.Scope, a *api.Authorizer) (bool, error),
) (*api.Authorizer, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var a *api.Authorizer
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		owner, loadErr := loadAPI(tx, m.Scope, apiID)
		if loadErr != nil {
			return loadErr
		}

		key := owned(m.Scope, kindAuthorizer, *owner.ApiId, common.Value(id))
		var found bool
		if a, found, loadErr = storage.Read[api.Authorizer](tx, key); loadErr != nil || !found {
			return common.ErrorsOr(loadErr, notFound("Invalid Authorizer identifier specified %s", common.Value(id)))
		}

		keep, fnErr := fn(tx, m.Scope, a)
		if fnErr != nil {
			return fnErr
		}

		if keep {
			if writeErr := storage.Write(tx, key, a); writeErr != nil {
				return writeErr
			}
		} else {
			tx.Delete(key)
		}

		return s.deploy(tx, m.Scope, *owner.ApiId, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	return a, nil
}

func (s *Service) GetAuthorizer(
	ctx context.Context,
	in *api.GetAuthorizerRequest,
) (*api.GetAuthorizerResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var a *api.Authorizer
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		owner, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		var found bool
		a, found, loadErr = storage.Read[api.Authorizer](tx, owned(m.Scope, kindAuthorizer, *owner.ApiId,
			common.Value(in.AuthorizerId)))
		return common.ErrorsOr(loadErr, notFoundIf(!found, "Invalid Authorizer identifier specified %s",
			common.Value(in.AuthorizerId)))
	})
	if err != nil {
		return nil, err
	}

	out := api.GetAuthorizerResponse(*a)
	return &out, nil
}

func (s *Service) GetAuthorizers(
	ctx context.Context,
	in *api.GetAuthorizersRequest,
) (*api.GetAuthorizersResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetAuthorizersResponse{Items: []api.Authorizer{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		owner, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := storage.Records[api.Authorizer](tx, m.Prefix(service, kindAuthorizer)+*owner.ApiId+"/")
		if readErr != nil {
			return readErr
		}

		items := make([]api.Authorizer, 0, len(all))
		for _, a := range all {
			items = append(items, *a)
		}

		items, out.NextToken, readErr = page(items, func(a api.Authorizer) string { return *a.AuthorizerId },
			in.NextToken, in.MaxResults)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateAuthorizer(
	ctx context.Context,
	in *api.UpdateAuthorizerRequest,
) (*api.UpdateAuthorizerResponse, error) {
	a, err := s.changeAuthorizer(ctx, in.ApiId, in.AuthorizerId,
		func(_ storage.WriteTx, _ request.Scope, a *api.Authorizer) (bool, error) {
			a.Name, a.AuthorizerType = common.OrElse(
				in.Name,
				a.Name,
			), common.OrElse(
				in.AuthorizerType,
				a.AuthorizerType,
			)
			a.AuthorizerUri = common.OrElse(in.AuthorizerUri, a.AuthorizerUri)
			a.AuthorizerPayloadFormatVersion = common.OrElse(in.AuthorizerPayloadFormatVersion,
				a.AuthorizerPayloadFormatVersion)
			a.AuthorizerResultTtlInSeconds = common.OrElse(
				in.AuthorizerResultTtlInSeconds,
				a.AuthorizerResultTtlInSeconds,
			)
			a.EnableSimpleResponses = common.OrElse(in.EnableSimpleResponses, a.EnableSimpleResponses)
			a.JwtConfiguration = common.OrElse(in.JwtConfiguration, a.JwtConfiguration)
			if in.IdentitySource != nil {
				a.IdentitySource = in.IdentitySource
			}

			if common.Value(a.AuthorizerType) == typeJWT {
				a.EnableSimpleResponses, a.AuthorizerResultTtlInSeconds = nil, nil
			}

			return true, checkAuthorizer(a)
		})
	if err != nil {
		return nil, err
	}

	out := api.UpdateAuthorizerResponse(*a)
	return &out, nil
}

func (s *Service) DeleteAuthorizer(
	ctx context.Context,
	in *api.DeleteAuthorizerRequest,
) (*api.DeleteAuthorizerOutput, error) {
	_, err := s.changeAuthorizer(ctx, in.ApiId, in.AuthorizerId,
		func(tx storage.WriteTx, scope request.Scope, a *api.Authorizer) (bool, error) {
			used, usedErr := routesUsing(tx, scope, common.Value(in.ApiId), *a.AuthorizerId,
				func(r *api.Route) string { return common.Value(r.AuthorizerId) })
			if usedErr != nil {
				return false, usedErr
			}

			if len(used) > 0 {
				return false, conflict("Cannot delete Authorizer %s: route(s) %s use it", *a.AuthorizerId,
					strings.Join(used, ", "))
			}

			return false, nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAuthorizerOutput{}, nil
}

package apigatewayv2

import (
	"context"
	"net/url"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	typeLambda         = "AWS_PROXY"
	typeHTTP           = "HTTP_PROXY"
	payload1           = "1.0"
	payload2           = "2.0"
	methodAny          = "ANY"
	connectionInternet = "INTERNET"
	minTimeout         = 50
	maxTimeout         = 30000
)

func isFunction(uri string) bool {
	_, ok := proxy.LambdaARN(uri)
	return ok
}

// isURL reports whether uri is an HTTP(S) URL, allowing stage variable
// references such as ${stageVariables.host}.
func isURL(uri string) bool {
	u, err := url.Parse(strings.NewReplacer("${", "", "}", "").Replace(uri))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

var methods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
	methodAny: true,
}

// checkIntegration validates an integration of an HTTP API: a Lambda proxy
// (payload format 1.0 or 2.0) or an HTTP proxy (1.0) over the internet.
func checkIntegration(i *api.Integration) error {
	switch common.Value(i.IntegrationType) {
	case typeLambda:
		if !isFunction(common.Value(i.IntegrationUri)) {
			return badRequest("IntegrationUri of an AWS_PROXY integration must be a Lambda function ARN or its " +
				"invocation URI.")
		}

		if p := common.Value(i.PayloadFormatVersion); p != payload1 && p != payload2 {
			return badRequest("PayloadFormatVersion must be 1.0 or 2.0.")
		}
	case typeHTTP:
		if !isURL(common.Value(i.IntegrationUri)) {
			return badRequest("IntegrationUri of an HTTP_PROXY integration must be an HTTP URL.")
		}

		if !methods[common.Value(i.IntegrationMethod)] {
			return badRequest("IntegrationMethod of an HTTP_PROXY integration must be an HTTP method or ANY.")
		}

		if common.Value(i.PayloadFormatVersion) != payload1 {
			return badRequest("PayloadFormatVersion of an HTTP_PROXY integration must be 1.0.")
		}
	default:
		return badRequest("TinyStack supports AWS_PROXY and HTTP_PROXY integrations for HTTP APIs.")
	}

	if common.Value(i.ConnectionType) != connectionInternet {
		return badRequest("TinyStack supports INTERNET connections only.")
	}

	if t := common.Value(i.TimeoutInMillis); t < minTimeout || t > maxTimeout {
		return badRequest("TimeoutInMillis must be between %d and %d.", minTimeout, maxTimeout)
	}

	return nil
}

func (s *Service) CreateIntegration(
	ctx context.Context,
	in *api.CreateIntegrationRequest,
) (*api.CreateIntegrationResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	i := &api.Integration{
		IntegrationId: new(common.RandomID(7)), IntegrationType: in.IntegrationType, IntegrationUri: in.IntegrationUri,
		IntegrationMethod: in.IntegrationMethod, Description: in.Description,
		ConnectionType:       common.OrElse(in.ConnectionType, new(connectionInternet)),
		TimeoutInMillis:      common.OrElse(in.TimeoutInMillis, new(int64(maxTimeout))),
		PayloadFormatVersion: common.OrElse(in.PayloadFormatVersion, new(payload1)),
	}
	if common.Value(i.IntegrationType) == typeLambda && i.IntegrationMethod == nil {
		i.IntegrationMethod = new("POST")
	}

	if checkErr := checkIntegration(i); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if writeErr := storage.Write(
			tx,
			owned(m.Scope, kindIntegration, *a.ApiId, *i.IntegrationId),
			i,
		); writeErr != nil {
			return writeErr
		}

		return s.deploy(tx, m.Scope, *a.ApiId, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateIntegrationResult(*i)
	return &out, nil
}

// changeIntegration runs fn on an integration and stores it, or, when fn
// returns false, deletes it; then it deploys the change.
func (s *Service) changeIntegration(
	ctx context.Context,
	apiID, id *string,
	fn func(tx storage.WriteTx, scope request.Scope, i *api.Integration) (bool, error),
) (*api.Integration, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var i *api.Integration
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, apiID)
		if loadErr != nil {
			return loadErr
		}

		key := owned(m.Scope, kindIntegration, *a.ApiId, common.Value(id))
		var found bool
		if i, found, loadErr = storage.Read[api.Integration](tx, key); loadErr != nil || !found {
			return common.ErrorsOr(loadErr, notFound("Invalid Integration identifier specified %s", common.Value(id)))
		}

		keep, fnErr := fn(tx, m.Scope, i)
		if fnErr != nil {
			return fnErr
		}

		if keep {
			if writeErr := storage.Write(tx, key, i); writeErr != nil {
				return writeErr
			}
		} else {
			tx.Delete(key)
		}

		return s.deploy(tx, m.Scope, *a.ApiId, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	return i, nil
}

func (s *Service) GetIntegration(
	ctx context.Context,
	in *api.GetIntegrationRequest,
) (*api.GetIntegrationResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var i *api.Integration
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		var found bool
		i, found, loadErr = storage.Read[api.Integration](tx, owned(m.Scope, kindIntegration, *a.ApiId,
			common.Value(in.IntegrationId)))
		return common.ErrorsOr(loadErr, notFoundIf(!found, "Invalid Integration identifier specified %s",
			common.Value(in.IntegrationId)))
	})
	if err != nil {
		return nil, err
	}

	out := api.GetIntegrationResult(*i)
	return &out, nil
}

func (s *Service) GetIntegrations(
	ctx context.Context,
	in *api.GetIntegrationsRequest,
) (*api.GetIntegrationsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetIntegrationsResponse{Items: []api.Integration{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := storage.Records[api.Integration](tx, prefix(m.Scope, kindIntegration)+*a.ApiId+"/")
		if readErr != nil {
			return readErr
		}

		items := make([]api.Integration, 0, len(all))
		for _, i := range all {
			items = append(items, *i)
		}

		items, out.NextToken, readErr = page(items, func(i api.Integration) string { return *i.IntegrationId },
			in.NextToken, in.MaxResults)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateIntegration(
	ctx context.Context,
	in *api.UpdateIntegrationRequest,
) (*api.UpdateIntegrationResult, error) {
	i, err := s.changeIntegration(ctx, in.ApiId, in.IntegrationId,
		func(_ storage.WriteTx, _ request.Scope, i *api.Integration) (bool, error) {
			i.IntegrationType = common.OrElse(in.IntegrationType, i.IntegrationType)
			i.IntegrationUri = common.OrElse(in.IntegrationUri, i.IntegrationUri)
			i.IntegrationMethod = common.OrElse(in.IntegrationMethod, i.IntegrationMethod)
			i.Description, i.ConnectionType = common.OrElse(
				in.Description,
				i.Description,
			), common.OrElse(
				in.ConnectionType,
				i.ConnectionType,
			)
			i.TimeoutInMillis = common.OrElse(in.TimeoutInMillis, i.TimeoutInMillis)
			i.PayloadFormatVersion = common.OrElse(in.PayloadFormatVersion, i.PayloadFormatVersion)
			return true, checkIntegration(i)
		})
	if err != nil {
		return nil, err
	}

	out := api.UpdateIntegrationResult(*i)
	return &out, nil
}

func (s *Service) DeleteIntegration(
	ctx context.Context,
	in *api.DeleteIntegrationRequest,
) (*api.DeleteIntegrationOutput, error) {
	_, err := s.changeIntegration(ctx, in.ApiId, in.IntegrationId,
		func(tx storage.WriteTx, scope request.Scope, i *api.Integration) (bool, error) {
			used, usedErr := routesUsing(tx, scope, common.Value(in.ApiId), *i.IntegrationId,
				func(r *api.Route) string { return integrationID(common.Value(r.Target)) })
			if usedErr != nil {
				return false, usedErr
			}

			if len(used) > 0 {
				return false, conflict("Cannot delete Integration %s: it is the target of route(s) %s",
					*i.IntegrationId, strings.Join(used, ", "))
			}

			return false, nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteIntegrationOutput{}, nil
}

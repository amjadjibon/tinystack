package apigatewayv2

import (
	"context"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	routeSelection = "$request.method $request.path"
	ipv4           = "ipv4"
)

// endpoint is the base URL an API is called at: a subdomain of the host the
// client used, <ID>.execute-api.<host>, where an IP address becomes
// "localhost", whose subdomains resolve to the loopback address.
func endpoint(base, id string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "http://" + id + ".execute-api.localhost"
	}

	host, port := u.Hostname(), u.Port()
	if net.ParseIP(host) != nil {
		host = "localhost"
	}

	if port != "" {
		host = net.JoinHostPort(host, port)
	}

	return u.Scheme + "://" + id + ".execute-api." + host
}

func checkCors(c *api.Cors) error {
	if c == nil {
		return nil
	}

	if c.MaxAge != nil && (*c.MaxAge < -1 || *c.MaxAge > 86400) {
		return badRequest("MaxAge must be between -1 and 86400.")
	}

	if common.Value(c.AllowCredentials) && slices.Contains(c.AllowOrigins, "*") {
		return badRequest("allow-credentials is not supported when allow-origins is *")
	}

	return nil
}

var apiName = regexp.MustCompile(`^.{1,128}$`)

// checkAPI validates the settings CreateApi and UpdateApi share.
func checkAPI(name, selection, ipType *string, cors *api.Cors) error {
	switch {
	case name != nil && !apiName.MatchString(*name):
		return badRequest("Name must be 1 to 128 characters.")
	case selection != nil && *selection != routeSelection:
		return badRequest("RouteSelectionExpression of an HTTP API must be %s.", routeSelection)
	case ipType != nil && *ipType != ipv4 && *ipType != "dualstack":
		return badRequest("IpAddressType must be ipv4 or dualstack.")
	}

	return checkCors(cors)
}

func loadAPI(tx storage.ReadTx, scope request.Scope, id *string) (*api.Api, error) {
	a, ok, err := storage.Read[api.Api](tx, apiKey(scope, common.Value(id)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(
			err,
			notFound("Invalid API identifier specified %s:%s", scope.AccountID, common.Value(id)),
		)
	}

	return a, nil
}

func (s *Service) CreateApi(ctx context.Context, in *api.CreateApiRequest) (*api.CreateApiResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case common.Value(in.Name) == "":
		return nil, badRequest("Name is required.")
	case common.Value(in.ProtocolType) != protocolHTTP && common.Value(in.ProtocolType) != protocolWebSocket:
		return nil, badRequest("ProtocolType must be HTTP or WEBSOCKET.")
	case in.RouteKey != nil && in.Target == nil:
		return nil, badRequest("RouteKey can only be set with Target.")
	}

	checkErr := checkProtocolAPI(common.Value(in.ProtocolType), in.Name, in.RouteSelectionExpression,
		in.IpAddressType, in.CorsConfiguration, in.Target, in.RouteKey)
	if checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	now := s.now().UTC()
	id := common.RandomID(10)
	a := &api.Api{
		ApiId: new(id), Name: in.Name, ProtocolType: in.ProtocolType, Description: in.Description,
		ApiEndpoint:              new(endpoint(m.Endpoint, id)),
		RouteSelectionExpression: new(routeSelection), CorsConfiguration: in.CorsConfiguration,
		DisableExecuteApiEndpoint: new(common.Value(in.DisableExecuteApiEndpoint)), Version: in.Version,
		IpAddressType: new(ipv4), CreatedDate: &now, Tags: common.CloneTags(in.Tags),
	}
	if common.Value(in.ProtocolType) == protocolWebSocket {
		a.RouteSelectionExpression = in.RouteSelectionExpression
		a.ApiEndpoint = new(strings.Replace(*a.ApiEndpoint, "http", "ws", 1))
	}

	if in.IpAddressType != nil {
		a.IpAddressType = in.IpAddressType
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, apiKey(m.Scope, id), a); writeErr != nil {
			return writeErr
		}

		if writeErr := storage.Write(
			tx,
			indexKey(id),
			location{Account: m.AccountID, Region: m.Region},
		); writeErr != nil {
			return writeErr
		}

		if in.Target == nil {
			return nil
		}

		return s.quickCreate(tx, m.Scope, a, common.Value(in.Target), in.RouteKey)
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateApiResponse(*a)
	return &out, nil
}

// quickCreate adds what a quick-created API has: an integration with the
// target, a route to it (by default $default), and an automatically deployed
// $default stage.
func (s *Service) quickCreate(tx storage.WriteTx, scope request.Scope, a *api.Api, target string, key *string) error {
	integration, err := targetIntegration(target)
	if err != nil {
		return err
	}

	integration.IntegrationId, integration.ApiGatewayManaged = new(common.RandomID(7)), new(true)
	if writeErr := storage.Write(tx, owned(scope, kindIntegration, *a.ApiId, *integration.IntegrationId),
		integration); writeErr != nil {
		return writeErr
	}

	routeKey := defaultRoute
	if key != nil {
		routeKey = *key
	}

	if checkErr := checkRouteKey(routeKey); checkErr != nil {
		return checkErr
	}

	r := newRoute(routeKey)
	r.ApiGatewayManaged, r.Target = new(true), new("integrations/"+*integration.IntegrationId)
	if writeErr := storage.Write(tx, owned(scope, kindRoute, *a.ApiId, *r.RouteId), r); writeErr != nil {
		return writeErr
	}

	now := s.now().UTC()
	st := &api.Stage{
		StageName: new(defaultStage), AutoDeploy: new(true), ApiGatewayManaged: new(true), CreatedDate: &now,
		LastUpdatedDate: &now, DefaultRouteSettings: &api.RouteSettings{DetailedMetricsEnabled: new(false)},
		RouteSettings: map[string]api.RouteSettings{}, StageVariables: map[string]string{}, Tags: map[string]string{},
	}
	if writeErr := storage.Write(tx, owned(scope, kindStage, *a.ApiId, defaultStage), st); writeErr != nil {
		return writeErr
	}

	return s.deploy(tx, scope, *a.ApiId, now)
}

// targetIntegration is a quick-created API's integration: a Lambda proxy
// integration for a function ARN, or an HTTP proxy for a URL.
func targetIntegration(target string) (*api.Integration, error) {
	integration := &api.Integration{
		ConnectionType: new(connectionInternet), TimeoutInMillis: new(int64(maxTimeout)),
		IntegrationUri: new(target),
	}
	switch {
	case isFunction(target):
		integration.IntegrationType, integration.PayloadFormatVersion = new(typeLambda), new(payload2)
		integration.IntegrationMethod = new("POST")
	case isURL(target):
		integration.IntegrationType, integration.PayloadFormatVersion = new(typeHTTP), new(payload1)
		integration.IntegrationMethod = new(methodAny)
	default:
		return nil, badRequest("Target must be a Lambda function ARN or an HTTP URL.")
	}

	return integration, nil
}

func (s *Service) GetApi(ctx context.Context, in *api.GetApiRequest) (*api.GetApiResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var a *api.Api
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		a, loadErr = loadAPI(tx, m.Scope, in.ApiId)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := api.GetApiResponse(*a)
	return &out, nil
}

func (s *Service) GetApis(ctx context.Context, in *api.GetApisRequest) (*api.GetApisResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetApisResponse{Items: []api.Api{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := storage.Records[api.Api](tx, m.Prefix(service, kindAPI))
		if readErr != nil {
			return readErr
		}

		items := make([]api.Api, 0, len(all))
		for _, a := range all {
			items = append(items, *a)
		}

		items, out.NextToken, readErr = page(items, func(a api.Api) string { return *a.ApiId }, in.NextToken,
			in.MaxResults)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateApi(ctx context.Context, in *api.UpdateApiRequest) (*api.UpdateApiResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var a *api.Api
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if a, loadErr = loadAPI(tx, m.Scope, in.ApiId); loadErr != nil {
			return loadErr
		}

		if checkErr := checkProtocolAPI(common.Value(a.ProtocolType), in.Name,
			common.OrElse(in.RouteSelectionExpression, a.RouteSelectionExpression),
			in.IpAddressType, in.CorsConfiguration, in.Target, in.RouteKey); checkErr != nil {
			return checkErr
		}

		a.RouteSelectionExpression = common.OrElse(in.RouteSelectionExpression, a.RouteSelectionExpression)
		a.Name, a.Description = common.OrElse(in.Name, a.Name), common.OrElse(in.Description, a.Description)
		a.CorsConfiguration = common.OrElse(in.CorsConfiguration, a.CorsConfiguration)
		a.DisableExecuteApiEndpoint = common.OrElse(in.DisableExecuteApiEndpoint, a.DisableExecuteApiEndpoint)
		a.Version, a.IpAddressType = common.OrElse(
			in.Version,
			a.Version,
		), common.OrElse(
			in.IpAddressType,
			a.IpAddressType,
		)
		if in.Target != nil || in.RouteKey != nil {
			if quickErr := s.quickUpdate(tx, m.Scope, *a.ApiId, in.Target, in.RouteKey); quickErr != nil {
				return quickErr
			}
		}

		return storage.Write(tx, apiKey(m.Scope, *a.ApiId), a)
	})
	if err != nil {
		return nil, err
	}

	out := api.UpdateApiResponse(*a)
	return &out, nil
}

// quickUpdate changes the target and route key of a quick-created API's
// managed integration and route.
func (s *Service) quickUpdate(tx storage.WriteTx, scope request.Scope, apiID string, target, key *string) error {
	routes, err := storage.Records[api.Route](tx, scope.Prefix(service, kindRoute)+apiID+"/")
	if err != nil {
		return err
	}

	i := slices.IndexFunc(routes, func(r *api.Route) bool { return common.Value(r.ApiGatewayManaged) })
	if i < 0 {
		return badRequest("Target and RouteKey can only be updated on an API that was created with a Target.")
	}

	r := routes[i]
	if key != nil {
		if checkErr := checkRouteKey(*key); checkErr != nil {
			return checkErr
		}

		r.RouteKey = key
	}

	if target != nil {
		integration, targetErr := targetIntegration(*target)
		if targetErr != nil {
			return targetErr
		}

		integration.ApiGatewayManaged = new(true)
		integration.IntegrationId = new(integrationID(common.Value(r.Target)))
		if writeErr := storage.Write(tx, owned(scope, kindIntegration, apiID, *integration.IntegrationId),
			integration); writeErr != nil {
			return writeErr
		}
	}

	if writeErr := storage.Write(tx, owned(scope, kindRoute, apiID, *r.RouteId), r); writeErr != nil {
		return writeErr
	}

	return s.deploy(tx, scope, apiID, s.now().UTC())
}

func (s *Service) DeleteApi(ctx context.Context, in *api.DeleteApiRequest) (*api.DeleteApiOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		for _, kind := range []string{
			kindRoute, kindIntegration, kindAuthorizer, kindDeployment, kindStage, kindRouteResponse,
		} {
			for _, k := range tx.Keys(m.Prefix(service, kind) + *a.ApiId + "/") {
				tx.Delete(k)
			}
		}

		tx.Delete(apiKey(m.Scope, *a.ApiId))
		tx.Delete(indexKey(*a.ApiId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteApiOutput{}, nil
}

func (s *Service) DeleteCorsConfiguration(
	ctx context.Context,
	in *api.DeleteCorsConfigurationRequest,
) (*api.DeleteCorsConfigurationOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if common.Value(a.ProtocolType) == protocolWebSocket {
			return badRequest("CORS is only supported for HTTP APIs.")
		}

		a.CorsConfiguration = nil
		return storage.Write(tx, apiKey(m.Scope, *a.ApiId), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCorsConfigurationOutput{}, nil
}

package apigatewayv2

import (
	"context"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	authNone   = "NONE"
	authJWT    = "JWT"
	authIAM    = "AWS_IAM"
	authCustom = "CUSTOM"
)

var routeKeyPattern = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|ANY) (/[^\s?#]*)$`)

// checkRouteKey accepts $default and "<method> <path>", whose path segments
// are literals, {name} parameters, or a final greedy {name+}.
func checkRouteKey(key string) error {
	if key == defaultRoute {
		return nil
	}

	match := routeKeyPattern.FindStringSubmatch(key)
	if match == nil {
		return badRequest("Invalid route key %s: use $default or a method and a path, such as GET /pets/{id}.", key)
	}

	segments := strings.Split(strings.TrimPrefix(match[2], "/"), "/")
	for i, seg := range segments {
		inner, isParam := strings.CutPrefix(seg, "{")
		if !isParam {
			if strings.ContainsAny(seg, "{}") || (seg == "" && len(segments) > 1) {
				return badRequest("Invalid route key %s.", key)
			}

			continue
		}

		inner, closed := strings.CutSuffix(inner, "}")
		greedy := strings.HasSuffix(inner, "+")
		if !closed || strings.TrimSuffix(inner, "+") == "" || (greedy && i != len(segments)-1) {
			return badRequest("Invalid route key %s.", key)
		}
	}

	return nil
}

func newRoute(key string) *api.Route {
	return &api.Route{
		RouteId: new(newID(7)), RouteKey: new(key), ApiKeyRequired: new(false), AuthorizationType: new(authNone),
	}
}

// integrationID returns the integration a route target names:
// "integrations/<ID>".
func integrationID(target string) string {
	id, _ := strings.CutPrefix(target, "integrations/")
	return id
}

// checkRoute validates a route's key, target, and authorization against the
// API's integrations and authorizers.
func checkRoute(tx storage.ReadTx, scope request.Scope, apiID string, r *api.Route) error {
	if checkErr := checkRouteKey(value(r.RouteKey)); checkErr != nil {
		return checkErr
	}

	if value(r.ApiKeyRequired) {
		return badRequest("API keys are not supported by HTTP APIs.")
	}

	if r.Target != nil && *r.Target != "" {
		id, ok := strings.CutPrefix(*r.Target, "integrations/")
		if _, found := tx.Get(owned(scope, kindIntegration, apiID, id)); !ok || !found {
			return badRequest("Invalid target %s: it must be integrations/<ID> of this API.", *r.Target)
		}
	}

	switch value(r.AuthorizationType) {
	case authNone, authIAM:
		if value(r.AuthorizerId) != "" {
			return badRequest("AuthorizerId can only be set with JWT or CUSTOM authorization.")
		}
	case authJWT, authCustom:
		a, found, err := read[api.Authorizer](tx, owned(scope, kindAuthorizer, apiID, value(r.AuthorizerId)))
		if err != nil {
			return err
		}

		want := typeJWT
		if *r.AuthorizationType == authCustom {
			want = typeRequest
		}

		if !found || value(a.AuthorizerType) != want {
			return badRequest("%s authorization needs a %s authorizer of this API.", *r.AuthorizationType, want)
		}
	default:
		return badRequest("AuthorizationType must be NONE, JWT, AWS_IAM, or CUSTOM.")
	}

	if len(r.AuthorizationScopes) > 0 && value(r.AuthorizationType) != authJWT {
		return badRequest("AuthorizationScopes can only be set with JWT authorization.")
	}

	routes, err := records[api.Route](tx, prefix(scope, kindRoute)+apiID+"/")
	if err != nil {
		return err
	}

	for _, other := range routes {
		if *other.RouteKey == *r.RouteKey && *other.RouteId != *r.RouteId {
			return conflict("Route with key %s already exists for this API", *r.RouteKey)
		}
	}

	return nil
}

func (s *Service) CreateRoute(ctx context.Context, in *api.CreateRouteRequest) (*api.CreateRouteResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	r := newRoute(value(in.RouteKey))
	r.ApiKeyRequired = orElse(in.ApiKeyRequired, r.ApiKeyRequired)
	r.AuthorizationType = orElse(in.AuthorizationType, r.AuthorizationType)
	r.AuthorizationScopes, r.AuthorizerId, r.Target = in.AuthorizationScopes, in.AuthorizerId, in.Target
	r.OperationName = in.OperationName
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		if checkErr := checkRoute(tx, m.Scope, *a.ApiId, r); checkErr != nil {
			return checkErr
		}

		if writeErr := write(tx, owned(m.Scope, kindRoute, *a.ApiId, *r.RouteId), r); writeErr != nil {
			return writeErr
		}

		return s.deploy(tx, m.Scope, *a.ApiId, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateRouteResult(*r)
	return &out, nil
}

// changeRoute runs fn on a route and stores it, or, when fn returns false,
// deletes it; then it deploys the change to automatically deployed stages.
func (s *Service) changeRoute(
	ctx context.Context,
	apiID, routeID *string,
	fn func(tx storage.WriteTx, scope request.Scope, r *api.Route) (bool, error),
) (*api.Route, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var r *api.Route
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAPI(tx, m.Scope, apiID)
		if loadErr != nil {
			return loadErr
		}

		key := owned(m.Scope, kindRoute, *a.ApiId, value(routeID))
		var found bool
		if r, found, loadErr = read[api.Route](tx, key); loadErr != nil || !found {
			return errorsOr(loadErr, notFound("Invalid Route identifier specified %s", value(routeID)))
		}

		keep, fnErr := fn(tx, m.Scope, r)
		if fnErr != nil {
			return fnErr
		}

		if keep {
			if writeErr := write(tx, key, r); writeErr != nil {
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

	return r, nil
}

func (s *Service) GetRoute(ctx context.Context, in *api.GetRouteRequest) (*api.GetRouteResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var r *api.Route
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		var found bool
		r, found, loadErr = read[api.Route](tx, owned(m.Scope, kindRoute, *a.ApiId, value(in.RouteId)))
		return errorsOr(loadErr, notFoundIf(!found, "Invalid Route identifier specified %s", value(in.RouteId)))
	})
	if err != nil {
		return nil, err
	}

	out := api.GetRouteResult(*r)
	return &out, nil
}

// notFoundIf is a NotFoundException when missing is true and otherwise nil.
func notFoundIf(missing bool, format string, args ...any) error {
	if missing {
		return notFound(format, args...)
	}

	return nil
}

func (s *Service) GetRoutes(ctx context.Context, in *api.GetRoutesRequest) (*api.GetRoutesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetRoutesResponse{Items: []api.Route{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAPI(tx, m.Scope, in.ApiId)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := records[api.Route](tx, prefix(m.Scope, kindRoute)+*a.ApiId+"/")
		if readErr != nil {
			return readErr
		}

		items := make([]api.Route, 0, len(all))
		for _, r := range all {
			items = append(items, *r)
		}

		items, out.NextToken, readErr = page(items, func(r api.Route) string { return *r.RouteId }, in.NextToken,
			in.MaxResults)
		out.Items = append(out.Items, items...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateRoute(ctx context.Context, in *api.UpdateRouteRequest) (*api.UpdateRouteResult, error) {
	r, err := s.changeRoute(ctx, in.ApiId, in.RouteId,
		func(tx storage.WriteTx, scope request.Scope, r *api.Route) (bool, error) {
			r.RouteKey, r.ApiKeyRequired = orElse(in.RouteKey, r.RouteKey), orElse(in.ApiKeyRequired, r.ApiKeyRequired)
			r.AuthorizationType = orElse(in.AuthorizationType, r.AuthorizationType)
			r.AuthorizerId, r.Target = orElse(in.AuthorizerId, r.AuthorizerId), orElse(in.Target, r.Target)
			r.OperationName = orElse(in.OperationName, r.OperationName)
			if in.AuthorizationScopes != nil {
				r.AuthorizationScopes = in.AuthorizationScopes
			}

			// Clearing authorization drops what only it used.
			if value(r.AuthorizationType) == authNone || value(r.AuthorizationType) == authIAM {
				r.AuthorizerId = nil
			}

			if value(r.AuthorizationType) != authJWT {
				r.AuthorizationScopes = nil
			}

			if value(r.Target) == "" {
				r.Target = nil
			}

			return true, checkRoute(tx, scope, value(in.ApiId), r)
		})
	if err != nil {
		return nil, err
	}

	out := api.UpdateRouteResult(*r)
	return &out, nil
}

func (s *Service) DeleteRoute(ctx context.Context, in *api.DeleteRouteRequest) (*api.DeleteRouteOutput, error) {
	_, err := s.changeRoute(ctx, in.ApiId, in.RouteId, func(storage.WriteTx, request.Scope, *api.Route) (bool, error) {
		return false, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRouteOutput{}, nil
}

// routesUsing returns the keys of the routes whose field names id.
func routesUsing(
	tx storage.ReadTx,
	scope request.Scope,
	apiID, id string,
	field func(*api.Route) string,
) ([]string, error) {
	routes, err := records[api.Route](tx, prefix(scope, kindRoute)+apiID+"/")
	if err != nil {
		return nil, err
	}

	var keys []string
	for _, r := range routes {
		if field(r) == id {
			keys = append(keys, *r.RouteKey)
		}
	}

	slices.Sort(keys)
	return keys, nil
}

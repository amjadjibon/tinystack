package apigatewayv2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func responseRoute(tx storage.ReadTx, scope request.Scope, apiID, routeID *string) error {
	a, err := loadAPI(tx, scope, apiID)
	if err != nil {
		return err
	}

	if common.Value(a.ProtocolType) != protocolWebSocket {
		return badRequest("Route responses require a WebSocket API.")
	}

	if _, ok := tx.Get(owned(scope, kindRoute, common.Value(apiID), common.Value(routeID))); !ok {
		return notFound("Invalid Route identifier specified")
	}

	return nil
}

func (s *Service) CreateRouteResponse(
	ctx context.Context,
	in *api.CreateRouteResponseRequest,
) (*api.CreateRouteResponseResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.RouteResponseKey) != defaultRoute {
		return nil, badRequest("Only $default route responses are supported.")
	}

	r := &api.RouteResponse{RouteResponseId: new(common.RandomID(7)), RouteResponseKey: in.RouteResponseKey}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if routeErr := responseRoute(tx, m.Scope, in.ApiId, in.RouteId); routeErr != nil {
			return routeErr
		}

		prefix := owned(m.Scope, kindRouteResponse, *in.ApiId, *in.RouteId) + "/"
		if len(tx.Keys(prefix)) != 0 {
			return conflict("A $default route response already exists.")
		}

		if writeErr := storage.Write(tx, prefix+*r.RouteResponseId, r); writeErr != nil {
			return writeErr
		}

		return s.deploy(tx, m.Scope, *in.ApiId, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}

	out := api.CreateRouteResponseResponse(*r)
	return &out, nil
}

func (s *Service) GetRouteResponses(
	ctx context.Context,
	in *api.GetRouteResponsesRequest,
) (*api.GetRouteResponsesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetRouteResponsesResponse{Items: []api.RouteResponse{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if routeErr := responseRoute(tx, m.Scope, in.ApiId, in.RouteId); routeErr != nil {
			return routeErr
		}

		records, readErr := storage.Records[api.RouteResponse](
			tx,
			owned(m.Scope, kindRouteResponse, *in.ApiId, *in.RouteId)+"/",
		)
		if readErr != nil {
			return readErr
		}

		items := make([]api.RouteResponse, 0, len(records))
		for _, r := range records {
			items = append(items, *r)
		}

		out.Items, out.NextToken, err = page(
			items,
			func(r api.RouteResponse) string { return *r.RouteResponseId },
			in.NextToken,
			in.MaxResults,
		)
		return err
	})
	return out, err
}

func (s *Service) routeResponse(
	ctx context.Context,
	apiID, routeID, id *string,
	remove bool,
) (*api.RouteResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var r *api.RouteResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if routeErr := responseRoute(tx, m.Scope, apiID, routeID); routeErr != nil {
			return routeErr
		}

		key := owned(m.Scope, kindRouteResponse, *apiID, *routeID) + "/" + common.Value(id)
		var found bool
		var readErr error
		r, found, readErr = storage.Read[api.RouteResponse](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Invalid RouteResponse identifier specified")
		}

		if remove {
			tx.Delete(key)
			return s.deploy(tx, m.Scope, *apiID, s.now().UTC())
		}

		return nil
	})
	return r, err
}

func (s *Service) GetRouteResponse(
	ctx context.Context,
	in *api.GetRouteResponseRequest,
) (*api.GetRouteResponseResponse, error) {
	r, err := s.routeResponse(ctx, in.ApiId, in.RouteId, in.RouteResponseId, false)
	if err != nil {
		return nil, err
	}

	out := api.GetRouteResponseResponse(*r)
	return &out, nil
}

func (s *Service) UpdateRouteResponse(
	ctx context.Context,
	in *api.UpdateRouteResponseRequest,
) (*api.UpdateRouteResponseResponse, error) {
	if in.RouteResponseKey != nil && *in.RouteResponseKey != defaultRoute {
		return nil, badRequest("Only $default route responses are supported.")
	}

	r, err := s.routeResponse(ctx, in.ApiId, in.RouteId, in.RouteResponseId, false)
	if err != nil {
		return nil, err
	}

	out := api.UpdateRouteResponseResponse(*r)
	return &out, nil
}

func (s *Service) DeleteRouteResponse(
	ctx context.Context,
	in *api.DeleteRouteResponseRequest,
) (*api.DeleteRouteResponseOutput, error) {
	_, err := s.routeResponse(ctx, in.ApiId, in.RouteId, in.RouteResponseId, true)
	if err != nil {
		return nil, err
	}

	return &api.DeleteRouteResponseOutput{}, nil
}

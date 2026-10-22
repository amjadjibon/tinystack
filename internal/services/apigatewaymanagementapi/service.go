// Package apigatewaymanagementapi implements live WebSocket connection management.
package apigatewaymanagementapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewaymanagementapi"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigatewayv2"
)

type Service struct{ connections *apigatewayv2.Service }

type (
	targetKey struct{}
	target    struct{ apiID, stage string }
)

var _ api.API = (*Service)(nil)

func New(connections *apigatewayv2.Service) *Service { return &Service{connections: connections} }

func (s *Service) manage(
	ctx context.Context,
	id *string,
	method string,
	data []byte,
) (*apigatewayv2.ConnectionDetails, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	t, _ := ctx.Value(targetKey{}).(target)
	return s.connections.ManageConnection(ctx, m.Scope, t.apiID, t.stage, common.Value(id), method, data)
}

func (s *Service) PostToConnection(
	ctx context.Context,
	in *api.PostToConnectionRequest,
) (*api.PostToConnectionOutput, error) {
	_, err := s.manage(ctx, in.ConnectionId, http.MethodPost, in.Data)
	if err != nil {
		return nil, err
	}

	return &api.PostToConnectionOutput{}, nil
}

func (s *Service) GetConnection(ctx context.Context, in *api.GetConnectionRequest) (*api.GetConnectionResponse, error) {
	conn, err := s.manage(ctx, in.ConnectionId, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}

	return &api.GetConnectionResponse{
		ConnectedAt: new(conn.ConnectedAt), LastActiveAt: new(conn.LastActiveAt),
		Identity: &api.Identity{SourceIp: new(conn.SourceIP), UserAgent: new(conn.UserAgent)},
	}, nil
}

func (s *Service) DeleteConnection(
	ctx context.Context,
	in *api.DeleteConnectionRequest,
) (*api.DeleteConnectionOutput, error) {
	_, err := s.manage(ctx, in.ConnectionId, http.MethodDelete, nil)
	if err != nil {
		return nil, err
	}

	return &api.DeleteConnectionOutput{}, nil
}

// Execute recognizes management requests on an execute-api endpoint before API route execution.
func (s *Service) Execute(ctx context.Context, apiID string, r *execute.Request) (*execute.Response, bool, error) {
	stage, rest, _ := strings.Cut(strings.TrimPrefix(r.Path, "/"), "/")
	id, found := strings.CutPrefix(rest, "@connections/")
	if !found {
		return nil, false, nil
	}

	if r.Caller == nil {
		return execute.Message(http.StatusForbidden, "Forbidden"), true, nil
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: request.Scope{
		AccountID: r.Caller.AccountID, Region: r.Caller.Region,
	}, Service: "apigatewaymanagementapi"})
	ctx = context.WithValue(ctx, targetKey{}, target{apiID: apiID, stage: stage})
	action := map[string]string{http.MethodPost: "POST", http.MethodGet: "GET", http.MethodDelete: "DELETE"}[r.Method]
	if action == "" {
		return execute.Message(http.StatusBadRequest, "Unsupported connection management method"), true, nil
	}

	resource := catalog.AuthorizationResource{
		Action: "execute-api:ManageConnections",
		ARN: "arn:aws:execute-api:" + r.Caller.Region + ":" + r.Caller.AccountID + ":" + apiID + "/" + stage + "/" +
			action + "/@connections/" + id, OwnerAccount: r.Caller.AccountID,
	}

	if denied := r.AuthorizePolicy(ctx, &resource, execute.Signed); denied != nil {
		return denied, true, nil
	}

	var err error
	var out any
	status := http.StatusOK
	switch r.Method {
	case http.MethodPost:
		out, err = s.PostToConnection(ctx, &api.PostToConnectionRequest{ConnectionId: new(id), Data: r.Body})
	case http.MethodGet:
		var conn *api.GetConnectionResponse
		conn, err = s.GetConnection(ctx, &api.GetConnectionRequest{ConnectionId: new(id)})
		out = conn
	case http.MethodDelete:
		out, err = s.DeleteConnection(ctx, &api.DeleteConnectionRequest{ConnectionId: new(id)})
		status = http.StatusNoContent
	}

	if err != nil {
		e := awserror.From(err)
		response := execute.Message(e.StatusCode, e.Message)
		response.Header.Set("X-Amzn-Errortype", e.Code)
		return response, true, nil
	}

	body, err := json.Marshal(out)
	return &execute.Response{
		Status: status,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   body,
	}, true, err
}

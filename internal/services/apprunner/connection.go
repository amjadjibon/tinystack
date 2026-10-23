package apprunner

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateConnection(
	ctx context.Context,
	in *api.CreateConnectionRequest,
) (*api.CreateConnectionResponse, error) {
	if err := validateCreateConnectionRequest(in); err != nil {
		return nil, err
	}

	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{
		Kind: kindConnection,
		Name: common.Value(in.ConnectionName),
		Tags: tags,
		Connection: &api.Connection{
			ConnectionName: in.ConnectionName,
			ProviderType:   in.ProviderType,
			Status:         new("PENDING_HANDSHAKE"),
			CreatedAt:      common.SecondsMilli(time.Now()),
		},
	}

	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}

	return &api.CreateConnectionResponse{Connection: r.Connection}, nil
}

func (s *Service) DeleteConnection(
	ctx context.Context,
	in *api.DeleteConnectionRequest,
) (*api.DeleteConnectionResponse, error) {
	if err := validateDeleteConnectionRequest(in); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Connection
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, common.Value(in.ConnectionArn), kindConnection)
		if e != nil {
			return e
		}

		out = r.Connection
		k, e := recordKey(scope, r.ARN)
		if e != nil {
			return e
		}

		tx.Delete(k)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteConnectionResponse{Connection: out}, nil
}

func (s *Service) ListConnections(
	ctx context.Context,
	in *api.ListConnectionsRequest,
) (*api.ListConnectionsResponse, error) {
	if err := validateListConnectionsRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(ctx, kindConnection, in.ConnectionName, nil, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListConnectionsResponse{NextToken: next, ConnectionSummaryList: []api.ConnectionSummary{}}
	for _, r := range items {
		out.ConnectionSummaryList = append(
			out.ConnectionSummaryList,
			api.ConnectionSummary{
				ConnectionName: r.Connection.ConnectionName,
				ConnectionArn:  r.Connection.ConnectionArn,
				ProviderType:   r.Connection.ProviderType,
				Status:         r.Connection.Status,
				CreatedAt:      r.Connection.CreatedAt,
			},
		)
	}

	return out, nil
}

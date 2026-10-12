package route53resolver

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateResolverEndpoint(
	ctx context.Context,
	in *api.CreateResolverEndpointRequest,
) (*api.CreateResolverEndpointResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, original, e := tokenRecord(tx, scope, endpointKind, in.CreatorRequestId, in)
		if e != nil {
			return e
		}

		if existing != nil {
			r = existing
			return nil
		}

		prefix := "rslvr-in-"
		if common.Value(in.Direction) == outbound {
			prefix = "rslvr-out-"
		}

		id := prefix + common.RandomID(17, "0123456789abcdef")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		r = &record{
			ID: id, Kind: endpointKind, CreateRequest: original, Tags: tags, Ips: in.IpAddresses,
			Endpoint: &api.ResolverEndpoint{
				Id:               new(id),
				Arn:              new(arn(scope, endpointKind, id)),
				CreatorRequestId: in.CreatorRequestId,
				Name:             in.Name,
				Direction:        in.Direction,
				SecurityGroupIds: in.SecurityGroupIds,
				Status:           new("OPERATIONAL"),
				CreationTime:     new(now),
				ModificationTime: new(now),
			},
		}

		if e := s.checkEndpoint(tx, scope, r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateResolverEndpointResponse{ResolverEndpoint: r.Endpoint}, nil
}

func (s *Service) UpdateResolverEndpoint(
	ctx context.Context,
	in *api.UpdateResolverEndpointRequest,
) (*api.UpdateResolverEndpointResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, endpointKind, in.ResolverEndpointId)
		if e != nil {
			return e
		}

		if in.Name != nil {
			r.Endpoint.Name = in.Name
		}

		if e := s.checkEndpoint(tx, scope, r); e != nil {
			return e
		}

		r.Endpoint.ModificationTime = new(time.Now().UTC().Format(time.RFC3339Nano))
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateResolverEndpointResponse{ResolverEndpoint: r.Endpoint}, nil
}

func (s *Service) DeleteResolverEndpoint(
	ctx context.Context,
	in *api.DeleteResolverEndpointRequest,
) (*api.DeleteResolverEndpointResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, endpointKind, in.ResolverEndpointId)
		if e != nil {
			return e
		}

		rules, e := storage.Records[record](tx, scope.Prefix(serviceName, ruleKind))
		if e != nil {
			return e
		}

		for _, rule := range rules {
			if common.Value(rule.Rule.ResolverEndpointId) == r.ID {
				return awserror.New("InvalidRequestException", "Endpoint is used by a Resolver rule")
			}
		}

		r.Endpoint.Status = new("DELETING")
		tx.Delete(scope.Key(serviceName, endpointKind, r.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResolverEndpointResponse{ResolverEndpoint: r.Endpoint}, nil
}

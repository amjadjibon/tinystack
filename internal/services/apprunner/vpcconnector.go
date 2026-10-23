package apprunner

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateVpcConnector(
	ctx context.Context,
	in *api.CreateVpcConnectorRequest,
) (*api.CreateVpcConnectorResponse, error) {
	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{
		Kind: kindVpcConnector,
		Name: common.Value(in.VpcConnectorName),
		Tags: tags,
		VpcConnector: &api.VpcConnector{
			VpcConnectorName:     in.VpcConnectorName,
			Subnets:              in.Subnets,
			SecurityGroups:       in.SecurityGroups,
			VpcConnectorRevision: new(int64(1)),
			Status:               new(statusActive),
			CreatedAt:            common.SecondsMilli(time.Now()),
		},
	}

	if err := s.insert(ctx, r); err != nil {
		return nil, err
	}

	return &api.CreateVpcConnectorResponse{VpcConnector: r.VpcConnector}, nil
}

func (s *Service) DescribeVpcConnector(
	ctx context.Context,
	in *api.DescribeVpcConnectorRequest,
) (*api.DescribeVpcConnectorResponse, error) {
	r, err := s.get(ctx, in.VpcConnectorArn, kindVpcConnector)
	if err != nil {
		return nil, err
	}

	return &api.DescribeVpcConnectorResponse{VpcConnector: r.VpcConnector}, nil
}

func (s *Service) DeleteVpcConnector(
	ctx context.Context,
	in *api.DeleteVpcConnectorRequest,
) (*api.DeleteVpcConnectorResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.VpcConnector
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, common.Value(in.VpcConnectorArn), kindVpcConnector)
		if e != nil {
			return e
		}

		out = r.VpcConnector
		r.VpcConnector.Status = new(statusInactive)
		r.VpcConnector.DeletedAt = common.SecondsMilli(time.Now())
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVpcConnectorResponse{VpcConnector: out}, nil
}

func (s *Service) ListVpcConnectors(
	ctx context.Context,
	in *api.ListVpcConnectorsRequest,
) (*api.ListVpcConnectorsResponse, error) {
	items, next, err := s.list(ctx, kindVpcConnector, nil, nil, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListVpcConnectorsResponse{NextToken: next, VpcConnectors: []api.VpcConnector{}}
	for _, r := range items {
		out.VpcConnectors = append(out.VpcConnectors, *r.VpcConnector)
	}

	return out, nil
}

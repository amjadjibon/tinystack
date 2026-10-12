package ssoadmin

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
)

func (s *Service) ListInstances(ctx context.Context, in *api.ListInstancesRequest) (*api.ListInstancesResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.ListInstancesRequest{}
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	arn, id := identitystore.Instance(m.AccountID)
	list := []api.InstanceMetadata{
		{InstanceArn: new(arn), IdentityStoreId: new(id), OwnerAccountId: new(m.AccountID), Status: new("ACTIVE")},
	}

	page, next, err := common.Page(list, func(i api.InstanceMetadata) string { return *i.InstanceArn },
		in.NextToken, limit, awserror.Validation("Invalid NextToken"))
	return &api.ListInstancesResponse{Instances: page, NextToken: next}, err
}

func (s *Service) DescribeInstance(
	ctx context.Context,
	in *api.DescribeInstanceRequest,
) (*api.DescribeInstanceResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	arn, id := identitystore.Instance(scope.AccountID)
	return &api.DescribeInstanceResponse{
		InstanceArn: new(arn), IdentityStoreId: new(id),
		OwnerAccountId: new(scope.AccountID), Status: new("ACTIVE"),
	}, nil
}

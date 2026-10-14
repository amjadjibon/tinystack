package lakeformation

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) RegisterResource(
	ctx context.Context,
	in *api.RegisterResourceRequest,
) (*api.RegisterResourceResponse, error) {
	scope, err := scopeOf(ctx, nil)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.ResourceArn)
	if !s3ARN(arn) {
		return nil, invalid("ResourceArn must be an S3 ARN")
	}

	role := in.RoleArn
	if common.Value(in.UseServiceLinkedRole) {
		if role != nil {
			return nil, invalid("Specify RoleArn or UseServiceLinkedRole")
		}

		role = new(
			"arn:aws:iam::" + scope.AccountID + ":role/aws-service-role/lakeformation.amazonaws.com/" +
				"AWSServiceRoleForLakeFormationDataAccess",
		)
	} else if role == nil || !strings.Contains(*role, ":role/") {
		return nil, invalid("RoleArn or UseServiceLinkedRole is required")
	}

	if validationErr := principal(&api.DataLakePrincipal{DataLakePrincipalIdentifier: role}); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "registration", arn)
		if _, ok := tx.Get(k); ok {
			return awserror.New("AlreadyExistsException", "Resource is already registered")
		}

		return storage.Write(
			tx,
			k,
			api.ResourceInfo{ResourceArn: in.ResourceArn, RoleArn: role, HybridAccessEnabled: in.HybridAccessEnabled},
		)
	})
	return &api.RegisterResourceResponse{}, err
}

func (s *Service) DescribeResource(
	ctx context.Context,
	in *api.DescribeResourceRequest,
) (*api.DescribeResourceResponse, error) {
	scope, err := scopeOf(ctx, nil)
	if err != nil {
		return nil, err
	}

	var r api.ResourceInfo
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		v, ok, e := storage.Read[api.ResourceInfo](tx, key(scope, "registration", common.Value(in.ResourceArn)))
		if e != nil {
			return e
		}

		if !ok {
			return missing("Resource is not registered")
		}

		r = *v
		return nil
	})
	return &api.DescribeResourceResponse{ResourceInfo: &r}, err
}

func (s *Service) DeregisterResource(
	ctx context.Context,
	in *api.DeregisterResourceRequest,
) (*api.DeregisterResourceResponse, error) {
	scope, err := scopeOf(ctx, nil)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "registration", common.Value(in.ResourceArn))
		if _, ok := tx.Get(k); !ok {
			return missing("Resource is not registered")
		}

		tx.Delete(k)
		return nil
	})
	return &api.DeregisterResourceResponse{}, err
}

func (s *Service) ListResources(ctx context.Context, in *api.ListResourcesRequest) (*api.ListResourcesResponse, error) {
	scope, err := scopeOf(ctx, nil)
	if err != nil {
		return nil, err
	}

	var items []api.ResourceInfo
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.ResourceInfo](tx, scope.Prefix("lakeformation", "registration"))
		for _, r := range records {
			items = append(items, *r)
		}

		return e
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(
		items,
		func(r api.ResourceInfo) string { return recordID(common.Value(r.ResourceArn)) },
		in.NextToken,
		in.MaxResults,
	)
	return &api.ListResourcesResponse{ResourceInfoList: items, NextToken: next}, err
}

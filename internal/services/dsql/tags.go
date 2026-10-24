package dsql

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dsql"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) tags(ctx context.Context, arn *string, change func(*record) error) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	remote, id, err := parseARN(scope, common.Value(arn))
	if err != nil {
		return nil, err
	}

	if remote.Region != scope.Region {
		return nil, missing()
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, id)
		if e != nil {
			return e
		}

		if e := change(r); e != nil {
			return e
		}

		return write(tx, scope, r)
	})
	return r, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	_, err := s.tags(ctx, in.ResourceArn, func(r *record) error {
		if err := tagging.CheckSet(in.Tags); err != nil {
			return invalid(err.Error())
		}

		tags, err := tagging.Merge(r.Tags, in.Tags)
		if err != nil {
			return invalid(err.Error())
		}

		r.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	_, err := s.tags(ctx, in.ResourceArn, func(r *record) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	r, err := s.tags(ctx, in.ResourceArn, func(_ *record) error { return nil })
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{Tags: r.Tags}, nil
}

func (s *Service) GetVpcEndpointServiceName(ctx context.Context, in *api.GetVpcEndpointServiceNameInput) (
	*api.GetVpcEndpointServiceNameOutput, error,
) {
	r, err := s.get(ctx, in.Identifier)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	return &api.GetVpcEndpointServiceNameOutput{
		ServiceName: new("com.amazonaws." + scope.Region + ".dsql-" + common.Value(r.Identifier)[:4]),
		ClusterVpcEndpoint: new(
			common.Value(r.Identifier) + ".dsql-" + common.Value(r.Identifier)[:4] + "." + scope.Region + ".on.aws",
		),
	}, nil
}

package ssoadmin

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := loadPermission(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}

		tags, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return awserror.Validation("%s", e)
		}

		r.Tags = tags
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if in == nil || len(in.TagKeys) < 1 || len(in.TagKeys) > 75 {
		return nil, awserror.Validation("Specify 1 to 75 TagKeys")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	for _, key := range in.TagKeys {
		if validationErr := checkTags(map[string]string{key: ""}); validationErr != nil {
			return nil, validationErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := loadPermission(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}

		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	var list []api.Tag
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := loadPermission(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}

		list = tagList(r.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(list, func(t api.Tag) string { return *t.Key }, in.NextToken, 50,
		awserror.Validation("Invalid NextToken"))
	return &api.ListTagsForResourceResponse{Tags: page, NextToken: next}, err
}

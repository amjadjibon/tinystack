package efs

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func taggable(tx storage.ReadTx, scope request.Scope, value *string) (*record, error) {
	_, kind, err := identifier(scope, value, "")
	if err != nil {
		return nil, err
	}
	if kind == kindMT {
		return nil, invalid("Mount targets cannot be tagged")
	}
	return read(tx, scope, value, kind)
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	if len(in.Tags) == 0 {
		return nil, invalid("Tags must not be empty")
	}
	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := taggable(tx, scope, in.ResourceId)
		if e != nil {
			return e
		}
		r.Tags, e = tagging.Merge(r.Tags, tags)
		if e != nil {
			return invalid(e.Error())
		}
		return save(tx, scope, r)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys requires 1-50 unique keys")
	}
	check := []api.Tag{}
	for _, key := range in.TagKeys {
		check = append(check, api.Tag{Key: new(key), Value: new("")})
	}
	if _, err := tagsMap(check); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := taggable(tx, scope, in.ResourceId)
		if e != nil {
			return e
		}
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return save(tx, scope, r)
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var tags []api.Tag
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := taggable(tx, scope, in.ResourceId)
		if e != nil {
			return e
		}
		tags = tagList(r.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}
	tags, next, err := page(
		tags,
		func(t api.Tag) string { return common.Value(t.Key) },
		in.NextToken,
		in.MaxResults,
		scope,
		"tags/"+common.Value(in.ResourceId),
	)
	return &api.ListTagsForResourceResponse{Tags: tags, NextToken: next}, err
}

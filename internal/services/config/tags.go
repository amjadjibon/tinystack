package config

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	if len(in.Tags) == 0 {
		return nil, invalid("Tags must not be empty")
	}
	add, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}
		tags, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return tagError(e)
		}
		r.Tags = tags
		return save(tx, scope, r)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys must contain 1-50 keys")
	}
	keys := map[string]string{}
	for _, key := range in.TagKeys {
		if _, dup := keys[key]; dup {
			return nil, invalid("TagKeys must be unique")
		}
		keys[key] = ""
	}
	if err := tagging.CheckSet(keys); err != nil {
		return nil, tagError(err)
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
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
	limit := common.Value(common.OrElse(in.Limit, new(int64(50))))
	if limit < 1 || limit > 100 {
		return nil, failure("InvalidLimitException", "Limit must be between 1 and 100")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var tags []api.Tag
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}
		tags = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	if err != nil {
		return nil, err
	}
	tags, next, err := common.Page(
		tags,
		func(t api.Tag) string { return common.Value(t.Key) },
		in.NextToken,
		int(limit),
		failure("InvalidNextTokenException", "Invalid NextToken"),
	)
	return &api.ListTagsForResourceResponse{Tags: tags, NextToken: next}, err
}

package wafv2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/services/wafcommon"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := wafcommon.Tags(in.Tags, true, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
	if err != nil {
		return nil, err
	}
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceARN), "", "")
		if e != nil {
			return e
		}
		r.Tags, e = tagging.Merge(r.Tags, tags)
		if e != nil {
			return wafcommon.TagError(e)
		}
		// Tagging is a separate API; it does not invalidate configuration lock tokens.
		return save(tx, scope, r)
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys requires 1-50 unique keys")
	}
	tags := make([]api.Tag, 0, len(in.TagKeys))
	for _, k := range in.TagKeys {
		tags = append(tags, api.Tag{Key: new(k), Value: new("")})
	}
	if _, err := wafcommon.Tags(tags, true, func(t api.Tag) (*string, *string) { return t.Key, t.Value }); err != nil {
		return nil, err
	}
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceARN), "", "")
		if e != nil {
			return e
		}
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return save(tx, scope, r)
	})
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = byARN(tx, scope, common.Value(in.ResourceARN), "", "")
		return e
	})
	if err != nil {
		return nil, err
	}
	tags := tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
	selected, next, err := page(
		tags,
		func(t api.Tag) string { return *t.Key },
		in.NextMarker,
		in.Limit,
		scope,
		"tags/"+r.ARN,
	)
	if err != nil {
		return nil, err
	}
	return &api.ListTagsForResourceResponse{
		NextMarker:         next,
		TagInfoForResource: &api.TagInfoForResource{ResourceARN: new(r.ARN), TagList: selected},
	}, nil
}

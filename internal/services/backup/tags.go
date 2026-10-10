package backup

import (
	"context"
	"maps"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	if len(in.Tags) == 0 {
		return nil, invalid("Tags must not be empty")
	}
	if err := tagsOK(in.Tags); err != nil {
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
		merged, e := tagging.Merge(r.Tags, in.Tags)
		if e != nil {
			return tagError(e)
		}
		r.Tags = merged
		return save(tx, scope, r)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	if len(in.TagKeyList) < 1 || len(in.TagKeyList) > 50 {
		return nil, invalid("TagKeyList must contain 1-50 unique keys")
	}
	check := map[string]string{}
	for _, key := range in.TagKeyList {
		if _, ok := check[key]; ok {
			return nil, invalid("TagKeyList must be unique")
		}
		check[key] = ""
	}
	if err := tagging.CheckSet(check); err != nil {
		return nil, invalid(err.Error())
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
		r.Tags = tagging.Remove(r.Tags, in.TagKeyList)
		return save(tx, scope, r)
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTags(ctx context.Context, in *api.ListTagsInput) (*api.ListTagsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}
		tags = r.Tags
		return nil
	})
	if err != nil {
		return nil, err
	}
	keys, next, err := page(
		slices.Sorted(maps.Keys(tags)),
		func(k string) string { return k },
		in.NextToken,
		in.MaxResults,
		scope,
		"tags/"+common.Value(in.ResourceArn),
	)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, key := range keys {
		out[key] = tags[key]
	}
	return &api.ListTagsOutput{Tags: out, NextToken: next}, nil
}

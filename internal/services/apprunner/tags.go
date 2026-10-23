package apprunner

import (
	"context"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) editTags(
	ctx context.Context,
	arn *string,
	change func(map[string]string) (map[string]string, error),
) ([]api.Tag, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, common.Value(arn), "")
		if e != nil {
			return e
		}

		tags, e = change(maps.Clone(r.Tags))
		if e != nil {
			return invalid("%s", e)
		}

		r.Tags = tags
		return save(tx, scope, r)
	})
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }), err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tagging.Merge(tags, add) },
	)
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	_, err := s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tagging.Remove(tags, in.TagKeys), nil },
	)
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tags, nil },
	)
	return &api.ListTagsForResourceResponse{Tags: tags}, err
}

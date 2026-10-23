package globalaccelerator

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagsMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil,
		func(key string) error { return invalid("Duplicate tag key %s", key) })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid("%s", err)
	}

	return out, nil
}

// TagResource merges tags into an accelerator record.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := tagsMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.change(ctx, in.ResourceArn, kindAccelerator, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		merged, e := tagging.Merge(r.Tags, tags)
		if e != nil {
			return invalid("%s", e)
		}

		r.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

// UntagResource removes accelerator tags.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	_, err := s.change(ctx, in.ResourceArn, kindAccelerator, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

// ListTagsForResource lists accelerator tags in key order.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	r, err := s.get(ctx, in.ResourceArn, kindAccelerator)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tagging.List(r.Tags, func(k, v string) api.Tag {
		return api.Tag{Key: new(k), Value: new(v)}
	})}, nil
}

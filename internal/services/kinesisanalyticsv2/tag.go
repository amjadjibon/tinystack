package kinesisanalyticsv2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	m, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags require keys and values."),
		func(k string) error { return invalid("Duplicate tag key %s.", k) },
	)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(m); err != nil {
		return nil, invalid("%s", err)
	}

	return m, nil
}

func (s *Service) editTags(ctx context.Context, arn *string, fn func(*record) error) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := arnName(scope, arn)
	if err != nil {
		return nil, err
	}

	return s.change(ctx, name, nil, func(_ storage.WriteTx, _ request.Scope, r *record) error { return fn(r) })
}

// ListTagsForResource returns the application's tags.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	r, err := s.editTags(ctx, in.ResourceARN, func(*record) error { return nil })
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags: tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }),
	}, nil
}

// TagResource merges tags into the application record.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if len(in.Tags) < 1 {
		return nil, invalid("Tags cannot be empty.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.editTags(ctx, in.ResourceARN, func(r *record) error {
		merged, e := tagging.Merge(r.Tags, tags)
		if e != nil {
			return invalid("%s", e)
		}

		r.Tags = merged
		return nil
	})
	return &api.TagResourceResponse{}, err
}

// UntagResource removes tags from the application record.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > tagging.Max {
		return nil, invalid("TagKeys requires 1-50 keys.")
	}

	for _, k := range in.TagKeys {
		if err := tagging.CheckSet(map[string]string{k: ""}); err != nil {
			return nil, invalid("%s", err)
		}
	}

	_, err := s.editTags(
		ctx,
		in.ResourceARN,
		func(r *record) error {
			r.Tags = tagging.Remove(r.Tags, in.TagKeys)
			return nil
		},
	)
	return &api.UntagResourceResponse{}, err
}

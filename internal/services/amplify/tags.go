package amplify

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

// TagResource serves the corresponding records-only Hosting operation.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if err := validateTagResourceRequest(in); err != nil {
		return nil, err
	}

	_, err := s.tagged(ctx, in.ResourceArn, func(r *record) error {
		tags, e := tagging.Merge(r.Tags, in.Tags)
		if e != nil {
			return bad("%s", e)
		}

		r.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

// UntagResource serves the corresponding records-only Hosting operation.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if err := validateUntagResourceRequest(in); err != nil {
		return nil, err
	}

	_, err := s.tagged(
		ctx,
		in.ResourceArn,
		func(r *record) error { r.Tags = tagging.Remove(r.Tags, in.TagKeys); return nil },
	)
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

// ListTagsForResource serves the corresponding records-only Hosting operation.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	if err := validateListTagsForResourceRequest(in); err != nil {
		return nil, err
	}

	r, err := s.tagged(ctx, in.ResourceArn, func(_ *record) error { return nil })
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: common.CloneTags(r.Tags)}, nil
}

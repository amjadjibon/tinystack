package signer

import (
	"context"
	"fmt"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/signer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

func profileName(ctx context.Context, arn *string) (*string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, ok := strings.CutPrefix(
		common.Value(arn),
		fmt.Sprintf("arn:aws:signer:%s:%s:/signing-profiles/", scope.Region, scope.AccountID),
	)
	if !ok || !namePattern.MatchString(name) {
		return nil, missing()
	}

	return new(name), nil
}

// TagResource merges checked tags into a signing profile.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	name, err := profileName(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid(err.Error())
	}

	_, err = s.change(ctx, name, func(r *profile) error {
		tags, e := tagging.Merge(r.Detail.Tags, in.Tags)
		if e != nil {
			return invalid(e.Error())
		}

		r.Detail.Tags = tags
		return nil
	})
	return &api.TagResourceResponse{}, err
}

// UntagResource removes signing profile tags.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	name, err := profileName(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	_, err = s.change(
		ctx,
		name,
		func(r *profile) error { r.Detail.Tags = tagging.Remove(r.Detail.Tags, in.TagKeys); return nil },
	)
	return &api.UntagResourceResponse{}, err
}

// ListTagsForResource returns signing profile tags.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	name, err := profileName(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	out, err := s.GetSigningProfile(ctx, &api.GetSigningProfileRequest{ProfileName: name})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: out.Tags}, nil
}

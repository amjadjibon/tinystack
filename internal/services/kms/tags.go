package kms

import (
	"context"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

// tagMap converts KMS tags to a map, rejecting missing keys.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.TagKey == nil || t.TagValue == nil {
			return nil, awserror.New("TagException", "Tags must have a TagKey and a TagValue.")
		}

		out[*t.TagKey] = *t.TagValue
	}

	return out, nil
}

func tagError(err error) error {
	return awserror.New("TagException", err.Error())
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		tags, mergeErr := tagging.Merge(k.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		k.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(
	ctx context.Context,
	in *api.UntagResourceRequest,
) (*api.UntagResourceOutput, error) {
	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		k.Tags = tagging.Remove(k.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

// ListResourceTags lists a key's tags in key order.
func (s *Service) ListResourceTags(
	ctx context.Context,
	in *api.ListResourceTagsRequest,
) (*api.ListResourceTagsResponse, error) {
	var tags map[string]string
	err := s.view(ctx, in.KeyId, false, func(_ request.Scope, k *key) error {
		tags = k.Tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	keys, next, err := page(
		slices.Sorted(maps.Keys(tags)), func(k string) string { return k }, in.Marker, in.Limit, 50, 50,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListResourceTagsResponse{Tags: []api.Tag{}, NextMarker: next, Truncated: new(next != nil)}
	for _, k := range keys {
		out.Tags = append(out.Tags, api.Tag{TagKey: new(k), TagValue: new(tags[k])})
	}

	return out, nil
}

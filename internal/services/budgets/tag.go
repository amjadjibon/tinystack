package budgets

import (
	"context"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

func tagMap(tags []api.ResourceTag) (map[string]string, error) {
	if len(tags) > 200 {
		return nil, invalid("ResourceTags supports at most 200 entries.")
	}

	out, err := tagging.Map(tags, func(t api.ResourceTag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags require Key and Value."), func(string) error { return invalid("Tag keys must be unique.") })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	account, name, err := resource(ctx, in.ResourceARN)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.ResourceTags)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, account, name, func(r *record) error {
		merged, e := tagging.Merge(r.Tags, tags)
		if e != nil {
			return invalid(e.Error())
		}

		r.Tags = merged
		return nil
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	account, name, err := resource(ctx, in.ResourceARN)
	if err != nil {
		return nil, err
	}

	if len(in.ResourceTagKeys) > 200 {
		return nil, invalid("ResourceTagKeys supports at most 200 entries.")
	}

	for _, k := range in.ResourceTagKeys {
		if n := utf8.RuneCountInString(k); n < 1 || n > 128 || tagging.Reserved(k) {
			return nil, invalid("Tag key is invalid.")
		}
	}

	err = s.change(
		ctx,
		account,
		name,
		func(r *record) error { r.Tags = tagging.Remove(r.Tags, in.ResourceTagKeys); return nil },
	)
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	account, name, err := resource(ctx, in.ResourceARN)
	if err != nil {
		return nil, err
	}

	r, err := s.get(ctx, account, name)
	return &api.ListTagsForResourceResponse{ResourceTags: tagging.List(r.Tags, func(k, v string) api.ResourceTag {
		return api.ResourceTag{Key: new(k), Value: new(v)}
	})}, err
}

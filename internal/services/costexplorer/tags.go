package costexplorer

import (
	"context"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var tagPattern = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

func tagMap(tags []api.ResourceTag) (map[string]string, error) {
	if len(tags) > 200 {
		return nil, awserror.Validation("ResourceTags has more than 200 entries.")
	}

	for _, t := range tags {
		if t.Key == nil || t.Value == nil || !tagPattern.MatchString(*t.Key) || !tagPattern.MatchString(*t.Value) {
			return nil, awserror.Validation("Resource tag key or value is invalid.")
		}
	}

	out, err := tagging.Map(
		tags,
		func(t api.ResourceTag) (*string, *string) { return t.Key, t.Value },
		nil,
		func(key string) error { return awserror.Validation("Duplicate resource tag key %q.", key) },
	)
	if err != nil {
		return nil, awserror.Validation("%s", err)
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	return out, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.ResourceTags)
	if err != nil {
		return nil, err
	}

	err = s.edit(ctx, in.ResourceArn, "", func(r *record, _ storage.WriteTx) error {
		merged, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return awserror.Validation("%s", e)
		}

		r.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.ResourceTagKeys) > 200 {
		return nil, awserror.Validation("ResourceTagKeys has more than 200 entries.")
	}

	for _, k := range in.ResourceTagKeys {
		if len(k) < 1 || len(k) > 128 || !tagPattern.MatchString(k) {
			return nil, awserror.Validation("Resource tag key is invalid.")
		}
	}

	err := s.edit(ctx, in.ResourceArn, "", func(r *record, _ storage.WriteTx) error {
		r.Tags = tagging.Remove(r.Tags, in.ResourceTagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	r, err := s.read(ctx, in.ResourceArn, "")
	if err != nil {
		return nil, err
	}

	tags := tagging.List(
		r.Tags,
		func(k, v string) api.ResourceTag { return api.ResourceTag{Key: new(k), Value: new(v)} },
	)
	return &api.ListTagsForResourceResponse{ResourceTags: tags}, nil
}

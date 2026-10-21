package bedrock

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkedTags(tags []api.Tag) (map[string]string, error) {
	out := map[string]string{}
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return nil, invalid("Tag key and value are required.")
		}

		if _, ok := out[*tag.Key]; ok {
			return nil, invalid("Duplicate tag key.")
		}

		out[*tag.Key] = *tag.Value
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func (s *Service) resourceTags(
	ctx context.Context,
	arn *string,
	fn func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	kind := guardrailKind
	recordKind := guardrailKind
	if strings.Contains(common.Value(arn), ":application-inference-profile/") {
		kind = applicationProfileKind
		recordKind = "profile"
	}

	if !strings.HasPrefix(common.Value(arn), "arn:") {
		return nil, invalid("A resource ARN is required.")
	}

	id, err := resourceID(scope, kind, common.Value(arn))
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, recordKind, id)
		if kind == guardrailKind {
			row, ok, e := storage.Read[guardrail](tx, key)
			if e != nil {
				return e
			}

			if !ok {
				return missing()
			}

			out, e = fn(row.Tags)
			if e != nil {
				return e
			}

			row.Tags = out
			return storage.Write(tx, key, row)
		}

		row, ok, e := storage.Read[inferenceProfile](tx, key)
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		out, e = fn(row.Tags)
		if e != nil {
			return e
		}

		row.Tags = out
		return storage.Write(tx, key, row)
	})
	return out, err
}

// TagResource merges checked tags into a guardrail or application inference profile.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := checkedTags(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.resourceTags(ctx, in.ResourceARN, func(old map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(old, tags)
		if e != nil {
			return nil, invalid(e.Error())
		}

		return merged, nil
	})
	return &api.TagResourceResponse{}, err
}

// UntagResource removes resource tags.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	keys := map[string]string{}
	for _, key := range in.TagKeys {
		keys[key] = ""
	}

	if err := tagging.CheckSet(keys); err != nil {
		return nil, invalid(err.Error())
	}

	_, err := s.resourceTags(
		ctx,
		in.ResourceARN,
		func(old map[string]string) (map[string]string, error) { return tagging.Remove(old, in.TagKeys), nil },
	)
	return &api.UntagResourceResponse{}, err
}

// ListTagsForResource lists tags on a guardrail or application inference profile.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.resourceTags(
		ctx,
		in.ResourceARN,
		func(old map[string]string) (map[string]string, error) { return old, nil },
	)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags: tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }),
	}, nil
}

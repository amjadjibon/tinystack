package transcribe

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transcribe"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("tag key and value are required"),
		func(k string) error { return invalid("duplicate tag %s", k) },
	)
	if err != nil {
		return nil, err
	}

	for key := range out {
		if tagging.Reserved(key) {
			return nil, invalid("%s", tagging.ErrReserved)
		}
	}

	return out, nil
}

func (s *Service) tags(
	ctx context.Context,
	resource string,
	change func(map[string]string) (map[string]string, error),
) ([]api.Tag, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := arn(scope, "", "")
	prefix = strings.TrimSuffix(prefix, "/")
	resourcePart, ok := strings.CutPrefix(resource, prefix)
	if !ok {
		return nil, missing(resource)
	}

	kind, name, ok := strings.Cut(resourcePart, "/")
	if !ok || (kind != kindVocabulary && kind != kindFilter && kind != kindJob) {
		return nil, missing(resource)
	}

	out := []api.Tag{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, name)
		if e != nil {
			return e
		}

		tags := r.Tags
		write := func(t map[string]string) error {
			r.Tags = t
			if r.Job != nil {
				r.Job.Tags = tagging.List(t, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
			}

			return storage.Write(tx, scope.Key(serviceName, kind, name), r)
		}

		if change != nil {
			tags, err = change(tags)
			if err != nil {
				return err
			}

			if writeErr := write(tags); writeErr != nil {
				return writeErr
			}
		}

		out = tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	return out, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.tags(ctx, common.Value(in.ResourceArn), func(old map[string]string) (map[string]string, error) {
		merged := maps.Clone(old)
		if merged == nil {
			merged = map[string]string{}
		}

		maps.Copy(merged, tags)
		// TagList has a modeled maximum of 200; merging is a cross-request constraint.
		if len(merged) > 200 {
			return nil, invalid("at most 200 resource tags are supported")
		}

		return merged, nil
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	_, err := s.tags(
		ctx,
		common.Value(in.ResourceArn),
		func(old map[string]string) (map[string]string, error) { return tagging.Remove(old, in.TagKeys), nil },
	)
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.tags(ctx, common.Value(in.ResourceArn), nil)
	return &api.ListTagsForResourceResponse{Tags: tags, ResourceArn: in.ResourceArn}, err
}

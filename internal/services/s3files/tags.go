package s3files

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(key, value string) api.Tag { return api.Tag{Key: new(key), Value: new(value)} })
}

// tagMap returns tags as a map after the shared checks; a repeated key is rejected.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil,
		func(key string) error { return invalid("tag key %s is repeated", key) })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid("%s", err.Error())
	}

	return out, nil
}

// tagged changes the tags of the file system or access point that ref names and returns the result.
func (s *Service) tagged(
	ctx context.Context,
	ref string,
	change func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	var tags map[string]string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if strings.HasPrefix(ref, "fsap-") || strings.Contains(ref, "/access-point/") {
			r, e := readAP(tx, scope, ref)
			if e != nil {
				return e
			}

			if r.Tags, e = change(r.Tags); e != nil {
				return e
			}

			tags = r.Tags
			return storage.Write(tx, apKey(scope, r.ID), r)
		}

		r, e := readFS(tx, scope, ref)
		if e != nil {
			return e
		}

		if r.Tags, e = change(r.Tags); e != nil {
			return e
		}

		tags = r.Tags
		return storage.Write(tx, fsKey(scope, r.ID), r)
	})

	return tags, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.tagged(ctx, common.Value(in.ResourceId), func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, add)
		if e != nil {
			return nil, invalid("%s", e.Error())
		}

		return merged, nil
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
	_, err := s.tagged(ctx, common.Value(in.ResourceId), func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.tagged(ctx, common.Value(in.ResourceId), func(current map[string]string) (map[string]string, error) {
		return current, nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(tagList(tags), func(t api.Tag) string { return common.Value(t.Key) }, in.NextToken,
		pageLimit(in.MaxResults, tagPageSize), errBadToken)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: slices.Clone(page), NextToken: next}, nil
}

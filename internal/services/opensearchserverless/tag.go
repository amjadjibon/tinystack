package opensearchserverless

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) editTags(ctx context.Context, arn *string,
	change func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf("arn:aws:aoss:%s:%s:collection/", scope.Region, scope.AccountID)
	if !strings.HasPrefix(common.Value(arn), prefix) {
		return nil, missing()
	}

	id := strings.TrimPrefix(*arn, prefix)
	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readCollection(tx, scope, new(id))
		if e != nil {
			return e
		}

		out, e = change(common.CloneTags(r.Tags))
		if e != nil {
			return e
		}

		r.Tags = out
		return storage.Write(tx, collectionKey(scope, id), r)
	})
	return out, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tags, nil },
	)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags: tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }),
	}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if len(in.Tags) == 0 {
		return nil, awserror.Validation("tags must not be empty.")
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.editTags(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(tags, add)
		if e != nil {
			return nil, awserror.Validation("%s", e)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > tagging.Max {
		return nil, awserror.Validation("Invalid tagKeys.")
	}

	for _, key := range in.TagKeys {
		if err := tagging.Check(map[string]string{key: ""}); err != nil {
			return nil, awserror.Validation("%s", err)
		}
	}

	_, err := s.editTags(
		ctx,
		in.ResourceArn,
		func(tags map[string]string) (map[string]string, error) { return tagging.Remove(tags, in.TagKeys), nil },
	)
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

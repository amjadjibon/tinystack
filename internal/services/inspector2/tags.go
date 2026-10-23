package inspector2

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) editTags(
	ctx context.Context,
	arn string,
	change func(map[string]string) (map[string]string, error),
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readFilter(tx, scope, arn)
		if e != nil {
			return e
		}

		tags, e := change(r.Tags)
		if e != nil {
			return awserror.Validation("%s", e)
		}

		r.Tags = tags
		return storage.Write(tx, scope.Key(serviceName, kindFilter, arn), r)
	})
	return err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	err := s.editTags(ctx, common.Value(in.ResourceArn), func(tags map[string]string) (map[string]string, error) {
		return tagging.Merge(tags, in.Tags)
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.editTags(ctx, common.Value(in.ResourceArn), func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(ctx context.Context, in *api.ListTagsForResourceRequest) (
	*api.ListTagsForResourceResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readFilter(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}

		tags = common.CloneTags(r.Tags)
		return nil
	})
	return &api.ListTagsForResourceResponse{Tags: tags}, err
}

package fis

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) changeTags(ctx context.Context, arn *string, add map[string]string, remove []string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	kind, id, err := parseARN(scope, common.Value(arn))
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readRecord(tx, scope, kind, id)
		if err != nil {
			return err
		}

		var tags map[string]string
		if r.Template != nil {
			tags = common.CloneTags(r.Template.Tags)
		} else {
			tags = common.CloneTags(r.Experiment.Tags)
		}

		maps.Copy(tags, add)
		for _, key := range remove {
			delete(tags, key)
		}

		if err := tagging.CheckSet(tags); err != nil {
			return awserror.Validation("%s", err)
		}

		if r.Template != nil {
			r.Template.Tags = tags
		} else {
			r.Experiment.Tags = tags
		}

		return storage.Write(tx, scope.Key(serviceName, kind, id), r)
	})
}

// TagResource adds tags to a template or experiment record.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.changeTags(ctx, in.ResourceArn, in.Tags, nil)
	return &api.TagResourceResponse{}, err
}

// UntagResource removes tags from a template or experiment record.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.changeTags(ctx, in.ResourceArn, nil, in.TagKeys)
	return &api.UntagResourceResponse{}, err
}

// ListTagsForResource returns a template or experiment’s stored tags.
func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	kind, id, err := parseARN(scope, common.Value(in.ResourceArn))
	if err != nil {
		return nil, err
	}

	r, err := s.get(ctx, kind, new(id))
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	if r.Template != nil {
		tags = r.Template.Tags
	} else {
		tags = r.Experiment.Tags
	}

	return &api.ListTagsForResourceResponse{Tags: tags}, nil
}

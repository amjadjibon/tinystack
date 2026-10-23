package securityhub

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Security Hub's tagging API supports the hub resource. Actions, subscriptions,
// and insights have no Tags member in the model and cannot be tagged.
func hubTags(value *string, h *hubRecord) error {
	if value == nil || *value != common.Value(h.Hub.HubArn) {
		return missing()
	}

	return nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, h *hubRecord) error {
		if err := hubTags(in.ResourceArn, h); err != nil {
			return err
		}

		if err := tagging.CheckSet(in.Tags); err != nil {
			return invalid(err.Error())
		}

		tags, err := tagging.Merge(h.Tags, in.Tags)
		if err != nil {
			return invalid(err.Error())
		}

		h.Tags = tags
		return storage.Write(tx, scope.Key(serviceName, "hub", "default"), h)
	})

	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, h *hubRecord) error {
		if err := hubTags(in.ResourceArn, h); err != nil {
			return err
		}

		h.Tags = tagging.Remove(h.Tags, in.TagKeys)
		return storage.Write(tx, scope.Key(serviceName, "hub", "default"), h)
	})

	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	out := &api.ListTagsForResourceResponse{}
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, h *hubRecord) error {
		if err := hubTags(in.ResourceArn, h); err != nil {
			return err
		}

		out.Tags = common.CloneTags(h.Tags)
		return nil
	})

	return out, err
}

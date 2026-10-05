package cognitoidp

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// taggedPool returns the ID of the scope's user pool arn names; only user
// pools take tags.
func taggedPool(scope request.Scope, arn *string) (*string, error) {
	id, ok := strings.CutPrefix(common.Value(arn), poolARN(scope, ""))
	if !ok || id == "" {
		return nil, notFound("The resource %s does not exist.", common.Value(arn))
	}

	return &id, nil
}

// tagged runs fn on the pool arn names and stores it after.
func (s *Service) tagged(ctx context.Context, arn *string, fn func(*pool) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	id, err := taggedPool(scope, arn)
	if err != nil {
		return err
	}

	return s.update(ctx, id, func(_ storage.WriteTx, _ request.Scope, p *pool) error { return fn(p) })
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.tagged(ctx, in.ResourceArn, func(p *pool) error {
		tags, mergeErr := tagging.Merge(p.Tags, in.Tags)
		if mergeErr != nil {
			return invalidParameter("%s", mergeErr.Error())
		}

		p.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.tagged(ctx, in.ResourceArn, func(p *pool) error {
		p.Tags = tagging.Remove(p.Tags, in.TagKeys)
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
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id, err := taggedPool(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{Tags: map[string]string{}}
	err = s.view(ctx, id, func(_ storage.ReadTx, _ request.Scope, p *pool) error {
		maps.Copy(out.Tags, p.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

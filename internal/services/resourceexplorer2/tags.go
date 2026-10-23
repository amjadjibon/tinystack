package resourceexplorer2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return bad()
	}

	return nil
}

func (s *Service) tags(ctx context.Context, a string, change func(*record) error) (map[string]string, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var tags map[string]string
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := scope.Key(serviceName, "view", a)
		r, ok, err := storage.Read[record](tx, k)
		if err != nil {
			return err
		}

		if !ok {
			r, err = loadIndex(tx, scope)
			if err != nil {
				return err
			}

			if common.Value(r.Index.Arn) != a {
				return missing()
			}

			k = indexKey(scope)
		}

		if change != nil {
			if changeErr := change(r); changeErr != nil {
				return changeErr
			}
		}

		tags = r.Tags
		return storage.Write(tx, k, r)
	})
	return tags, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	_, err := s.tags(ctx, common.Value(in.ResourceArn), func(r *record) error {
		tags, err := tagging.Merge(r.Tags, in.Tags)
		if err != nil {
			return bad()
		}

		r.Tags = tags
		return nil
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	_, err := s.tags(
		ctx,
		common.Value(in.ResourceArn),
		func(r *record) error { r.Tags = tagging.Remove(r.Tags, in.TagKeys); return nil },
	)
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	tags, err := s.tags(ctx, common.Value(in.ResourceArn), nil)
	return &api.ListTagsForResourceOutput{Tags: tags}, err
}

package sesv2

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagged returns the key of the identity, configuration set, or template an
// ARN names, and its tags.
func tagged(tx storage.ReadTx, scope request.Scope, resource *string) (
	key string, tags map[string]string, err error,
) {
	for _, kind := range []string{kindIdentity, kindConfigSet, kindTemplate} {
		name, ok := strings.CutPrefix(common.Value(resource), arn(scope, kind, ""))
		if !ok || name == "" {
			continue
		}

		switch kind {
		case kindIdentity:
			i, err := loadIdentity(tx, scope, &name)
			if err != nil {
				return "", nil, err
			}

			return identityKey(scope, name), i.Tags, nil
		case kindConfigSet:
			c, err := loadConfigSet(tx, scope, &name)
			if err != nil {
				return "", nil, err
			}

			return scope.Key(service, kind, name), c.Tags, nil
		default:
			t, err := loadTemplate(tx, scope, &name)
			if err != nil {
				return "", nil, err
			}

			return scope.Key(service, kind, name), t.Tags, nil
		}
	}

	return "", nil, badRequest("Invalid resource ARN %s.", common.Value(resource))
}

// retag replaces the tags of the resource an ARN names with fn's result,
// rewriting only the record's Tags.
func (s *Service) retag(
	ctx context.Context,
	resource *string,
	fn func(map[string]string) (map[string]string, error),
) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key, tags, err := tagged(tx, scope, resource)
		if err != nil {
			return err
		}

		if tags, err = fn(common.CloneTags(tags)); err != nil {
			return err
		}

		record, _, err := storage.Read[map[string]any](tx, key)
		if err != nil {
			return err
		}

		(*record)["Tags"] = tags
		return storage.Write(tx, key, record)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return nil, badRequest("%s", mergeErr.Error())
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
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
	var tags map[string]string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		_, tags, err = tagged(tx, scope, in.ResourceArn)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tagList(tags)}, nil
}

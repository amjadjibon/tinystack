package sfn

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// retag replaces the tags of the state machine or activity an ARN names.
func (s *Service) retag(ctx context.Context, arnString *string, fn func(map[string]string) map[string]string) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, kind, name, ok := parseARN(common.Value(arnString))
		if !ok || scope != m.Scope {
			return invalidARN(common.Value(arnString))
		}

		missing := awserror.New("ResourceNotFound", "Resource not found: '"+common.Value(arnString)+"'")
		switch kind {
		case arnStateMachine:
			sm, found, readErr := storage.Read[stateMachine](tx, machineKey(scope, name))
			if readErr != nil || !found {
				return errorsOr(readErr, missing)
			}

			if sm.Tags = fn(sm.Tags); checkTags(sm.Tags) != nil {
				return checkTags(sm.Tags)
			}

			return storage.Write(tx, machineKey(scope, name), sm)
		case kindActivity:
			a, found, readErr := storage.Read[activity](tx, activityKey(scope, name))
			if readErr != nil || !found {
				return errorsOr(readErr, missing)
			}

			if a.Tags = fn(a.Tags); checkTags(a.Tags) != nil {
				return checkTags(a.Tags)
			}

			return storage.Write(tx, activityKey(scope, name), a)
		}

		return missing
	})
}

func errorsOr(err, fallback error) error {
	if err != nil {
		return err
	}

	return fallback
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		out := maps.Clone(tags)
		if out == nil {
			out = map[string]string{}
		}

		maps.Copy(out, tagsOf(in.Tags))
		return out
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		return tagging.Remove(tags, in.TagKeys)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	var tags map[string]string
	err := s.retag(ctx, in.ResourceArn, func(t map[string]string) map[string]string {
		tags = t
		return t
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{Tags: tagList(tags)}, nil
}

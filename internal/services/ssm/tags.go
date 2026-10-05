package ssm

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// taggedParameter checks a tagging request's resource: TinyStack tags only
// parameters, named by ResourceId.
func taggedParameter(ctx context.Context, tx storage.ReadTx, kind, id *string) (*parameter, string, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, "", err
	}

	if value(kind, "") != resourceType {
		return nil, "", awserror.New("InvalidResourceType", "The resource type is not valid. TinyStack tags only "+
			"parameters.")
	}

	p, found, err := storage.Read[parameter](tx, parameterKey(m.Scope, value(id, "")))
	if err != nil {
		return nil, "", err
	}

	if !found || checkBareName(id) != nil {
		return nil, "", awserror.New("InvalidResourceId", "The resource ID is not valid. Verify that the resource "+
			"ID is correct and try again.")
	}

	return p, parameterKey(m.Scope, p.Name), nil
}

// retag applies fn to a parameter's tags and saves them.
func (s *Service) retag(ctx context.Context, kind, id *string, fn func(tags map[string]string) error) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, k, err := taggedParameter(ctx, tx, kind, id)
		if err != nil {
			return err
		}

		if p.Tags == nil {
			p.Tags = map[string]string{}
		}

		if fnErr := fn(p.Tags); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, k, p)
	})
}

func (s *Service) AddTagsToResource(
	ctx context.Context,
	in *api.AddTagsToResourceRequest,
) (*api.AddTagsToResourceResult, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(add) == 0 {
		return nil, validation("Tags must list at least one tag.")
	}

	err = s.retag(ctx, in.ResourceType, in.ResourceId, func(tags map[string]string) error {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		maps.Copy(tags, merged)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddTagsToResourceResult{}, nil
}

func (s *Service) RemoveTagsFromResource(
	ctx context.Context,
	in *api.RemoveTagsFromResourceRequest,
) (*api.RemoveTagsFromResourceResult, error) {
	if len(in.TagKeys) == 0 {
		return nil, validation("TagKeys must list at least one key.")
	}

	err := s.retag(ctx, in.ResourceType, in.ResourceId, func(tags map[string]string) error {
		tagging.Remove(tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveTagsFromResourceResult{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResult, error) {
	out := &api.ListTagsForResourceResult{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		p, _, err := taggedParameter(ctx, tx, in.ResourceType, in.ResourceId)
		if err != nil {
			return err
		}

		out.TagList = tagging.List(p.Tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

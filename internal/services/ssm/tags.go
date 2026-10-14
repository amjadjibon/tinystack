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

// taggedResource is the record whose tags a tagging request changes: a
// parameter or a document. TinyStack tags nothing else.
type taggedResource struct {
	tags *map[string]string
	save func(tx storage.WriteTx) error
}

func invalidResourceID() error {
	return awserror.New("InvalidResourceId", "The resource ID is not valid. Verify that the resource ID is "+
		"correct and try again.")
}

// tagged finds the resource a tagging request names, ResourceType and ResourceId.
func tagged(ctx context.Context, tx storage.ReadTx, kind, id *string) (*taggedResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	switch value(kind, "") {
	case resourceType:
		p, found, readErr := storage.Read[parameter](tx, parameterKey(m.Scope, value(id, "")))
		if readErr != nil {
			return nil, readErr
		}

		if !found || checkBareName(id) != nil {
			return nil, invalidResourceID()
		}

		return &taggedResource{tags: &p.Tags, save: func(w storage.WriteTx) error {
			return storage.Write(w, parameterKey(m.Scope, p.Name), p)
		}}, nil
	case documentKind:
		name, refErr := documentRef(id)
		if refErr != nil {
			return nil, invalidResourceID()
		}

		if _, _, isBuiltin := builtin(name); isBuiltin {
			return nil, awserror.New("InvalidResourceId", "AWS-owned documents can't be tagged.")
		}

		rec, found, readErr := storage.Read[docRecord](tx, docKey(m.Scope, name))
		if readErr != nil {
			return nil, readErr
		}

		if !found {
			return nil, invalidResourceID()
		}

		return &taggedResource{tags: &rec.Tags, save: func(w storage.WriteTx) error {
			return storage.Write(w, docKey(m.Scope, rec.Name), rec)
		}}, nil
	}

	return nil, awserror.New("InvalidResourceType", "The resource type is not valid. TinyStack tags only "+
		"parameters and documents.")
}

// retag applies fn to the tags of a resource and saves them.
func (s *Service) retag(ctx context.Context, kind, id *string, fn func(tags map[string]string) error) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagged(ctx, tx, kind, id)
		if err != nil {
			return err
		}

		if *r.tags == nil {
			*r.tags = map[string]string{}
		}

		if fnErr := fn(*r.tags); fnErr != nil {
			return fnErr
		}

		return r.save(tx)
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
		r, err := tagged(ctx, tx, in.ResourceType, in.ResourceId)
		if err != nil {
			return err
		}

		out.TagList = tagging.List(*r.tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

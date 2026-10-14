package codeconnections

import (
	"context"
	"errors"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codeconnections"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return fault("LimitExceededException", "%s", err.Error())
	}

	return awserror.Validation("%s", err.Error())
}

// tagMap reads tags given at creation or by TagResource.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil, nil)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, tagError(err)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

// tagSet holds a resource's tags. Every stored resource embeds it, so its tags stay in the resource's own record.
type tagSet struct {
	Tags map[string]string
}

func (t *tagSet) tagMap() map[string]string        { return t.Tags }
func (t *tagSet) setTagMap(tags map[string]string) { t.Tags = tags }

type tagged interface {
	tagMap() map[string]string
	setTagMap(map[string]string)
}

// editTags applies change to the tags of the resource an ARN names, in one transaction.
func (s *Service) editTags(
	ctx context.Context,
	value *string,
	change func(tags map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	kind, id, err := split(scope, value)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var r tagged
		var found bool
		var e error
		if kind == kindConnection {
			r, found, e = readTagged[connectionRecord](tx, key(scope, kind, id))
		} else {
			r, found, e = readTagged[hostRecord](tx, key(scope, kind, id))
		}

		if e != nil {
			return e
		}

		if !found {
			return notFound(kind, *value)
		}

		if out, e = change(maps.Clone(r.tagMap())); e != nil {
			return e
		}

		r.setTagMap(out)
		return storage.Write(tx, key(scope, kind, id), r)
	})

	return out, err
}

func readTagged[T any, P interface {
	*T
	tagged
}](tx storage.ReadTx, k string) (tagged, bool, error) {
	r, found, err := storage.Read[T](tx, k)
	if err != nil || !found {
		return nil, found, err
	}

	return P(r), true, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	tags, err := s.editTags(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tags, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{Tags: tagList(tags)}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(add) == 0 {
		return nil, awserror.Validation("Tags is required.")
	}

	_, err = s.editTags(ctx, in.ResourceArn, func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, add)
		if e != nil {
			return nil, tagError(e)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	if len(in.TagKeys) == 0 {
		return nil, awserror.Validation("TagKeys is required.")
	}

	_, err := s.editTags(ctx, in.ResourceArn, func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

package emr

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

func parseTags(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag Key and Value required."),
		nil,
	)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid("%s", err)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func (s *Service) AddTags(ctx context.Context, in *api.AddTagsInput) (*api.AddTagsOutput, error) {
	add, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.change(ctx, in.ResourceId, func(r *record) error {
		tags, e := parseTags(r.Cluster.Tags)
		if e != nil {
			return e
		}

		merged, e := tagging.Merge(tags, add)
		if e != nil {
			return invalid("%s", e)
		}

		r.Cluster.Tags = tagList(merged)
		return nil
	})
	return &api.AddTagsOutput{}, err
}

func (s *Service) RemoveTags(ctx context.Context, in *api.RemoveTagsInput) (*api.RemoveTagsOutput, error) {
	_, err := s.change(ctx, in.ResourceId, func(r *record) error {
		tags, e := parseTags(r.Cluster.Tags)
		if e != nil {
			return e
		}

		r.Cluster.Tags = tagList(tagging.Remove(tags, in.TagKeys))
		return nil
	})
	return &api.RemoveTagsOutput{}, err
}

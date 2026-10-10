package cloudtrail

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudtrail"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

func parseTags(tags []api.Tag, keysOnly bool) (map[string]string, error) {
	missing := invalid("Tag key and value are required")
	if keysOnly {
		missing = nil
	}

	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, missing,
		func(string) error { return invalid("Duplicate tag key") })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func (s *Service) AddTags(ctx context.Context, in *api.AddTagsRequest) (*api.AddTagsResponse, error) {
	tags, err := parseTags(in.TagsList, false)
	if err != nil {
		return nil, err
	}

	_, err = s.edit(ctx, common.Value(in.ResourceId), func(r *record) error {
		var e error
		r.Tags, e = tagging.Merge(r.Tags, tags)
		if e != nil {
			return invalid(e.Error())
		}

		return nil
	})
	return &api.AddTagsResponse{}, err
}

func (s *Service) RemoveTags(ctx context.Context, in *api.RemoveTagsRequest) (*api.RemoveTagsResponse, error) {
	tags, err := parseTags(in.TagsList, true)
	if err != nil {
		return nil, err
	}

	_, err = s.edit(ctx, common.Value(in.ResourceId), func(r *record) error {
		for k := range tags {
			delete(r.Tags, k)
		}

		return nil
	})
	return &api.RemoveTagsResponse{}, err
}

func (s *Service) ListTags(ctx context.Context, in *api.ListTagsRequest) (*api.ListTagsResponse, error) {
	if len(in.ResourceIdList) < 1 || len(in.ResourceIdList) > 20 {
		return nil, invalid("ResourceIdList must have 1 to 20 resources")
	}

	if in.NextToken != nil {
		return nil, awserror.New("InvalidNextTokenException", "ListTags has no subsequent pages")
	}

	out := &api.ListTagsResponse{}
	for _, id := range in.ResourceIdList {
		r, err := s.get(ctx, id)
		if err != nil {
			return nil, err
		}

		out.ResourceTagList = append(
			out.ResourceTagList,
			api.ResourceTag{ResourceId: r.Trail.TrailARN, TagsList: tagList(r.Tags)},
		)
	}

	return out, nil
}

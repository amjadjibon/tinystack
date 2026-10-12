package route53resolver

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	if len(tags) > 200 {
		return nil, invalid("Tags must contain at most 200 entries")
	}

	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag Key and Value are required"), func(string) error { return invalid("Duplicate tag key") })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func (s *Service) changeTags(ctx context.Context, resource *string, add map[string]string, remove []string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	kind, id, err := resourceReference(scope, resource)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, new(id))
		if e != nil {
			return e
		}

		r.Tags, e = tagging.Merge(r.Tags, add)
		if e != nil {
			return invalid(e.Error())
		}

		r.Tags = tagging.Remove(r.Tags, remove)
		return save(tx, scope, r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if in == nil || in.Tags == nil {
		return nil, invalid("Tags is required")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if err := s.changeTags(ctx, in.ResourceArn, tags, nil); err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if in == nil || in.TagKeys == nil {
		return nil, invalid("TagKeys is required")
	}

	if len(in.TagKeys) > 200 {
		return nil, invalid("TagKeys must contain at most 200 entries")
	}

	for _, key := range in.TagKeys {
		if err := checkString(new(key), 1, 128, "TagKey"); err != nil {
			return nil, err
		}
	}

	if err := s.changeTags(ctx, in.ResourceArn, nil, in.TagKeys); err != nil {
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

	if in == nil {
		return nil, invalid("Input is required")
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	kind, id, err := resourceReference(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var tags []api.Tag
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, kind, new(id))
		if e != nil {
			return e
		}

		tags = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	if err != nil {
		return nil, err
	}

	tags, next, err := common.Page(tags, func(t api.Tag) string { return *t.Key }, in.NextToken, limit,
		awserror.New("InvalidNextTokenException", "Invalid NextToken"))
	return &api.ListTagsForResourceResponse{Tags: tags, NextToken: next}, err
}

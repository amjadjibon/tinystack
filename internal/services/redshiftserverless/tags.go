package redshiftserverless

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag key and value required"),
		func(k string) error { return invalid("Duplicate tag: " + k) },
	)
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func (s *Service) changeTags(ctx context.Context, a *string, add map[string]string, remove []string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(a))
		if e != nil {
			return e
		}

		if r.Namespace == nil && r.Workgroup == nil && r.Snapshot == nil {
			return invalid("Resource does not support tags")
		}

		tags, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return invalid(e.Error())
		}

		r.Tags = tagging.Remove(tags, remove)
		return save(tx, scope, r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, s.changeTags(ctx, in.ResourceArn, tags, nil)
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	return &api.UntagResourceResponse{}, s.changeTags(ctx, in.ResourceArn, nil, in.TagKeys)
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}

		if r.Namespace == nil && r.Workgroup == nil && r.Snapshot == nil {
			return invalid("Resource does not support tags")
		}

		out.Tags = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	return out, err
}

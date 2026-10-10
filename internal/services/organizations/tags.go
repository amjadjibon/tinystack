package organizations

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/organizations"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	ts, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.ResourceId), "")
		if txErr != nil {
			return txErr
		}

		x.Tags, txErr = tagging.Merge(x.Tags, ts)
		if txErr != nil {
			return invalid(txErr.Error())
		}

		return nil
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.ResourceId), "")
		if txErr != nil {
			return txErr
		}

		x.Tags = tagging.Remove(x.Tags, in.TagKeys)
		return nil
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	out := &api.ListTagsForResourceResponse{}
	err := s.view(ctx, true, func(r *record) error {
		x, txErr := lookup(r, common.Value(in.ResourceId), "")
		if txErr != nil {
			return txErr
		}

		items := tagging.List(x.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		out.Tags, out.NextToken, txErr = common.Page(
			items,
			func(t api.Tag) string { return *t.Key },
			in.NextToken,
			50,
			invalid("Invalid NextToken"),
		)
		return txErr
	})
	return out, err
}

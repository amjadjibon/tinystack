package shield

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		nil,
		func(_ string) error { return invalid("Duplicate tag key") },
	)
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func (s *Service) tagged(ctx context.Context, value string, f func(*record) error) error {
	return s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		prefix := "arn:aws:shield::" + scope.AccountID + ":"
		if !strings.HasPrefix(value, prefix) {
			return missing()
		}

		kind, id, ok := strings.Cut(strings.TrimPrefix(value, prefix), "/")
		if !ok || kind != kindProtection && kind != kindGroup {
			return missing()
		}

		r, err := read(tx, scope, kind, id)
		if err != nil {
			return err
		}

		if err := f(r); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(serviceName, kind, id), r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	operationErr := s.tagged(ctx, common.Value(in.ResourceARN), func(r *record) error {
		merged, err := tagging.Merge(r.Tags, tags)
		if err != nil {
			return invalid(err.Error())
		}

		r.Tags = merged
		return nil
	})
	if operationErr != nil {
		return nil, operationErr
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.tagged(
		ctx,
		common.Value(in.ResourceARN),
		func(r *record) error { r.Tags = tagging.Remove(r.Tags, in.TagKeys); return nil },
	)
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	out := &api.ListTagsForResourceResponse{}
	err := s.tagged(ctx, common.Value(in.ResourceARN), func(r *record) error {
		out.Tags = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	return out, err
}

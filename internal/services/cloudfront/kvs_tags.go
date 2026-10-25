package cloudfront

import (
	"context"
	"errors"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func kvsTags(in *api.Tags) (map[string]string, error) {
	var items []api.Tag
	if in != nil {
		items = in.Items
	}

	tags, err := tagging.Map(
		items,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalidArgument("Tag key and value are required"),
		func(string) error { return invalidArgument("Duplicate tag key") },
	)
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(tags); err != nil {
		return nil, invalidArgument(err.Error())
	}

	return tags, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	tags, err := kvsTags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.kvsTagTransaction(ctx, common.Value(in.Resource), func(_ storage.WriteTx, _ string, r *KVSRecord) error {
		merged, e := tagging.Merge(r.Tags, tags)
		if e != nil {
			return invalidArgument(e.Error())
		}

		r.Tags = merged
		return nil
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	err := s.kvsTagTransaction(ctx, common.Value(in.Resource), func(_ storage.WriteTx, _ string, r *KVSRecord) error {
		if in.TagKeys != nil {
			r.Tags = tagging.Remove(r.Tags, in.TagKeys.Items)
		}

		return nil
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResult, error) {
	out := &api.ListTagsForResourceResult{}
	err := s.kvsTagTransaction(ctx, common.Value(in.Resource), func(_ storage.WriteTx, _ string, r *KVSRecord) error {
		out.Tags = &api.Tags{
			Items: tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }),
		}

		return nil
	})
	return out, err
}

// kvsTagTransaction translates data-plane lookup failures to CloudFront's tag error shape.
func (s *Service) kvsTagTransaction(
	ctx context.Context,
	arn string,
	fn func(storage.WriteTx, string, *KVSRecord) error,
) error {
	err := s.KVSTransaction(ctx, arn, fn)
	var awsErr *awserror.Error
	if errors.As(err, &awsErr) && awsErr.Code == "ResourceNotFoundException" {
		return awserror.New("NoSuchResource", "Key value store does not exist")
	}

	return err
}

package cloudfront

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagSet(in *api.Tags) (map[string]string, error) {
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
	tags, err := tagSet(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.updateTags(ctx, common.Value(in.Resource), func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, tags)
		if e != nil {
			return nil, invalidArgument(e.Error())
		}

		return merged, nil
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	err := s.updateTags(ctx, common.Value(in.Resource), func(current map[string]string) (map[string]string, error) {
		if in.TagKeys == nil {
			return current, nil
		}

		return tagging.Remove(current, in.TagKeys.Items), nil
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResult, error) {
	out := &api.ListTagsForResourceResult{}
	err := s.updateTags(ctx, common.Value(in.Resource), func(current map[string]string) (map[string]string, error) {
		out.Tags = &api.Tags{
			Items: tagging.List(current, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} }),
		}

		return current, nil
	})
	return out, err
}

// updateTags stores what fn makes of the tags of the distribution or key value store that arn names. Listing uses it
// with a fn that returns the tags unchanged, as both resource kinds keep their tags in their own record.
func (s *Service) updateTags(
	ctx context.Context,
	arn string,
	fn func(map[string]string) (map[string]string, error),
) error {
	m, err := request.Global(ctx)
	if err != nil {
		return err
	}

	if id, ok := strings.CutPrefix(arn, fmt.Sprintf("arn:aws:cloudfront::%s:distribution/", m.AccountID)); ok {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			k := key(m.AccountID, kindDistribution, id)
			rec, found, readErr := storage.Read[distributionRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			if !found {
				return awserror.New("NoSuchResource", "The specified resource does not exist")
			}

			tags, fnErr := fn(rec.Tags)
			if fnErr != nil {
				return fnErr
			}

			rec.Tags = tags
			return storage.Write(tx, k, rec)
		})
	}

	err = s.KVSTransaction(ctx, arn, func(_ storage.WriteTx, _ string, r *KVSRecord) error {
		tags, fnErr := fn(r.Tags)
		if fnErr != nil {
			return fnErr
		}

		r.Tags = tags
		return nil
	})

	// The data plane reports its own lookup failure; tag operations report CloudFront's.
	var awsErr *awserror.Error
	if errors.As(err, &awsErr) && awsErr.Code == "ResourceNotFoundException" {
		return awserror.New("NoSuchResource", "The specified resource does not exist")
	}

	return err
}

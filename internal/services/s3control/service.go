// Package s3control exposes general purpose bucket tagging over S3 Control.
// Buckets and their inventory remain owned by S3; this service has no records.
package s3control

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

// Service delegates to S3 so both API families observe the same tag set.
type Service struct{ buckets *s3.Service }

func New(buckets *s3.Service) *Service { return &Service{buckets: buckets} }

func bucket(ctx context.Context, account, arn *string) (*string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(account) != scope.AccountID {
		return nil, &awserror.Error{Code: "AccessDenied", Message: "Account does not own the resource", StatusCode: 403}
	}

	name, ok := strings.CutPrefix(common.Value(arn), "arn:aws:s3:::")
	if !ok || name == "" || strings.ContainsAny(name, "/:") {
		return nil, awserror.New("InvalidRequest", "TinyStack S3 Control supports only general purpose bucket ARNs")
	}

	return new(name), nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResult, error) {
	name, err := bucket(ctx, in.AccountId, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	tags := make(map[string]string, len(in.Tags))
	for _, tag := range in.Tags {
		key := common.Value(tag.Key)
		if _, exists := tags[key]; exists {
			return nil, awserror.New("InvalidTag", "Duplicate tag key")
		}

		tags[key] = common.Value(tag.Value)
	}

	if err := tagging.CheckSet(tags); err != nil {
		return nil, awserror.New("InvalidTag", err.Error())
	}

	if err := s.buckets.MergeBucketTags(ctx, name, in.AccountId, tags, nil); err != nil {
		return nil, err
	}

	return &api.TagResourceResult{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResult, error) {
	name, err := bucket(ctx, in.AccountId, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	for _, key := range in.TagKeys {
		if tagging.Reserved(key) {
			return nil, awserror.New("InvalidTag", "System tags cannot be removed")
		}
	}

	if err := s.buckets.MergeBucketTags(ctx, name, in.AccountId, nil, in.TagKeys); err != nil {
		return nil, err
	}

	return &api.UntagResourceResult{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResult, error) {
	name, err := bucket(ctx, in.AccountId, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	out, err := s.buckets.GetBucketTagging(
		ctx,
		&s3api.GetBucketTaggingRequest{Bucket: name, ExpectedBucketOwner: in.AccountId},
	)
	if err != nil {
		var awsErr *awserror.Error
		if errors.As(err, &awsErr) && awsErr.Code == "NoSuchTagSet" {
			return &api.ListTagsForResourceResult{Tags: []api.Tag{}}, nil
		}

		return nil, err
	}

	tags := make([]api.Tag, len(out.TagSet))
	for i, tag := range out.TagSet {
		tags[i] = api.Tag{Key: tag.Key, Value: tag.Value}
	}

	return &api.ListTagsForResourceResult{Tags: tags}, nil
}

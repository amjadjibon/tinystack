package s3

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func invalidTag(message string) error { return fail(http.StatusBadRequest, "InvalidTag", message) }

// bucketTags validates a bucket tag set, which replaces the previous one.
func bucketTags(in *api.Tagging) (map[string]string, error) {
	if in == nil {
		return nil, fail(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.")
	}

	tags := make(map[string]string, len(in.TagSet))
	for _, tag := range in.TagSet {
		k := value(tag.Key)
		if _, dup := tags[k]; dup {
			return nil, invalidTag("Cannot provide multiple Tags with the same key")
		}

		if strings.HasPrefix(k, "aws:") {
			return nil, invalidTag("System tags cannot be added/updated by requester")
		}

		tags[k] = value(tag.Value)
	}

	if err := tagging.Check(tags); err != nil {
		return nil, invalidTag("The TagSet is not valid: " + err.Error())
	}

	return tags, nil
}

// updateBucket applies fn to the caller's bucket metadata and saves it.
func (s *Service) updateBucket(ctx context.Context, bucket, owner *string, fn func(*bucketMeta)) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		var meta bucketMeta
		if _, readErr := readJSON(tx, key, &meta); readErr != nil {
			return readErr
		}

		fn(&meta)
		return writeJSON(tx, key, &meta)
	})
}

func (s *Service) PutBucketTagging(
	ctx context.Context,
	in *api.PutBucketTaggingRequest,
) (*api.PutBucketTaggingOutput, error) {
	tags, err := bucketTags(in.Tagging)
	if err != nil {
		return nil, err
	}

	err = s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Tags = tags })
	if err != nil {
		return nil, err
	}

	return &api.PutBucketTaggingOutput{}, nil
}

func (s *Service) DeleteBucketTagging(
	ctx context.Context,
	in *api.DeleteBucketTaggingRequest,
) (*api.DeleteBucketTaggingOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Tags = nil })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketTaggingOutput{}, nil
}

func (s *Service) GetBucketTagging(
	ctx context.Context,
	in *api.GetBucketTaggingRequest,
) (*api.GetBucketTaggingOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var meta bucketMeta
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		_, readErr := readJSON(tx, key, &meta)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	if len(meta.Tags) == 0 {
		return nil, fail(http.StatusNotFound, "NoSuchTagSet", "The TagSet does not exist")
	}

	out := &api.GetBucketTaggingOutput{}
	for _, k := range slices.Sorted(maps.Keys(meta.Tags)) {
		out.TagSet = append(out.TagSet, api.Tag{Key: ptr(k), Value: ptr(meta.Tags[k])})
	}

	return out, nil
}

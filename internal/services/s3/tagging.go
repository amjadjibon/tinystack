package s3

import (
	"context"
	"maps"
	"net/http"
	"net/url"
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
		out.TagSet = append(out.TagSet, api.Tag{Key: new(k), Value: new(meta.Tags[k])})
	}

	return out, nil
}

// maxObjectTags is how many tags an object may have.
const maxObjectTags = 10

// objectTags validates an object tag set, which replaces the previous one.
func objectTags(set []api.Tag) (map[string]string, error) {
	tags := make(map[string]string, len(set))
	for _, tag := range set {
		k, v := value(tag.Key), value(tag.Value)
		if _, dup := tags[k]; dup {
			return nil, invalidTag("Cannot provide multiple Tags with the same key")
		}

		if strings.HasPrefix(k, "aws:") {
			return nil, invalidTag("System tags cannot be added/updated by requester")
		}

		tags[k] = v
	}

	if len(tags) > maxObjectTags {
		return nil, fail(http.StatusBadRequest, "BadRequest", "Object tags cannot be greater than 10")
	}

	if err := tagging.Check(tags); err != nil {
		return nil, invalidTag("The TagSet is not valid: " + err.Error())
	}

	return tags, nil
}

// headerTags parses the URL-encoded tag set of an x-amz-tagging header.
func headerTags(header *string) (map[string]string, error) {
	if header == nil {
		return map[string]string{}, nil
	}

	values, err := url.ParseQuery(*header)
	if err != nil {
		return nil, invalidArgument("The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL " +
			"query parameters without tag name duplicates.")
	}

	var set []api.Tag
	for k, vs := range values {
		for _, v := range vs {
			set = append(set, api.Tag{Key: new(k), Value: new(v)})
		}
	}

	return objectTags(set)
}

// tagSet renders tags in key order.
func tagSet(tags map[string]string) []api.Tag {
	set := []api.Tag{}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		set = append(set, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return set
}

// retagObject applies fn to the tags of the caller's object.
func (s *Service) retagObject(
	ctx context.Context,
	bucket, key, owner *string,
	fn func(map[string]string) map[string]string,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	if guardErr := validKey(key); guardErr != nil {
		return guardErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		prefix, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		var obj object
		found, readErr := readJSON(tx, prefix+"/o/"+*key, &obj)
		if readErr != nil {
			return readErr
		}

		if !found {
			return noSuchKey()
		}

		obj.Tags = fn(obj.Tags)
		return writeJSON(tx, prefix+"/o/"+*key, &obj)
	})
}

func (s *Service) PutObjectTagging(
	ctx context.Context,
	in *api.PutObjectTaggingRequest,
) (*api.PutObjectTaggingOutput, error) {
	if in.Tagging == nil {
		return nil, fail(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.")
	}

	tags, err := objectTags(in.Tagging.TagSet)
	if err != nil {
		return nil, err
	}

	err = s.retagObject(ctx, in.Bucket, in.Key, in.ExpectedBucketOwner, func(map[string]string) map[string]string {
		return tags
	})
	if err != nil {
		return nil, err
	}

	return &api.PutObjectTaggingOutput{}, nil
}

func (s *Service) DeleteObjectTagging(
	ctx context.Context,
	in *api.DeleteObjectTaggingRequest,
) (*api.DeleteObjectTaggingOutput, error) {
	err := s.retagObject(ctx, in.Bucket, in.Key, in.ExpectedBucketOwner, func(map[string]string) map[string]string {
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteObjectTaggingOutput{}, nil
}

// GetObjectTagging returns the object's tags; an object without tags has an
// empty tag set.
func (s *Service) GetObjectTagging(
	ctx context.Context,
	in *api.GetObjectTaggingRequest,
) (*api.GetObjectTaggingOutput, error) {
	obj, _, err := s.read(ctx, in.Bucket, in.Key, nil, nil, in.ExpectedBucketOwner, false)
	if err != nil {
		return nil, err
	}

	return &api.GetObjectTaggingOutput{TagSet: tagSet(obj.Tags)}, nil
}

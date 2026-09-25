package s3

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	directiveCopy    = "COPY"
	directiveReplace = "REPLACE"
)

// copySource splits an x-amz-copy-source header, "<bucket>/<key>" with an
// optional leading slash, URL-encoded. Version IDs are not supported.
func copySource(header *string) (bucket, key string, err error) {
	raw := strings.TrimPrefix(value(header), "/")
	if path, query, hasQuery := strings.Cut(raw, "?"); hasQuery {
		if strings.HasPrefix(query, "versionId=") {
			return "", "", fail(http.StatusNotImplemented, "NotImplemented",
				"TinyStack does not support object versions")
		}

		raw = path
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", invalidArgument("Invalid copy source encoding")
	}

	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", invalidArgument("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}

	return bucket, key, nil
}

// directive validates a metadata or tagging directive, COPY by default.
func directive(p *string, name string) (string, error) {
	switch d := value(p); d {
	case "":
		return directiveCopy, nil
	case directiveCopy, directiveReplace:
		return d, nil
	default:
		return "", invalidArgument("Unknown " + name + " directive.")
	}
}

// resolveSource returns the storage prefix of a bucket the caller owns in any
// region, as a copy source may be.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func resolveSource(tx storage.ReadTx, m request.Metadata, name string, expectedOwner *string) (string, error) {
	var index bucketIndex
	found, err := readJSON(tx, indexPrefix+name, &index)
	if err != nil {
		return "", err
	}

	if !found {
		return "", noSuchBucket()
	}

	if index.Account != m.AccountID || (expectedOwner != nil && *expectedOwner != m.AccountID) {
		return "", accessDenied()
	}

	return m.AccountID + "/" + index.Region + "/s3/" + name, nil
}

// CopyObject copies an object within or between the caller's buckets. The
// copy keeps the source's metadata, content type, and tags unless the
// directives replace them; copying an object onto itself must replace
// something.
func (s *Service) CopyObject(ctx context.Context, in *api.CopyObjectRequest) (*api.CopyObjectOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	sourceBucket, sourceKey, err := copySource(in.CopySource)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(&sourceKey); guardErr != nil {
		return nil, guardErr
	}

	if c := value(in.StorageClass); c != "" && c != storageStandard {
		return nil, fail(
			http.StatusNotImplemented,
			"NotImplemented",
			"TinyStack only supports the STANDARD storage class",
		)
	}

	metadataDirective, err := directive(in.MetadataDirective, "metadata")
	if err != nil {
		return nil, err
	}

	taggingDirective, err := directive(in.TaggingDirective, "tagging")
	if err != nil {
		return nil, err
	}

	tags, err := headerTags(in.Tagging)
	if err != nil {
		return nil, err
	}

	if sourceBucket == value(in.Bucket) && sourceKey == value(in.Key) && metadataDirective == directiveCopy &&
		taggingDirective == directiveCopy && in.StorageClass == nil {
		return nil, fail(http.StatusBadRequest, "InvalidRequest", "This copy request is illegal because it is "+
			"trying to copy an object to itself without changing the object's metadata, storage class, website "+
			"redirect location or encryption attributes.")
	}

	var obj object
	var cfg *api.NotificationConfiguration
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		source, resolveErr := resolveSource(tx, m, sourceBucket, in.ExpectedSourceBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		target, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		found, readErr := readJSON(tx, source+"/o/"+sourceKey, &obj)
		if readErr != nil {
			return readErr
		}

		if !found {
			return noSuchKey()
		}

		if in.CopySourceIfMatch != nil && !matches(*in.CopySourceIfMatch, obj.ETag) ||
			in.CopySourceIfNoneMatch != nil && matches(*in.CopySourceIfNoneMatch, obj.ETag) {
			return preconditionFailed()
		}

		if metadataDirective == directiveReplace {
			obj.Metadata, obj.ContentType = maps.Clone(in.Metadata), value(in.ContentType)
			if obj.ContentType == "" {
				obj.ContentType = defaultType
			}
		}

		if taggingDirective == directiveReplace {
			obj.Tags = tags
		}

		var cfgErr error
		if cfg, cfgErr = notifications(tx, target); cfgErr != nil {
			return cfgErr
		}

		body, _ := tx.Get(source + "/d/" + sourceKey)
		obj.LastModified = s.now().UnixMilli()
		tx.Put(target+"/d/"+*in.Key, body)
		return writeJSON(tx, target+"/o/"+*in.Key, &obj)
	})
	if err != nil {
		return nil, err
	}

	s.notify(ctx, m, cfg, objectEvent{
		name:   "ObjectCreated:Copy",
		reason: "CopyObject",
		bucket: *in.Bucket,
		key:    *in.Key,
		etag:   obj.ETag,
		size:   obj.Size,
	})
	return &api.CopyObjectOutput{CopyObjectResult: &api.CopyObjectResult{
		ETag:         new(obj.ETag),
		LastModified: new(time.UnixMilli(obj.LastModified).UTC()),
	}}, nil
}

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
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	directiveCopy    = "COPY"
	directiveReplace = "REPLACE"
)

// copySource splits an x-amz-copy-source header, "<bucket>/<key>" with an
// optional leading slash, URL-encoded, and an optional "?versionId=".
func copySource(header *string) (bucket, key, version string, err error) {
	raw := strings.TrimPrefix(common.Value(header), "/")
	if path, query, hasQuery := strings.Cut(raw, "?"); hasQuery {
		values, parseErr := url.ParseQuery(query)
		if parseErr != nil || len(values) != 1 || len(values["versionId"]) != 1 || values.Get("versionId") == "" {
			return "", "", "", invalidArgument("Invalid copy source version")
		}

		raw, version = path, values.Get("versionId")
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", "", invalidArgument("Invalid copy source encoding")
	}

	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", "", invalidArgument(
			"Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}

	return bucket, key, version, nil
}

// directive validates a metadata or tagging directive, COPY by default.
func directive(p *string, name string) (string, error) {
	switch d := common.Value(p); d {
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
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	sourceBucket, sourceKey, sourceVersion, err := copySource(in.CopySource)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(&sourceKey); guardErr != nil {
		return nil, guardErr
	}

	if c := common.Value(in.StorageClass); c != "" && c != storageStandard {
		return nil, fail(
			http.StatusNotImplemented,
			"NotImplemented",
			"TinyStack only supports the STANDARD storage class",
		)
	}

	if sseErr := encryption(in.ServerSideEncryption); sseErr != nil {
		return nil, sseErr
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

	algorithm := common.Value(in.ChecksumAlgorithm)
	if algorithmErr := validAlgorithm(algorithm); algorithmErr != nil {
		return nil, algorithmErr
	}

	if sourceBucket == common.Value(in.Bucket) && sourceKey == common.Value(in.Key) && sourceVersion == "" &&
		metadataDirective == directiveCopy &&
		taggingDirective == directiveCopy && in.StorageClass == nil {
		return nil, fail(http.StatusBadRequest, "InvalidRequest", "This copy request is illegal because it is "+
			"trying to copy an object to itself without changing the object's metadata, storage class, website "+
			"redirect location or encryption attributes.")
	}

	var obj object
	var cfg *api.NotificationConfiguration
	var copied, written *string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		source, resolveErr := resolveSource(tx, m, sourceBucket, in.ExpectedSourceBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		target, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		sourceStatus, readErr := versioning(tx, source)
		if readErr != nil {
			return readErr
		}

		var data string
		if obj, data, readErr = locate(tx, source, sourceKey, sourceVersion); readErr != nil {
			return readErr
		}

		if copied = reported(sourceStatus, &obj); sourceVersion != "" {
			copied = &sourceVersion
		}

		if in.CopySourceIfMatch != nil && !matches(*in.CopySourceIfMatch, obj.ETag) ||
			in.CopySourceIfNoneMatch != nil && matches(*in.CopySourceIfNoneMatch, obj.ETag) {
			return preconditionFailed()
		}

		if metadataDirective == directiveReplace {
			obj.Metadata, obj.ContentType = maps.Clone(in.Metadata), common.Value(in.ContentType)
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

		body, _ := tx.Get(data)

		// The copy keeps the source's checksum unless a new algorithm is asked for or the
		// source has none, which S3 would have given the default.
		if algorithm != "" || obj.Checksum == nil {
			use := algorithm
			if use == "" {
				use = defaultChecksum
			}

			obj.Checksum = &checksum{Algorithm: use, Value: sum(use, body), Type: typeFullObject}
		}

		obj.LastModified = s.now().UnixMilli()
		status, putErr := putCurrent(tx, target, *in.Key, &obj, body)
		written = reported(status, &obj)
		return putErr
	})
	if err != nil {
		return nil, err
	}

	s.notify(ctx, m, cfg, objectEvent{
		name:    "ObjectCreated:Copy",
		reason:  "CopyObject",
		bucket:  *in.Bucket,
		key:     *in.Key,
		etag:    obj.ETag,
		version: common.Value(written),
		size:    obj.Size,
	})
	result := &api.CopyObjectResult{
		ETag:         new(obj.ETag),
		LastModified: new(time.UnixMilli(obj.LastModified).UTC()),
	}
	obj.Checksum.apply(result)

	return &api.CopyObjectOutput{
		CopyObjectResult:    result,
		VersionId:           written,
		CopySourceVersionId: copied,
	}, nil
}

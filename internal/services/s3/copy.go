package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
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

	var snapshot []byte
	var sourceObject object
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		source, resolveErr := resolveSource(tx, m, sourceBucket, in.ExpectedSourceBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		obj, data, readErr := locate(tx, source, sourceKey, sourceVersion)
		if readErr != nil {
			return readErr
		}

		sourceObject = obj
		snapshot, _ = tx.Get(data)
		if blob, ok := storage.BlobReference(snapshot); ok {
			s.blobs.Pin(blob)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	defer s.releaseBody(snapshot)
	var replacement *checksum
	if algorithm != "" || sourceObject.Checksum == nil {
		use := algorithm
		if use == "" {
			use = defaultChecksum
		}

		reader, openErr := s.openBody(snapshot)
		if openErr != nil {
			return nil, openErr
		}

		digest := checksums()[use]()
		_, hashErr := io.Copy(digest, reader)
		closeErr := reader.Close()
		if hashErr != nil {
			return nil, hashErr
		}

		if closeErr != nil {
			return nil, closeErr
		}

		replacement = &checksum{
			Algorithm: use,
			Value:     base64.StdEncoding.EncodeToString(digest.Sum(nil)),
			Type:      typeFullObject,
		}
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

		if guardErr := copyConditions(&obj, in.CopySourceIfMatch, in.CopySourceIfNoneMatch,
			in.CopySourceIfModifiedSince, in.CopySourceIfUnmodifiedSince); guardErr != nil {
			return guardErr
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

		targetMeta, _, readErr := storage.Read[bucketMeta](tx, target)
		if readErr != nil {
			return readErr
		}

		obj.ObjectLockMode = ""
		obj.ObjectLockRetainUntilDate = nil
		obj.ObjectLockLegalHoldStatus = ""
		lockErr := applyObjectLock(
			targetMeta,
			&obj,
			in.ObjectLockMode,
			in.ObjectLockRetainUntilDate,
			in.ObjectLockLegalHoldStatus,
			s.now(),
		)
		if lockErr != nil {
			return lockErr
		}

		var cfgErr error
		if cfg, cfgErr = notifications(tx, target); cfgErr != nil {
			return cfgErr
		}

		body, _ := tx.Get(data)

		if !bytes.Equal(body, snapshot) {
			return fail(409, "OperationAborted", "Source changed during copy; retry the request")
		}

		if replacement != nil {
			obj.Checksum = replacement
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

// copyConditions applies S3's ETag precedence over date conditions.
func copyConditions(obj *object, match, none *string, modified, unmodified *time.Time) error {
	changed := time.UnixMilli(obj.LastModified).UTC().Truncate(time.Second)
	if match != nil && !matches(*match, obj.ETag) || none != nil && matches(*none, obj.ETag) {
		return preconditionFailed()
	}

	if match == nil && unmodified != nil && changed.After(*unmodified) ||
		none == nil && modified != nil && !changed.After(*modified) {
		return preconditionFailed()
	}

	return nil
}

// copyRange requires an explicit inclusive interval; suffix and open ranges are not copy ranges.
func copyRange(spec string, size int64) (byteRange, error) {
	value, ok := strings.CutPrefix(spec, "bytes=")
	first, last, split := strings.Cut(value, "-")
	start, err1 := strconv.ParseInt(first, 10, 64)
	end, err2 := strconv.ParseInt(last, 10, 64)
	if !ok || !split || err1 != nil || err2 != nil || start < 0 || end < start || end >= size ||
		strings.HasPrefix(first, "+") || strings.HasPrefix(last, "+") {
		return byteRange{}, invalidArgument("The x-amz-copy-source-range value is invalid for the source object")
	}

	return byteRange{start: start, end: end}, nil
}

func (s *Service) UploadPartCopy(
	ctx context.Context,
	in *api.UploadPartCopyRequest,
) (*api.UploadPartCopyOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	n := common.Value(in.PartNumber)
	if n < 1 || n > 10000 {
		return nil, invalidArgument("Part number must be from 1 to 10000")
	}

	sourceBucket, sourceKey, version, err := copySource(in.CopySource)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(&sourceKey); guardErr != nil {
		return nil, guardErr
	}

	var snapshot []byte
	var obj object
	var copied *string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		target, targetErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if targetErr != nil {
			return targetErr
		}

		if _, uploadErr := loadUpload(tx, target, *in.Key, common.Value(in.UploadId)); uploadErr != nil {
			return uploadErr
		}

		source, transactionErr := resolveSource(tx, m, sourceBucket, in.ExpectedSourceBucketOwner)
		if transactionErr != nil {
			return transactionErr
		}

		var data string
		obj, data, transactionErr = locate(tx, source, sourceKey, version)
		if transactionErr != nil {
			return transactionErr
		}

		if guardErr := copyConditions(&obj, in.CopySourceIfMatch, in.CopySourceIfNoneMatch,
			in.CopySourceIfModifiedSince, in.CopySourceIfUnmodifiedSince); guardErr != nil {
			return guardErr
		}

		status, transactionErr := versioning(tx, source)
		if transactionErr != nil {
			return transactionErr
		}

		copied = reported(status, &obj)
		snapshot, _ = tx.Get(data)
		if blob, ok := storage.BlobReference(snapshot); ok {
			s.blobs.Pin(blob)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	defer s.releaseBody(snapshot)
	interval := byteRange{end: obj.Size - 1}
	if in.CopySourceRange != nil {
		interval, err = copyRange(*in.CopySourceRange, obj.Size)
		if err != nil {
			return nil, err
		}
	}

	size := interval.end - interval.start + 1
	if size > s.maxObjectBytes {
		return nil, fail(400, "EntityTooLarge", "Part exceeds configured limit")
	}

	reader, err := s.openBody(snapshot)
	if err != nil {
		return nil, err
	}

	defer func() { _ = reader.Close() }()
	if _, seekErr := reader.Seek(interval.start, io.SeekStart); seekErr != nil {
		return nil, seekErr
	}

	blob, digest, values, err := s.writeStream(io.LimitReader(reader, size))
	if err != nil {
		return nil, err
	}

	committed := false
	defer func() {
		if committed {
			s.blobs.Release(blob)
		} else {
			s.blobs.Discard(blob)
		}
	}()
	part := uploadPart{ETag: fmt.Sprintf(`"%x"`, digest), Size: blob.Size, LastModified: s.now().UnixMilli()}
	var sum checksum
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, transactionErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if transactionErr != nil {
			return transactionErr
		}

		u, transactionErr := loadUpload(tx, bucket, *in.Key, common.Value(in.UploadId))
		if transactionErr != nil {
			return transactionErr
		}

		algorithm, _ := u.kind()
		sum = checksum{Algorithm: algorithm, Value: values[algorithm]}
		part.Checksum = sum.Value
		tx.Put(partKey(bucket, *in.UploadId, n), blob.Record())
		return storage.Write(tx, partMetaKey(bucket, *in.UploadId, n), &part)
	})
	if err != nil {
		return nil, err
	}

	committed = true
	result := &api.CopyPartResult{ETag: new(part.ETag), LastModified: new(time.UnixMilli(part.LastModified).UTC())}
	sum.apply(result)
	return &api.UploadPartCopyOutput{CopyPartResult: result, CopySourceVersionId: copied}, nil
}

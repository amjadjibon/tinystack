package s3

import (
	"bytes"
	"context"
	"crypto/md5"  // #nosec G501 -- S3 defines MD5 ETags and Content-MD5; they are not a security control.
	"crypto/sha1" // #nosec G505 -- S3 defines SHA-1 object checksums; they are not a security control.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxKeyBytes      = 1024
	maxMetadataBytes = 2048
	maxListKeys      = 1000
	storageStandard  = "STANDARD"
	defaultType      = "binary/octet-stream"
	encodingURL      = "url"
)

// object is the stored metadata of one object; its bytes are a separate record.
type object struct {
	Metadata map[string]string `json:",omitempty"`
	Tags     map[string]string `json:",omitempty"`
	// Checksum is the stored checksum, absent on objects written before it was kept.
	Checksum *checksum `json:",omitempty"`
	// ObjectLockRetainUntilDate is when retention expires.
	ObjectLockRetainUntilDate *time.Time `json:",omitempty"`
	ETag                      string
	ContentType               string
	// VersionID is empty for the null version.
	VersionID string `json:",omitempty"`
	// ObjectLockMode is the lock mode: "GOVERNANCE" or "COMPLIANCE".
	ObjectLockMode string `json:",omitempty"`
	// ObjectLockLegalHoldStatus is the legal hold status: "ON" or "OFF".
	ObjectLockLegalHoldStatus string           `json:",omitempty"`
	Parts                     []api.ObjectPart `json:",omitempty"`
	Size                      int64
	LastModified              int64
	// DeleteMarker marks a delete marker, which only a history record holds.
	DeleteMarker bool `json:",omitempty"`
}

// crc64NVME is the CRC-64/NVME table S3 uses for CRC64NVME checksums.
var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

func checksums() map[string]func() hash.Hash {
	return map[string]func() hash.Hash{
		"CRC32":     func() hash.Hash { return crc32.NewIEEE() },
		"CRC32C":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
		"CRC64NVME": func() hash.Hash { return crc64.New(crc64NVME) },
		"SHA1":      sha1.New, // #nosec G401 -- S3 protocol checksum.
		"SHA256":    sha256.New,
	}
}

func noSuchKey() error { return awserror.New("NoSuchKey", "The specified key does not exist.") }

func preconditionFailed() error {
	return fail(http.StatusPreconditionFailed, "PreconditionFailed",
		"At least one of the pre-conditions you specified did not hold")
}

func validKey(key *string) error {
	if k := common.Value(key); k == "" || len(k) > maxKeyBytes || !utf8.ValidString(k) {
		return fail(http.StatusBadRequest, "KeyTooLongError", "Object keys must be 1 to 1024 bytes of UTF-8")
	}

	return nil
}

// verify checks the Content-MD5 the client sent.
func verify(in *api.PutObjectRequest, digest []byte) error {
	if in.ContentMD5 != nil {
		want, err := base64.StdEncoding.DecodeString(*in.ContentMD5)
		if err != nil || len(want) != md5.Size {
			return fail(http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified was invalid.")
		}

		if !bytes.Equal(digest, want) {
			return fail(http.StatusBadRequest, "BadDigest",
				"The Content-MD5 you specified did not match what we received.")
		}
	}

	return nil
}

// matches reports whether an If-Match or If-None-Match value names the ETag.
func matches(condition, etag string) bool {
	for _, c := range strings.Split(condition, ",") {
		if c = strings.TrimSpace(c); c == "*" || strings.Trim(c, `"`) == strings.Trim(etag, `"`) {
			return true
		}
	}

	return false
}

func (s *Service) PutObject(ctx context.Context, in *api.PutObjectRequest) (*api.PutObjectOutput, error) {
	payload := request.ResponsePayload(ctx)
	isStream := payload != nil && payload.Stream != nil

	if !isStream && int64(len(in.Body)) > s.maxObjectBytes {
		return nil, fail(400, "EntityTooLarge", "Object exceeds configured limit")
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
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

	if n := common.Value(in.IfNoneMatch); n != "" && n != "*" {
		return nil, fail(http.StatusNotImplemented, "NotImplemented", "If-None-Match only supports *")
	}

	metadataSize := 0
	for k, v := range in.Metadata {
		metadataSize += len(k) + len(v)
	}

	if metadataSize > maxMetadataBytes {
		return nil, fail(http.StatusBadRequest, "MetadataTooLarge",
			"Your metadata headers exceed the maximum allowed metadata size")
	}

	var (
		blob   storage.Blob
		digest []byte
		values map[string]string
	)
	if isStream {
		blob, digest, values, err = s.writeStream(payload.Stream)
	} else {
		blob, digest, values, err = s.writeBody(in.Body)
	}
	if err != nil {
		return nil, err
	}

	committed := false
	defer func() {
		if !committed {
			s.blobs.Discard(blob)
		} else {
			s.blobs.Release(blob)
		}
	}()

	if payload != nil && payload.Trailers != nil {
		applyTrailers(in, payload.Trailers())
	}

	if in.ContentLength != nil && *in.ContentLength != blob.Size {
		return nil, fail(http.StatusBadRequest, "IncompleteBody", "Content length does not match the uploaded body")
	}

	if guardErr := verify(in, digest); guardErr != nil {
		return nil, guardErr
	}

	stored, err := checksumOf(in, values, "")
	if err != nil {
		return nil, err
	}

	stored.Type = typeFullObject

	tags, err := headerTags(in.Tagging)
	if err != nil {
		return nil, err
	}

	obj := object{
		ETag:         `"` + hex.EncodeToString(digest) + `"`,
		ContentType:  common.Value(in.ContentType),
		Size:         blob.Size,
		LastModified: s.now().UnixMilli(),
		Metadata:     in.Metadata,
		Tags:         tags,
		Checksum:     &stored,
	}

	if obj.ContentType == "" {
		obj.ContentType = defaultType
	}

	var cfg *api.NotificationConfiguration
	var status string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, key)
		if readErr != nil {
			return readErr
		}

		if lockErr := applyObjectLock(
			meta,
			&obj,
			in.ObjectLockMode,
			in.ObjectLockRetainUntilDate,
			in.ObjectLockLegalHoldStatus,
			s.now(),
		); lockErr != nil {
			return lockErr
		}

		var cfgErr error
		if cfg, cfgErr = notifications(tx, key); cfgErr != nil {
			return cfgErr
		}

		var existing object
		found, readErr := readJSON(tx, key+"/o/"+*in.Key, &existing)
		switch {
		case readErr != nil:
			return readErr
		case in.IfNoneMatch != nil && found:
			return preconditionFailed()
		case in.IfMatch != nil && !found:
			return noSuchKey()
		case in.IfMatch != nil && !matches(*in.IfMatch, existing.ETag):
			return preconditionFailed()
		}

		var putErr error
		status, putErr = putCurrent(tx, key, *in.Key, &obj, blob.Record())
		return putErr
	})
	if err != nil {
		return nil, err
	}
	committed = true

	version := reported(status, &obj)
	s.notify(ctx, m, cfg, objectEvent{
		name:    "ObjectCreated:Put",
		reason:  "PutObject",
		bucket:  *in.Bucket,
		key:     *in.Key,
		etag:    obj.ETag,
		version: common.Value(version),
		size:    obj.Size,
	})
	out := &api.PutObjectOutput{ETag: new(obj.ETag), VersionId: version}
	obj.Checksum.apply(out)

	return out, nil
}

// target names an object, or one version of it, in a request.
type target struct {
	bucket, key, version, owner *string
}

// found is an object version read by a request.
type found struct {
	version *string // the version ID to report, if any
	body    []byte
	obj     object
}

// read loads the current object, or the named version, and applies If-Match
// and If-None-Match.
func (s *Service) read(ctx context.Context, t target, ifMatch, ifNoneMatch *string, withBody bool) (*found, error) {
	if s.blobErr != nil {
		return nil, s.blobErr
	}
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(t.key); guardErr != nil {
		return nil, guardErr
	}

	var f found
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix, resolveErr := resolve(tx, m, t.bucket, t.owner)
		if resolveErr != nil {
			return resolveErr
		}

		status, readErr := versioning(tx, prefix)
		if readErr != nil {
			return readErr
		}

		obj, data, readErr := locate(tx, prefix, *t.key, common.Value(t.version))
		if readErr != nil {
			return readErr
		}

		f.obj, f.version = obj, reported(status, &obj)
		if t.version != nil {
			f.version = t.version
		}

		if withBody {
			f.body, _ = tx.Get(data)
			if blob, ok := storage.BlobReference(f.body); ok {
				s.blobs.Pin(blob)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if ifMatch != nil && !matches(*ifMatch, f.obj.ETag) {
		s.releaseBody(f.body)
		return nil, preconditionFailed()
	}

	if ifNoneMatch != nil && matches(*ifNoneMatch, f.obj.ETag) {
		s.releaseBody(f.body)
		return nil, fail(http.StatusNotModified, "NotModified", "Not Modified")
	}

	return &f, nil
}

// Object returns the bytes of the current object, or of one version, in the
// caller's account and region, for services that read S3 in-process, such as
// Lambda deployment packages.
func (s *Service) Object(ctx context.Context, bucket, key, version string) ([]byte, error) {
	t := target{bucket: &bucket, key: &key}
	if version != "" {
		t.version = &version
	}

	f, err := s.read(ctx, t, nil, nil, true)
	if err != nil {
		return nil, err
	}

	defer s.releaseBody(f.body)
	reader, err := s.openBody(f.body)
	if err != nil {
		return nil, err
	}

	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

func (s *Service) GetObject(ctx context.Context, in *api.GetObjectRequest) (*api.GetObjectOutput, error) {
	f, err := s.read(ctx, target{in.Bucket, in.Key, in.VersionId, in.ExpectedBucketOwner}, in.IfMatch,
		in.IfNoneMatch, true)
	if err != nil {
		return nil, err
	}

	defer s.releaseBody(f.body)
	report, err := reporting(in.ChecksumMode)
	if err != nil {
		return nil, err
	}

	obj := &f.obj
	reader, err := s.openBody(f.body)
	if err != nil {
		return nil, err
	}

	transferred := false
	defer func() {
		if !transferred {
			_ = reader.Close()
		}
	}()
	out := &api.GetObjectOutput{
		VersionId:     f.version,
		ContentLength: new(obj.Size),
		ContentType:   new(obj.ContentType),
		ETag:          new(obj.ETag),
		LastModified:  new(time.UnixMilli(obj.LastModified).UTC()),
		Metadata:      obj.Metadata,
	}

	if obj.ObjectLockMode != "" {
		out.ObjectLockMode = new(obj.ObjectLockMode)
	}

	if obj.ObjectLockRetainUntilDate != nil {
		out.ObjectLockRetainUntilDate = obj.ObjectLockRetainUntilDate
	}

	if obj.ObjectLockLegalHoldStatus != "" {
		out.ObjectLockLegalHoldStatus = new(obj.ObjectLockLegalHoldStatus)
	}

	if len(obj.Tags) > 0 {
		out.TagCount = new(int64(len(obj.Tags)))
	}

	if in.Range != nil {
		interval, rangeErr := parseRange(*in.Range, obj.Size)
		if rangeErr != nil {
			return nil, rangeErr
		}

		if _, seekErr := reader.Seek(interval.start, io.SeekStart); seekErr != nil {
			return nil, seekErr
		}

		out.ContentLength = new(interval.end - interval.start + 1)
		out.ContentRange = new(fmt.Sprintf("bytes %d-%d/%d", interval.start, interval.end, obj.Size))
		out.AcceptRanges = new("bytes")
	} else if report {
		// S3 reports no full-object checksum for part of an object.
		obj.Checksum.apply(out)
	}

	if payload := request.ResponsePayload(ctx); payload != nil {
		payload.Body = &limitedBody{Reader: io.LimitReader(reader, *out.ContentLength), Closer: reader}
		transferred = true
	} else {
		out.Body, err = io.ReadAll(io.LimitReader(reader, *out.ContentLength))
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (s *Service) HeadObject(ctx context.Context, in *api.HeadObjectRequest) (*api.HeadObjectOutput, error) {
	f, err := s.read(ctx, target{in.Bucket, in.Key, in.VersionId, in.ExpectedBucketOwner}, in.IfMatch,
		in.IfNoneMatch, false)
	if err != nil {
		return nil, err
	}

	report, err := reporting(in.ChecksumMode)
	if err != nil {
		return nil, err
	}

	obj := &f.obj
	out := &api.HeadObjectOutput{
		VersionId:     f.version,
		ContentLength: new(obj.Size),
		ContentType:   new(obj.ContentType),
		ETag:          new(obj.ETag),
		LastModified:  new(time.UnixMilli(obj.LastModified).UTC()),
		Metadata:      obj.Metadata,
	}

	if obj.ObjectLockMode != "" {
		out.ObjectLockMode = new(obj.ObjectLockMode)
	}

	if obj.ObjectLockRetainUntilDate != nil {
		out.ObjectLockRetainUntilDate = obj.ObjectLockRetainUntilDate
	}

	if obj.ObjectLockLegalHoldStatus != "" {
		out.ObjectLockLegalHoldStatus = new(obj.ObjectLockLegalHoldStatus)
	}

	if in.Range != nil {
		interval, rangeErr := parseRange(*in.Range, obj.Size)
		if rangeErr != nil {
			return nil, rangeErr
		}

		out.ContentLength = new(interval.end - interval.start + 1)
		out.AcceptRanges = new("bytes")
	} else if report {
		obj.Checksum.apply(out)
	}

	return out, nil
}

// DeleteObject removes an object, or hides it behind a delete marker in a
// versioned bucket; naming a version deletes that version permanently.
// Deleting a missing key or version succeeds.
func (s *Service) DeleteObject(ctx context.Context, in *api.DeleteObjectRequest) (*api.DeleteObjectOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	var cfg *api.NotificationConfiguration
	var r removal
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		var deleteErr error
		if in.VersionId != nil {
			r, deleteErr = deleteVersion(
				tx,
				key,
				*in.Key,
				*in.VersionId,
				common.Value(in.BypassGovernanceRetention),
				s.now(),
			)
		} else {
			r, deleteErr = deleteCurrent(tx, key, *in.Key, s.now().UnixMilli())
		}

		// Only a deletion that removed something is notified.
		if deleteErr != nil || !r.removed {
			return deleteErr
		}

		cfg, deleteErr = notifications(tx, key)
		return deleteErr
	})
	if err != nil {
		return nil, err
	}

	e := objectEvent{
		name: "ObjectRemoved:Delete", reason: "DeleteObject", bucket: *in.Bucket, key: *in.Key, version: r.version,
	}

	if r.marker && in.VersionId == nil {
		e.name = "ObjectRemoved:DeleteMarkerCreated"
	}

	s.notify(ctx, m, cfg, e)
	out := &api.DeleteObjectOutput{}
	if r.version != "" {
		out.VersionId = &r.version
	}

	if r.marker {
		out.DeleteMarker = new(true)
	}

	return out, nil
}

// maxDeletes is how many objects one DeleteObjects request may name.
const maxDeletes = 1000

// DeleteObjects deletes each named object or version as DeleteObject does, in
// one transaction. A key that is not valid is reported in Errors; Quiet leaves
// the deleted objects out of the response.
func (s *Service) DeleteObjects(ctx context.Context, in *api.DeleteObjectsRequest) (*api.DeleteObjectsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.Delete == nil || len(in.Delete.Objects) == 0 || len(in.Delete.Objects) > maxDeletes {
		return nil, malformedXML()
	}

	var cfg *api.NotificationConfiguration
	var events []objectEvent
	out := &api.DeleteObjectsOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		now := s.now().UnixMilli()
		for _, o := range in.Delete.Objects {
			if keyErr := validKey(o.Key); keyErr != nil {
				e := awserror.From(keyErr)
				out.Errors = append(out.Errors, api.Error{
					Key: o.Key, VersionId: o.VersionId, Code: new(e.Code), Message: new(e.Message),
				})

				continue
			}

			var r removal
			var deleteErr error
			if o.VersionId != nil {
				r, deleteErr = deleteVersion(
					tx,
					key,
					*o.Key,
					*o.VersionId,
					common.Value(in.BypassGovernanceRetention),
					s.now(),
				)
			} else {
				r, deleteErr = deleteCurrent(tx, key, *o.Key, now)
			}

			if deleteErr != nil {
				if e := awserror.From(deleteErr); e.Code == "AccessDenied" {
					out.Errors = append(out.Errors, api.Error{
						Key: o.Key, VersionId: o.VersionId, Code: new(e.Code), Message: new(e.Message),
					})

					continue
				}

				return deleteErr
			}

			deleted := api.DeletedObject{Key: o.Key, VersionId: o.VersionId}
			if r.marker {
				deleted.DeleteMarker = new(true)
				if o.VersionId == nil {
					deleted.DeleteMarkerVersionId = new(r.version)
				}
			}

			if !common.Value(in.Delete.Quiet) {
				out.Deleted = append(out.Deleted, deleted)
			}

			if r.removed {
				e := objectEvent{
					name: "ObjectRemoved:Delete", reason: "DeleteObjects", bucket: *in.Bucket,
					key: *o.Key, version: r.version,
				}

				if r.marker && o.VersionId == nil {
					e.name = "ObjectRemoved:DeleteMarkerCreated"
				}

				events = append(events, e)
			}
		}

		if len(events) == 0 {
			return nil
		}

		var readErr error
		cfg, readErr = notifications(tx, key)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	for _, e := range events {
		s.notify(ctx, m, cfg, e)
	}

	return out, nil
}

// GetBucketLocation returns the bucket's Region, which is empty for us-east-1.
// Clients ask it to find a bucket's Region, so it answers from any Region.
func (s *Service) GetBucketLocation(
	ctx context.Context,
	in *api.GetBucketLocationRequest,
) (*api.GetBucketLocationOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var index bucketIndex
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		found, readErr := readJSON(tx, indexPrefix+common.Value(in.Bucket), &index)
		switch {
		case readErr != nil:
			return readErr
		case !found:
			return noSuchBucket()
		case index.Account != m.AccountID || (in.ExpectedBucketOwner != nil && *in.ExpectedBucketOwner != m.AccountID):
			return accessDenied()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetBucketLocationOutput{}
	if index.Region != defaultRegion {
		out.LocationConstraint = new(index.Region)
	}

	return out, nil
}

// listing is one ListObjectsV2 page before encoding.
type listing struct {
	last     string
	contents []api.Object
	prefixes []string
	more     bool
}

// page walks keys in order after start, grouping keys that share a prefix up
// to the delimiter into common prefixes, until limit entries are collected.
func page(keys []string, prefix, delimiter, start string, limit int64) listing {
	var l listing
	seen := map[string]bool{}
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) || key <= start {
			continue
		}
		// A common prefix returned on an earlier page covers its keys.
		if delimiter != "" && strings.HasSuffix(start, delimiter) && strings.HasPrefix(key, start) {
			continue
		}

		entry, rolled := key, false
		if i := strings.Index(key[len(prefix):], delimiter); delimiter != "" && i >= 0 {
			entry, rolled = key[:len(prefix)+i+len(delimiter)], true
		}

		if seen[entry] || entry <= start {
			continue
		}

		if int64(len(l.contents)+len(l.prefixes)) == limit {
			l.more = true
			break
		}

		seen[entry] = true
		l.last = entry
		if rolled {
			l.prefixes = append(l.prefixes, entry)
			continue
		}

		l.contents = append(l.contents, api.Object{Key: new(key), StorageClass: new(storageStandard)})
	}

	return l
}

func (s *Service) ListObjectsV2(ctx context.Context, in *api.ListObjectsV2Request) (*api.ListObjectsV2Output, error) {
	limit := int64(maxListKeys)
	if in.MaxKeys != nil {
		if *in.MaxKeys < 0 {
			return nil, fail(http.StatusBadRequest, "InvalidArgument", "max-keys must not be negative")
		}

		limit = min(*in.MaxKeys, maxListKeys)
	}

	encoding := common.Value(in.EncodingType)
	if encoding != "" && encoding != encodingURL {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Invalid Encoding Method specified in Request")
	}

	start := common.Value(in.StartAfter)
	if in.ContinuationToken != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*in.ContinuationToken)
		if decodeErr != nil || len(b) == 0 {
			return nil, fail(http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")
		}

		start = string(b)
	}

	l, err := s.listObjects(ctx, in.Bucket, in.ExpectedBucketOwner,
		common.Value(in.Prefix), common.Value(in.Delimiter), start, limit)
	if err != nil {
		return nil, err
	}

	encode := func(s string) *string {
		if encoding == "url" {
			s = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
		}

		return new(s)
	}

	out := &api.ListObjectsV2Output{
		Name:              in.Bucket,
		Prefix:            encode(common.Value(in.Prefix)),
		MaxKeys:           new(limit),
		KeyCount:          new(int64(len(l.contents) + len(l.prefixes))),
		IsTruncated:       new(l.more),
		ContinuationToken: in.ContinuationToken,
		EncodingType:      in.EncodingType,
	}

	if in.Delimiter != nil {
		out.Delimiter = encode(*in.Delimiter)
	}

	if in.StartAfter != nil {
		out.StartAfter = encode(*in.StartAfter)
	}

	if l.more {
		out.NextContinuationToken = new(base64.RawURLEncoding.EncodeToString([]byte(l.last)))
	}

	for i := range l.contents {
		l.contents[i].Key = encode(*l.contents[i].Key)
	}

	out.Contents = l.contents
	for _, p := range l.prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, api.CommonPrefix{Prefix: encode(p)})
	}

	return out, nil
}

// encryption checks a write's server-side encryption. S3 encrypts every
// object with AES256 keys it manages; aws:kms is accepted because a bucket
// whose default encryption is KMS, such as the CDK bootstrap bucket, makes
// clients ask for it, but the object is stored as for AES256 and no KMS key is used.
func encryption(sse *string) error {
	if v := common.Value(sse); v != "" && v != sseS3 && v != "aws:kms" {
		return fail(
			http.StatusNotImplemented,
			"NotImplemented",
			"TinyStack only supports AES256 and aws:kms server-side encryption",
		)
	}

	return nil
}

// listObjects selects the page before decoding metadata, including only the requested prefix.
func (s *Service) listObjects(ctx context.Context, bucket, owner *string,
	prefix, delimiter, start string, limit int64,
) (listing, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return listing{}, err
	}

	var l listing
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		base, transactionErr := resolve(tx, m, bucket, owner)
		if transactionErr != nil {
			return transactionErr
		}

		if limit == 0 {
			return nil
		}

		records := tx.Keys(base + "/o/" + prefix)
		keys := make([]string, len(records))
		for i, key := range records {
			keys[i] = strings.TrimPrefix(key, base+"/o/")
		}

		l = page(keys, prefix, delimiter, start, limit)
		for i := range l.contents {
			entry := &l.contents[i]
			obj, _, transactionErr := storage.Read[object](tx, base+"/o/"+*entry.Key)
			if transactionErr != nil {
				return transactionErr
			}

			entry.LastModified = new(time.UnixMilli(obj.LastModified).UTC())
			entry.ETag, entry.Size = new(obj.ETag), new(obj.Size)
		}

		return nil
	})
	return l, err
}

func (s *Service) ListObjects(ctx context.Context, in *api.ListObjectsRequest) (*api.ListObjectsOutput, error) {
	// V1 and V2 use the same page selection, validation and encoding.
	out, err := s.ListObjectsV2(ctx, &api.ListObjectsV2Request{
		Bucket: in.Bucket, ExpectedBucketOwner: in.ExpectedBucketOwner, Prefix: in.Prefix,
		Delimiter: in.Delimiter, StartAfter: in.Marker, MaxKeys: in.MaxKeys, EncodingType: in.EncodingType,
	})
	if err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	owner := m.AccountID
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		base, transactionErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if transactionErr != nil {
			return transactionErr
		}

		owner, _, _ = strings.Cut(base, "/")
		return nil
	})
	if err != nil {
		return nil, err
	}

	for i := range out.Contents {
		out.Contents[i].Owner = &api.Owner{ID: new(canonicalID(owner))}
	}

	result := &api.ListObjectsOutput{
		Name: out.Name, Prefix: out.Prefix, Delimiter: out.Delimiter, MaxKeys: out.MaxKeys,
		EncodingType: out.EncodingType, Marker: out.StartAfter, IsTruncated: out.IsTruncated,
		Contents: out.Contents, CommonPrefixes: out.CommonPrefixes,
	}
	if common.Value(in.Delimiter) != "" && common.Value(out.IsTruncated) {
		raw, _ := base64.RawURLEncoding.DecodeString(common.Value(out.NextContinuationToken))
		marker := string(raw)
		if common.Value(in.EncodingType) == encodingURL {
			marker = strings.ReplaceAll(url.QueryEscape(marker), "+", "%20")
		}

		result.NextMarker = new(marker)
	}

	return result, nil
}

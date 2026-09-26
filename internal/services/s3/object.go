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
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
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
	Metadata    map[string]string `json:",omitempty"`
	Tags        map[string]string `json:",omitempty"`
	ETag        string
	ContentType string
	// VersionID is empty for the null version.
	VersionID    string `json:",omitempty"`
	Size         int64
	LastModified int64
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
	if k := value(key); k == "" || len(k) > maxKeyBytes || !utf8.ValidString(k) {
		return fail(http.StatusBadRequest, "KeyTooLongError", "Object keys must be 1 to 1024 bytes of UTF-8")
	}

	return nil
}

// verify checks Content-MD5 and any flexible checksums the client sent.
func verify(in *api.PutObjectRequest) error {
	if in.ContentMD5 != nil {
		want, err := base64.StdEncoding.DecodeString(*in.ContentMD5)
		if err != nil || len(want) != md5.Size {
			return fail(http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified was invalid.")
		}

		if got := md5.Sum(in.Body); !bytes.Equal(got[:], want) { // #nosec G401 -- S3 protocol digest.
			return fail(http.StatusBadRequest, "BadDigest",
				"The Content-MD5 you specified did not match what we received.")
		}
	}

	if a := value(in.ChecksumAlgorithm); a != "" && checksums()[a] == nil {
		return fail(http.StatusBadRequest, "InvalidRequest", "Checksum algorithm "+a+" is not supported")
	}

	for name, sent := range map[string]*string{
		"CRC32":     in.ChecksumCRC32,
		"CRC32C":    in.ChecksumCRC32C,
		"CRC64NVME": in.ChecksumCRC64NVME,
		"SHA1":      in.ChecksumSHA1,
		"SHA256":    in.ChecksumSHA256,
	} {
		if sent == nil {
			continue
		}

		h := checksums()[name]()
		_, _ = h.Write(in.Body)
		if base64.StdEncoding.EncodeToString(h.Sum(nil)) != *sent {
			return fail(http.StatusBadRequest, "BadDigest",
				"The "+name+" you specified did not match the calculated checksum.")
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
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	if c := value(in.StorageClass); c != "" && c != storageStandard {
		return nil, fail(
			http.StatusNotImplemented,
			"NotImplemented",
			"TinyStack only supports the STANDARD storage class",
		)
	}

	if n := value(in.IfNoneMatch); n != "" && n != "*" {
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

	if guardErr := verify(in); guardErr != nil {
		return nil, guardErr
	}

	tags, err := headerTags(in.Tagging)
	if err != nil {
		return nil, err
	}

	sum := md5.Sum(in.Body) // #nosec G401 -- S3 ETags are MD5 digests.
	obj := object{
		ETag:         `"` + hex.EncodeToString(sum[:]) + `"`,
		ContentType:  value(in.ContentType),
		Size:         int64(len(in.Body)),
		LastModified: s.now().UnixMilli(),
		Metadata:     in.Metadata,
		Tags:         tags,
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
		status, putErr = putCurrent(tx, key, *in.Key, &obj, in.Body)
		return putErr
	})
	if err != nil {
		return nil, err
	}

	version := reported(status, &obj)
	s.notify(ctx, m, cfg, objectEvent{
		name:    "ObjectCreated:Put",
		reason:  "PutObject",
		bucket:  *in.Bucket,
		key:     *in.Key,
		etag:    obj.ETag,
		version: value(version),
		size:    obj.Size,
	})
	return &api.PutObjectOutput{ETag: new(obj.ETag), VersionId: version}, nil
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
	m, err := identity(ctx)
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

		obj, data, readErr := locate(tx, prefix, *t.key, value(t.version))
		if readErr != nil {
			return readErr
		}

		f.obj, f.version = obj, reported(status, &obj)
		if t.version != nil {
			f.version = t.version
		}

		if withBody {
			f.body, _ = tx.Get(data)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if ifMatch != nil && !matches(*ifMatch, f.obj.ETag) {
		return nil, preconditionFailed()
	}

	if ifNoneMatch != nil && matches(*ifNoneMatch, f.obj.ETag) {
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

	return f.body, nil
}

func (s *Service) GetObject(ctx context.Context, in *api.GetObjectRequest) (*api.GetObjectOutput, error) {
	f, err := s.read(ctx, target{in.Bucket, in.Key, in.VersionId, in.ExpectedBucketOwner}, in.IfMatch,
		in.IfNoneMatch, true)
	if err != nil {
		return nil, err
	}

	obj, body := &f.obj, f.body
	out := &api.GetObjectOutput{
		Body:          body,
		VersionId:     f.version,
		ContentLength: new(obj.Size),
		ContentType:   new(obj.ContentType),
		ETag:          new(obj.ETag),
		LastModified:  new(time.UnixMilli(obj.LastModified).UTC()),
		Metadata:      obj.Metadata,
	}
	if len(obj.Tags) > 0 {
		out.TagCount = new(int64(len(obj.Tags)))
	}

	if in.Range != nil {
		interval, rangeErr := parseRange(*in.Range, obj.Size)
		if rangeErr != nil {
			return nil, rangeErr
		}

		out.Body = body[interval.start : interval.end+1]
		out.ContentLength = new(interval.end - interval.start + 1)
		out.ContentRange = new(fmt.Sprintf("bytes %d-%d/%d", interval.start, interval.end, obj.Size))
		out.AcceptRanges = new("bytes")
	}

	return out, nil
}

func (s *Service) HeadObject(ctx context.Context, in *api.HeadObjectRequest) (*api.HeadObjectOutput, error) {
	f, err := s.read(ctx, target{in.Bucket, in.Key, in.VersionId, in.ExpectedBucketOwner}, in.IfMatch,
		in.IfNoneMatch, false)
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
	if in.Range != nil {
		interval, rangeErr := parseRange(*in.Range, obj.Size)
		if rangeErr != nil {
			return nil, rangeErr
		}

		out.ContentLength = new(interval.end - interval.start + 1)
		out.AcceptRanges = new("bytes")
	}

	return out, nil
}

// DeleteObject removes an object, or hides it behind a delete marker in a
// versioned bucket; naming a version deletes that version permanently.
// Deleting a missing key or version succeeds.
func (s *Service) DeleteObject(ctx context.Context, in *api.DeleteObjectRequest) (*api.DeleteObjectOutput, error) {
	m, err := identity(ctx)
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
			r, deleteErr = deleteVersion(tx, key, *in.Key, *in.VersionId)
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

// listing is one ListObjectsV2 page before encoding.
type listing struct {
	last     string
	contents []api.Object
	prefixes []string
	more     bool
}

// page walks keys in order after start, grouping keys that share a prefix up
// to the delimiter into common prefixes, until limit entries are collected.
func page(keys []string, objects map[string]object, prefix, delimiter, start string, limit int64) listing {
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

		entry, common := key, false
		if i := strings.Index(key[len(prefix):], delimiter); delimiter != "" && i >= 0 {
			entry, common = key[:len(prefix)+i+len(delimiter)], true
		}

		if seen[entry] {
			continue
		}

		if int64(len(l.contents)+len(l.prefixes)) == limit {
			l.more = true
			break
		}

		seen[entry] = true
		l.last = entry
		if common {
			l.prefixes = append(l.prefixes, entry)
			continue
		}

		obj := objects[key]
		l.contents = append(l.contents, api.Object{
			Key:          new(key),
			LastModified: new(time.UnixMilli(obj.LastModified).UTC()),
			ETag:         new(obj.ETag),
			Size:         new(obj.Size),
			StorageClass: new(storageStandard),
		})
	}

	return l
}

func (s *Service) ListObjectsV2(ctx context.Context, in *api.ListObjectsV2Request) (*api.ListObjectsV2Output, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(maxListKeys)
	if in.MaxKeys != nil {
		if *in.MaxKeys < 0 {
			return nil, fail(http.StatusBadRequest, "InvalidArgument", "max-keys must not be negative")
		}

		limit = min(*in.MaxKeys, maxListKeys)
	}

	encoding := value(in.EncodingType)
	if encoding != "" && encoding != encodingURL {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Invalid Encoding Method specified in Request")
	}

	start := value(in.StartAfter)
	if in.ContinuationToken != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*in.ContinuationToken)
		if decodeErr != nil || len(b) == 0 {
			return nil, fail(http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")
		}

		start = string(b)
	}

	objects := map[string]object{}
	var keys []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		for _, k := range tx.Keys(prefix + "/o/" + value(in.Prefix)) {
			name := strings.TrimPrefix(k, prefix+"/o/")

			var obj object
			if _, readErr := readJSON(tx, k, &obj); readErr != nil {
				return readErr
			}

			objects[name] = obj
			keys = append(keys, name)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	l := page(keys, objects, value(in.Prefix), value(in.Delimiter), start, limit)
	encode := func(s string) *string {
		if encoding == "url" {
			s = url.QueryEscape(s)
		}

		return new(s)
	}

	out := &api.ListObjectsV2Output{
		Name:              in.Bucket,
		Prefix:            encode(value(in.Prefix)),
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

package s3

import (
	"bytes"
	"context"
	"crypto/md5"  // #nosec G501 -- S3 defines MD5 ETags and Content-MD5; they are not a security control.
	"crypto/sha1" // #nosec G505 -- S3 defines SHA-1 object checksums; they are not a security control.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"net/http"
	"net/url"
	"sort"
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
)

// object is the stored metadata of one object; its bytes are a separate record.
type object struct {
	Metadata     map[string]string `json:",omitempty"`
	ETag         string
	ContentType  string
	Size         int64
	LastModified int64
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

	sum := md5.Sum(in.Body) // #nosec G401 -- S3 ETags are MD5 digests.
	obj := object{
		ETag:         `"` + hex.EncodeToString(sum[:]) + `"`,
		ContentType:  value(in.ContentType),
		Size:         int64(len(in.Body)),
		LastModified: s.now().UnixMilli(),
		Metadata:     in.Metadata,
	}

	if obj.ContentType == "" {
		obj.ContentType = defaultType
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
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

		tx.Put(key+"/d/"+*in.Key, in.Body)
		return writeJSON(tx, key+"/o/"+*in.Key, &obj)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutObjectOutput{ETag: ptr(obj.ETag)}, nil
}

// read loads an object and applies If-Match and If-None-Match.
func (s *Service) read(
	ctx context.Context,
	bucket, key, ifMatch, ifNoneMatch, owner *string,
	withBody bool,
) (*object, []byte, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, nil, err
	}

	if guardErr := validKey(key); guardErr != nil {
		return nil, nil, guardErr
	}

	var obj object
	var body []byte
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix, resolveErr := resolve(tx, m, bucket, owner)
		if resolveErr != nil {
			return resolveErr
		}

		found, readErr := readJSON(tx, prefix+"/o/"+*key, &obj)
		if readErr != nil {
			return readErr
		}

		if !found {
			return noSuchKey()
		}

		if withBody {
			body, _ = tx.Get(prefix + "/d/" + *key)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	if ifMatch != nil && !matches(*ifMatch, obj.ETag) {
		return nil, nil, preconditionFailed()
	}

	if ifNoneMatch != nil && matches(*ifNoneMatch, obj.ETag) {
		return nil, nil, fail(http.StatusNotModified, "NotModified", "Not Modified")
	}

	return &obj, body, nil
}

func (s *Service) GetObject(ctx context.Context, in *api.GetObjectRequest) (*api.GetObjectOutput, error) {
	obj, body, err := s.read(ctx, in.Bucket, in.Key, in.IfMatch, in.IfNoneMatch, in.ExpectedBucketOwner, true)
	if err != nil {
		return nil, err
	}

	return &api.GetObjectOutput{
		Body:          body,
		ContentLength: ptr(obj.Size),
		ContentType:   ptr(obj.ContentType),
		ETag:          ptr(obj.ETag),
		LastModified:  ptr(time.UnixMilli(obj.LastModified).UTC()),
		Metadata:      obj.Metadata,
	}, nil
}

func (s *Service) HeadObject(ctx context.Context, in *api.HeadObjectRequest) (*api.HeadObjectOutput, error) {
	obj, _, err := s.read(ctx, in.Bucket, in.Key, in.IfMatch, in.IfNoneMatch, in.ExpectedBucketOwner, false)
	if err != nil {
		return nil, err
	}

	return &api.HeadObjectOutput{
		ContentLength: ptr(obj.Size),
		ContentType:   ptr(obj.ContentType),
		ETag:          ptr(obj.ETag),
		LastModified:  ptr(time.UnixMilli(obj.LastModified).UTC()),
		Metadata:      obj.Metadata,
	}, nil
}

// DeleteObject removes an object; deleting a missing key succeeds.
func (s *Service) DeleteObject(ctx context.Context, in *api.DeleteObjectRequest) (*api.DeleteObjectOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		tx.Delete(key + "/o/" + *in.Key)
		tx.Delete(key + "/d/" + *in.Key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteObjectOutput{}, nil
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
			Key:          ptr(key),
			LastModified: ptr(time.UnixMilli(obj.LastModified).UTC()),
			ETag:         ptr(obj.ETag),
			Size:         ptr(obj.Size),
			StorageClass: ptr(storageStandard),
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
	if encoding != "" && encoding != "url" {
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

		for _, k := range tx.Keys() {
			name, ok := strings.CutPrefix(k, prefix+"/o/")
			if !ok || !strings.HasPrefix(name, value(in.Prefix)) {
				continue
			}

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

	sort.Strings(keys)
	l := page(keys, objects, value(in.Prefix), value(in.Delimiter), start, limit)
	encode := func(s string) *string {
		if encoding == "url" {
			s = url.QueryEscape(s)
		}

		return ptr(s)
	}

	out := &api.ListObjectsV2Output{
		Name:              in.Bucket,
		Prefix:            encode(value(in.Prefix)),
		MaxKeys:           ptr(limit),
		KeyCount:          ptr(int64(len(l.contents) + len(l.prefixes))),
		IsTruncated:       ptr(l.more),
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
		out.NextContinuationToken = ptr(base64.RawURLEncoding.EncodeToString([]byte(l.last)))
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

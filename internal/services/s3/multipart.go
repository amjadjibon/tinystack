package s3

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 defines MD5-based multipart ETags.
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const minMultipartPart = 5 << 20

type upload struct {
	Metadata    map[string]string
	Key         string
	ContentType string
	Initiated   int64
}

type uploadPart struct {
	ETag         string
	Size         int64
	LastModified int64
}

func noSuchUpload() error {
	return fail(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
}

func uploadKey(bucket, id string) string { return bucket + "/u/" + id }
func partKey(bucket, id string, n int64) string {
	return fmt.Sprintf("%s/p/%s/%05d", bucket, id, n)
}

func partMetaKey(bucket, id string, n int64) string {
	return fmt.Sprintf("%s/pm/%s/%05d", bucket, id, n)
}

func loadUpload(tx storage.ReadTx, bucket, key, id string) (upload, error) {
	var u upload
	if id == "" {
		return u, noSuchUpload()
	}
	found, err := readJSON(tx, uploadKey(bucket, id), &u)
	if err != nil {
		return u, err
	}
	if !found || u.Key != key {
		return u, noSuchUpload()
	}
	return u, nil
}

func removeUpload(tx storage.WriteTx, bucket, id string) {
	tx.Delete(uploadKey(bucket, id))
	for _, k := range tx.Keys() {
		if strings.HasPrefix(k, bucket+"/p/"+id+"/") || strings.HasPrefix(k, bucket+"/pm/"+id+"/") {
			tx.Delete(k)
		}
	}
}

func (s *Service) CreateMultipartUpload(
	ctx context.Context,
	in *api.CreateMultipartUploadRequest,
) (*api.CreateMultipartUploadOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}
	if c := value(in.StorageClass); c != "" && c != storageStandard {
		return nil, fail(http.StatusNotImplemented, "NotImplemented", "Only STANDARD storage is supported")
	}
	metadataSize := 0
	for k, v := range in.Metadata {
		metadataSize += len(k) + len(v)
	}
	if metadataSize > maxMetadataBytes {
		return nil, fail(http.StatusBadRequest, "MetadataTooLarge", "Metadata exceeds the supported limit")
	}
	idBytes := make([]byte, 24)
	if _, err = rand.Read(idBytes); err != nil {
		return nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	u := upload{Key: *in.Key, ContentType: value(in.ContentType), Metadata: in.Metadata, Initiated: s.now().UnixMilli()}
	if u.ContentType == "" {
		u.ContentType = defaultType
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}
		return writeJSON(tx, uploadKey(bucket, id), &u)
	})
	if err != nil {
		return nil, err
	}
	return &api.CreateMultipartUploadOutput{Bucket: in.Bucket, Key: in.Key, UploadId: &id}, nil
}

func (s *Service) UploadPart(ctx context.Context, in *api.UploadPartRequest) (*api.UploadPartOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}
	n := value(in.PartNumber)
	if n < 1 || n > 10000 {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Part number must be from 1 to 10000")
	}
	if in.ContentLength != nil && *in.ContentLength != int64(len(in.Body)) {
		return nil, fail(http.StatusBadRequest, "IncompleteBody", "Content length does not match the uploaded part")
	}
	if in.ContentMD5 != nil {
		want, decodeErr := base64.StdEncoding.DecodeString(*in.ContentMD5)
		if decodeErr != nil || len(want) != md5.Size {
			return nil, fail(400, "InvalidDigest", "Invalid Content-MD5")
		}
		got := md5.Sum(in.Body) // #nosec G401 -- S3 protocol digest.
		if !bytes.Equal(want, got[:]) {
			return nil, fail(400, "BadDigest", "Content-MD5 does not match")
		}
	}
	sum := md5.Sum(in.Body) // #nosec G401 -- S3 part ETag.
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	p := uploadPart{ETag: etag, Size: int64(len(in.Body)), LastModified: s.now().UnixMilli()}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}
		if _, loadErr := loadUpload(tx, bucket, *in.Key, value(in.UploadId)); loadErr != nil {
			return loadErr
		}
		tx.Put(partKey(bucket, *in.UploadId, n), in.Body)
		return writeJSON(tx, partMetaKey(bucket, *in.UploadId, n), &p)
	})
	if err != nil {
		return nil, err
	}
	return &api.UploadPartOutput{ETag: &etag}, nil
}

func (s *Service) CompleteMultipartUpload(
	ctx context.Context,
	in *api.CompleteMultipartUploadRequest,
) (*api.CompleteMultipartUploadOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}
	if in.MultipartUpload == nil || len(in.MultipartUpload.Parts) == 0 {
		return nil, fail(400, "MalformedXML", "Completion requires at least one part")
	}
	if len(in.MultipartUpload.Parts) > 10000 {
		return nil, fail(400, "InvalidPart", "Too many parts")
	}
	lastNumber := int64(0)
	for _, requested := range in.MultipartUpload.Parts {
		n := value(requested.PartNumber)
		if n <= lastNumber || n > 10000 {
			return nil, fail(400, "InvalidPartOrder", "Parts must be in ascending order")
		}
		lastNumber = n
	}
	var etag string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}
		u, loadErr := loadUpload(tx, bucket, *in.Key, value(in.UploadId))
		if loadErr != nil {
			return loadErr
		}
		var body, digests []byte
		last := int64(0)
		for i, requested := range in.MultipartUpload.Parts {
			n := value(requested.PartNumber)
			if n <= last || n > 10000 {
				return fail(400, "InvalidPartOrder", "Parts must be in ascending order")
			}
			last = n
			var p uploadPart
			found, readErr := readJSON(tx, partMetaKey(bucket, *in.UploadId, n), &p)
			if readErr != nil {
				return readErr
			}
			if !found || requested.ETag == nil ||
				strings.Trim(*requested.ETag, `"`) != strings.Trim(p.ETag, `"`) {
				return fail(400, "InvalidPart", "A part is missing or its ETag does not match")
			}
			if i < len(in.MultipartUpload.Parts)-1 && p.Size < minMultipartPart {
				return fail(400, "EntityTooSmall", "Every nonfinal part must be at least 5 MiB")
			}
			if int64(len(body))+p.Size > restMultipartLimit {
				return fail(400, "EntityTooLarge", "Completed object exceeds the 64 MiB limit")
			}
			part, ok := tx.Get(partKey(bucket, *in.UploadId, n))
			if !ok || int64(len(part)) != p.Size {
				return fail(400, "InvalidPart", "Part data is missing")
			}
			body = append(body, part...)
			digest, decodeErr := hex.DecodeString(strings.Trim(p.ETag, `"`))
			if decodeErr != nil {
				return decodeErr
			}
			digests = append(digests, digest...)
		}
		var existing object
		found, readErr := readJSON(tx, bucket+"/o/"+*in.Key, &existing)
		if readErr != nil {
			return readErr
		}
		if in.IfNoneMatch != nil && matches(*in.IfNoneMatch, existing.ETag) && found {
			return preconditionFailed()
		}
		if in.IfMatch != nil && (!found || !matches(*in.IfMatch, existing.ETag)) {
			return preconditionFailed()
		}
		finalSum := md5.Sum(digests) // #nosec G401 -- S3 multipart ETag.
		etag = fmt.Sprintf(`"%x-%d"`, finalSum, len(in.MultipartUpload.Parts))
		obj := object{
			ETag: etag, ContentType: u.ContentType, Metadata: u.Metadata,
			Size: int64(len(body)), LastModified: s.now().UnixMilli(),
		}
		tx.Put(bucket+"/d/"+*in.Key, body)
		if writeErr := writeJSON(tx, bucket+"/o/"+*in.Key, &obj); writeErr != nil {
			return writeErr
		}
		removeUpload(tx, bucket, *in.UploadId)
		return nil
	})
	if err != nil {
		return nil, err
	}
	location := "/" + *in.Bucket + "/" + url.PathEscape(*in.Key)
	return &api.CompleteMultipartUploadOutput{Location: &location, Bucket: in.Bucket, Key: in.Key, ETag: &etag}, nil
}

const restMultipartLimit = 64 << 20

func (s *Service) AbortMultipartUpload(
	ctx context.Context,
	in *api.AbortMultipartUploadRequest,
) (*api.AbortMultipartUploadOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}
		if _, loadErr := loadUpload(tx, bucket, *in.Key, value(in.UploadId)); loadErr != nil {
			return loadErr
		}
		removeUpload(tx, bucket, *in.UploadId)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &api.AbortMultipartUploadOutput{}, nil
}

func (s *Service) ListParts(ctx context.Context, in *api.ListPartsRequest) (*api.ListPartsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}
	limit := int64(1000)
	if in.MaxParts != nil {
		limit = *in.MaxParts
	}
	if limit < 0 || limit > 1000 {
		return nil, fail(400, "InvalidArgument", "max-parts must be from 0 to 1000")
	}
	marker := int64(0)
	if in.PartNumberMarker != nil {
		marker, err = strconv.ParseInt(*in.PartNumberMarker, 10, 64)
		if err != nil || marker < 0 {
			return nil, fail(400, "InvalidArgument", "Invalid part-number-marker")
		}
	}
	out := &api.ListPartsOutput{
		Bucket: in.Bucket, Key: in.Key, UploadId: in.UploadId, MaxParts: &limit,
		IsTruncated: new(false), PartNumberMarker: new(strconv.FormatInt(marker, 10)),
		StorageClass: new(storageStandard),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}
		if _, loadErr := loadUpload(tx, bucket, *in.Key, value(in.UploadId)); loadErr != nil {
			return loadErr
		}
		var nums []int64
		for _, k := range tx.Keys() {
			if part, ok := strings.CutPrefix(k, bucket+"/pm/"+*in.UploadId+"/"); ok {
				n, parseErr := strconv.ParseInt(part, 10, 64)
				if parseErr == nil && n > marker {
					nums = append(nums, n)
				}
			}
		}
		slices.Sort(nums)
		if int64(len(nums)) > limit {
			*out.IsTruncated = true
			nums = nums[:limit]
		}
		for _, n := range nums {
			var p uploadPart
			if _, readErr := readJSON(tx, partMetaKey(bucket, *in.UploadId, n), &p); readErr != nil {
				return readErr
			}
			out.Parts = append(out.Parts, api.Part{
				PartNumber: &n, ETag: &p.ETag, Size: &p.Size,
				LastModified: new(time.UnixMilli(p.LastModified).UTC()),
			})
		}
		if *out.IsTruncated && len(nums) > 0 {
			out.NextPartNumberMarker = new(strconv.FormatInt(nums[len(nums)-1], 10))
		}
		return nil
	})
	return out, err
}

func (s *Service) ListMultipartUploads(
	ctx context.Context,
	in *api.ListMultipartUploadsRequest,
) (*api.ListMultipartUploadsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	limit := int64(1000)
	if in.MaxUploads != nil {
		limit = *in.MaxUploads
	}
	if limit < 0 || limit > 1000 {
		return nil, fail(400, "InvalidArgument", "max-uploads must be from 0 to 1000")
	}
	if value(in.UploadIdMarker) != "" && value(in.KeyMarker) == "" {
		return nil, fail(400, "InvalidArgument", "upload-id-marker requires key-marker")
	}
	if e := value(in.EncodingType); e != "" && e != encodingURL {
		return nil, fail(400, "InvalidArgument", "Invalid encoding type")
	}
	out := &api.ListMultipartUploadsOutput{
		Bucket: in.Bucket, KeyMarker: in.KeyMarker, UploadIdMarker: in.UploadIdMarker,
		Prefix: in.Prefix, Delimiter: in.Delimiter, MaxUploads: &limit,
		IsTruncated: new(false), EncodingType: in.EncodingType,
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}
		type item struct {
			id string
			u  upload
		}
		var items []item
		for _, k := range tx.Keys() {
			id, ok := strings.CutPrefix(k, bucket+"/u/")
			if !ok {
				continue
			}
			var u upload
			if _, readErr := readJSON(tx, k, &u); readErr != nil {
				return readErr
			}
			if !strings.HasPrefix(u.Key, value(in.Prefix)) || u.Key < value(in.KeyMarker) ||
				(u.Key == value(in.KeyMarker) && (in.UploadIdMarker == nil || id <= *in.UploadIdMarker)) {
				continue
			}
			items = append(items, item{id, u})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].u.Key == items[j].u.Key {
				return items[i].id < items[j].id
			}
			return items[i].u.Key < items[j].u.Key
		})
		seen := map[string]bool{}
		count := int64(0)
		for _, it := range items {
			if d := value(in.Delimiter); d != "" {
				if pos := strings.Index(it.u.Key[len(value(in.Prefix)):], d); pos >= 0 {
					common := it.u.Key[:len(value(in.Prefix))+pos+len(d)]
					if common <= value(in.KeyMarker) {
						continue
					}
					if seen[common] {
						continue
					}
					if count == limit {
						*out.IsTruncated = true
						break
					}
					seen[common] = true
					out.CommonPrefixes = append(out.CommonPrefixes, api.CommonPrefix{Prefix: &common})
					count++
					out.NextKeyMarker = new(common)
					out.NextUploadIdMarker = nil
					continue
				}
			}
			if count == limit {
				*out.IsTruncated = true
				break
			}
			key := it.u.Key
			if in.EncodingType != nil {
				key = url.PathEscape(key)
			}
			out.Uploads = append(out.Uploads, api.MultipartUpload{
				Key: &key, UploadId: &it.id,
				Initiated:    new(time.UnixMilli(it.u.Initiated).UTC()),
				StorageClass: new(storageStandard),
			})
			out.NextKeyMarker = new(it.u.Key)
			out.NextUploadIdMarker = new(it.id)
			count++
		}
		if !*out.IsTruncated {
			out.NextKeyMarker = nil
			out.NextUploadIdMarker = nil
		}
		return nil
	})
	return out, err
}

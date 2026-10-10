package s3

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 defines MD5-based multipart ETags.
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const minMultipartPart = 5 << 20

type upload struct {
	Metadata    map[string]string
	Key         string
	ContentType string
	// ChecksumAlgorithm and ChecksumType are what the upload was created with, or the default.
	ChecksumAlgorithm string `json:",omitempty"`
	ChecksumType      string `json:",omitempty"`
	Initiated         int64
}

// kind returns the upload's checksum algorithm and type.
func (u *upload) kind() (algorithm, typ string) {
	if u.ChecksumAlgorithm == "" {
		return defaultChecksum, typeFullObject
	}

	return u.ChecksumAlgorithm, u.ChecksumType
}

type uploadPart struct {
	ETag string
	// Checksum is the part's digest under the upload's algorithm.
	Checksum     string `json:",omitempty"`
	Size         int64
	LastModified int64
}

func noSuchUpload() error {
	return fail(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
}

func uploadKey(bucket, id string) string { return bucket + "/u/" + id }
func partKey(bucket, id string, n int64) string {
	return fmt.Sprintf("%s/pb/%s/%05d", bucket, id, n)
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
	for _, k := range append(tx.Keys(bucket+"/pb/"+id+"/"), tx.Keys(bucket+"/pm/"+id+"/")...) {
		tx.Delete(k)
	}
}

func (s *Service) CreateMultipartUpload(
	ctx context.Context,
	in *api.CreateMultipartUploadRequest,
) (*api.CreateMultipartUploadOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	if c := common.Value(in.StorageClass); c != "" && c != storageStandard {
		return nil, fail(http.StatusNotImplemented, "NotImplemented", "Only STANDARD storage is supported")
	}

	if sseErr := encryption(in.ServerSideEncryption); sseErr != nil {
		return nil, sseErr
	}

	metadataSize := 0
	for k, v := range in.Metadata {
		metadataSize += len(k) + len(v)
	}

	if metadataSize > maxMetadataBytes {
		return nil, fail(http.StatusBadRequest, "MetadataTooLarge", "Metadata exceeds the supported limit")
	}

	algorithm, checksumType, err := uploadType(in.ChecksumAlgorithm, in.ChecksumType)
	if err != nil {
		return nil, err
	}

	idBytes := make([]byte, 24)
	if _, err = rand.Read(idBytes); err != nil {
		return nil, err
	}

	id := base64.RawURLEncoding.EncodeToString(idBytes)
	u := upload{
		Key:         *in.Key,
		ContentType: common.Value(in.ContentType),
		Metadata:    in.Metadata,
		Initiated:   s.now().UnixMilli(),

		ChecksumAlgorithm: algorithm,
		ChecksumType:      checksumType,
	}

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

	out := &api.CreateMultipartUploadOutput{Bucket: in.Bucket, Key: in.Key, UploadId: &id}
	if in.ChecksumAlgorithm != nil {
		out.ChecksumAlgorithm, out.ChecksumType = &algorithm, &checksumType
	}

	return out, nil
}

func (s *Service) UploadPart(ctx context.Context, in *api.UploadPartRequest) (*api.UploadPartOutput, error) {
	if int64(len(in.Body)) > s.maxObjectBytes {
		return nil, fail(400, "EntityTooLarge", "Part exceeds configured limit")
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	n := common.Value(in.PartNumber)
	if n < 1 || n > 10000 {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Part number must be from 1 to 10000")
	}

	if in.ContentLength != nil && *in.ContentLength != int64(len(in.Body)) {
		return nil, fail(http.StatusBadRequest, "IncompleteBody", "Content length does not match the uploaded part")
	}

	blob, md5Sum, values, err := s.writeBody(in.Body)
	if err != nil {
		return nil, err
	}

	defer s.blobs.Release(blob)
	if in.ContentMD5 != nil {
		want, decodeErr := base64.StdEncoding.DecodeString(*in.ContentMD5)
		if decodeErr != nil || len(want) != md5.Size {
			return nil, fail(400, "InvalidDigest", "Invalid Content-MD5")
		}

		if !bytes.Equal(want, md5Sum) {
			return nil, fail(400, "BadDigest", "Content-MD5 does not match")
		}
	}

	etag := `"` + hex.EncodeToString(md5Sum) + `"`
	var partChecksum checksum
	p := uploadPart{ETag: etag, Size: int64(len(in.Body)), LastModified: s.now().UnixMilli()}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		u, loadErr := loadUpload(tx, bucket, *in.Key, common.Value(in.UploadId))
		if loadErr != nil {
			return loadErr
		}

		algorithm, _ := u.kind()
		stored, checkErr := checksumOf(in, values, algorithm)
		if checkErr != nil {
			return checkErr
		}

		p.Checksum, partChecksum = stored.Value, stored
		tx.Put(partKey(bucket, *in.UploadId, n), blob.Record())
		return writeJSON(tx, partMetaKey(bucket, *in.UploadId, n), &p)
	})
	if err != nil {
		return nil, err
	}

	out := &api.UploadPartOutput{ETag: &etag}
	partChecksum.apply(out)

	return out, nil
}

func (s *Service) CompleteMultipartUpload(
	ctx context.Context,
	in *api.CompleteMultipartUploadRequest,
) (*api.CompleteMultipartUploadOutput, error) {
	m, err := request.Regional(ctx)
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
		n := common.Value(requested.PartNumber)
		if n <= lastNumber || n > 10000 {
			return nil, fail(400, "InvalidPartOrder", "Parts must be in ascending order")
		}

		lastNumber = n
	}

	var etag string
	var size int64
	var version *string
	var stored *checksum
	var cfg *api.NotificationConfiguration
	var u upload
	var bucket string
	var refs [][]byte
	var partSums []string
	var digests []byte
	defer func() {
		for _, ref := range refs {
			s.releaseBody(ref)
		}
	}()
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var resolveErr error
		bucket, resolveErr = resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		var cfgErr error
		if cfg, cfgErr = notifications(tx, bucket); cfgErr != nil {
			return cfgErr
		}

		var loadErr error
		u, loadErr = loadUpload(tx, bucket, *in.Key, common.Value(in.UploadId))
		if loadErr != nil {
			return loadErr
		}

		algorithm, _ := u.kind()
		last := int64(0)
		for i, requested := range in.MultipartUpload.Parts {
			n := common.Value(requested.PartNumber)
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

			if partErr := checkPart(&requested, algorithm, p.Checksum); partErr != nil {
				return partErr
			}

			partSums = append(partSums, p.Checksum)
			if i < len(in.MultipartUpload.Parts)-1 && p.Size < minMultipartPart {
				return fail(400, "EntityTooSmall", "Every nonfinal part must be at least 5 MiB")
			}

			if size+p.Size > s.maxObjectBytes {
				return fail(400, "EntityTooLarge", "Completed object exceeds configured limit")
			}

			part, ok := tx.Get(partKey(bucket, *in.UploadId, n))
			blob, isBlob := storage.BlobReference(part)
			if !ok || (isBlob && blob.Size != p.Size) || (!isBlob && int64(len(part)) != p.Size) {
				return fail(400, "InvalidPart", "Part data is missing")
			}

			if isBlob {
				s.blobs.Pin(blob)
			}

			refs = append(refs, part)
			size += p.Size
			digest, decodeErr := hex.DecodeString(strings.Trim(p.ETag, `"`))
			if decodeErr != nil {
				return decodeErr
			}

			digests = append(digests, digest...)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	algorithm, checksumType := u.kind()
	reader := &partsReader{service: s, refs: refs}
	defer func() { _ = reader.Close() }()
	digest := checksums()[algorithm]()
	blob, err := s.blobs.Write(io.TeeReader(reader, digest))
	if err != nil {
		return nil, err
	}

	defer s.blobs.Release(blob)
	if blob.Size != size {
		return nil, fail(400, "InvalidPart", "Part data size does not match")
	}

	final, checksumErr := completedChecksum(in, algorithm, checksumType, size,
		base64.StdEncoding.EncodeToString(digest.Sum(nil)), partSums)
	if checksumErr != nil {
		return nil, checksumErr
	}

	stored = &final
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadUpload(tx, bucket, *in.Key, common.Value(in.UploadId)); loadErr != nil {
			return loadErr
		}

		for i, requested := range in.MultipartUpload.Parts {
			data, ok := tx.Get(partKey(bucket, *in.UploadId, common.Value(requested.PartNumber)))
			if !ok || !bytes.Equal(data, refs[i]) {
				return fail(409, "OperationAborted", "Parts changed during completion; retry the request")
			}
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
			Size: size, LastModified: s.now().UnixMilli(), Checksum: stored,
		}

		status, writeErr := putCurrent(tx, bucket, *in.Key, &obj, blob.Record())
		if writeErr != nil {
			return writeErr
		}

		version = reported(status, &obj)
		removeUpload(tx, bucket, *in.UploadId)
		size = obj.Size
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.notify(ctx, m, cfg, objectEvent{
		name: "ObjectCreated:CompleteMultipartUpload", reason: "CompleteMultipartUpload",
		bucket: *in.Bucket, key: *in.Key, etag: etag, version: common.Value(version), size: size,
	})
	location := "/" + *in.Bucket + "/" + url.PathEscape(*in.Key)
	out := &api.CompleteMultipartUploadOutput{
		Location: &location, Bucket: in.Bucket, Key: in.Key, ETag: &etag, VersionId: version,
	}

	stored.apply(out)

	return out, nil
}

func (s *Service) AbortMultipartUpload(
	ctx context.Context,
	in *api.AbortMultipartUploadRequest,
) (*api.AbortMultipartUploadOutput, error) {
	m, err := request.Regional(ctx)
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

		if _, loadErr := loadUpload(tx, bucket, *in.Key, common.Value(in.UploadId)); loadErr != nil {
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
	m, err := request.Regional(ctx)
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

		u, loadErr := loadUpload(tx, bucket, *in.Key, common.Value(in.UploadId))
		if loadErr != nil {
			return loadErr
		}

		algorithm, checksumType := u.kind()
		out.ChecksumAlgorithm, out.ChecksumType = &algorithm, &checksumType
		var nums []int64
		prefix := bucket + "/pm/" + *in.UploadId + "/"
		for _, k := range tx.Keys(prefix) {
			n, parseErr := strconv.ParseInt(strings.TrimPrefix(k, prefix), 10, 64)
			if parseErr == nil && n > marker {
				nums = append(nums, n)
			}
		}

		if int64(len(nums)) > limit {
			*out.IsTruncated = true
			nums = nums[:limit]
		}

		for _, n := range nums {
			var p uploadPart
			if _, readErr := readJSON(tx, partMetaKey(bucket, *in.UploadId, n), &p); readErr != nil {
				return readErr
			}

			part := api.Part{
				PartNumber: &n, ETag: &p.ETag, Size: &p.Size,
				LastModified: new(time.UnixMilli(p.LastModified).UTC()),
			}

			(&checksum{Algorithm: algorithm, Value: p.Checksum}).apply(&part)
			out.Parts = append(out.Parts, part)
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
	m, err := request.Regional(ctx)
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

	if common.Value(in.UploadIdMarker) != "" && common.Value(in.KeyMarker) == "" {
		return nil, fail(400, "InvalidArgument", "upload-id-marker requires key-marker")
	}

	if e := common.Value(in.EncodingType); e != "" && e != encodingURL {
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
		for _, k := range tx.Keys(bucket + "/u/") {
			id := strings.TrimPrefix(k, bucket+"/u/")
			var u upload
			if _, readErr := readJSON(tx, k, &u); readErr != nil {
				return readErr
			}

			if !strings.HasPrefix(u.Key, common.Value(in.Prefix)) || u.Key < common.Value(in.KeyMarker) ||
				(u.Key == common.Value(in.KeyMarker) && (in.UploadIdMarker == nil || id <= *in.UploadIdMarker)) {
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
			if d := common.Value(in.Delimiter); d != "" {
				if pos := strings.Index(it.u.Key[len(common.Value(in.Prefix)):], d); pos >= 0 {
					prefix := it.u.Key[:len(common.Value(in.Prefix))+pos+len(d)]
					if prefix <= common.Value(in.KeyMarker) {
						continue
					}

					if seen[prefix] {
						continue
					}

					if count == limit {
						*out.IsTruncated = true
						break
					}

					seen[prefix] = true
					out.CommonPrefixes = append(out.CommonPrefixes, api.CommonPrefix{Prefix: &prefix})
					count++
					out.NextKeyMarker = new(prefix)
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

// checkPart checks the checksum a completion request lists for a part against
// the one the part was uploaded with.
func checkPart(requested *api.CompletedPart, algorithm, stored string) error {
	for name, value := range sentChecksums(requested) {
		if name != algorithm {
			return fail(http.StatusBadRequest, "InvalidRequest",
				"Checksum Type mismatch occurred, expected checksum Type: "+algorithm+", actual checksum Type: "+name)
		}

		if value != stored {
			return fail(http.StatusBadRequest, "InvalidPart", "The checksum of a part does not match the uploaded part")
		}
	}

	return nil
}

// completedChecksum is the checksum of a completed upload: a COMPOSITE one
// combines the parts' digests, and a FULL_OBJECT one covers all the bytes.
// Anything the request states about the whole object must agree.
func completedChecksum(
	in *api.CompleteMultipartUploadRequest,
	algorithm, checksumType string,
	size int64,
	fullSum string,
	partSums []string,
) (checksum, error) {
	if t := common.Value(in.ChecksumType); t != "" && t != checksumType {
		return checksum{}, fail(http.StatusBadRequest, "InvalidRequest",
			"The upload was created using the "+checksumType+" checksum type; the request used "+t+".")
	}

	if in.MpuObjectSize != nil && *in.MpuObjectSize != size {
		return checksum{}, fail(http.StatusBadRequest, "InvalidRequest",
			"The provided 'x-amz-mp-object-size' header value does not match what was computed.")
	}

	c := checksum{Algorithm: algorithm, Type: checksumType}
	if checksumType == typeFullObject {
		c.Value = fullSum
	} else {
		var err error
		if c.Value, err = compositeOf(algorithm, partSums); err != nil {
			return checksum{}, fail(http.StatusBadRequest, "InvalidPart", "A part has no usable checksum")
		}
	}

	for name, value := range sentChecksums(in) {
		if name != algorithm {
			return checksum{}, fail(http.StatusBadRequest, "InvalidRequest",
				"Checksum Type mismatch occurred, expected checksum Type: "+algorithm+", actual checksum Type: "+name)
		}

		if value != c.Value {
			return checksum{}, fail(http.StatusBadRequest, "BadDigest",
				"The "+name+" you specified did not match the calculated checksum.")
		}
	}

	return c, nil
}

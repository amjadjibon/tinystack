package s3

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A bucket's versioning status is "" until versioning is first enabled; it can
// then only be suspended, never turned off.
//
// The current version of a key is always ".../o/<key>" with its bytes at
// ".../db/<key>", so ListObjectsV2 reads only current objects. Every other
// version, and every delete marker, is in the key's history record
// ".../v/<key>", newest first; a noncurrent version's bytes are at
// ".../bb/<version ID>/<key>". When a key has no current object, the newest
// history entry is a delete marker, which is then the latest version.
const (
	versioningEnabled   = "Enabled"
	versioningSuspended = "Suspended"
	// nullVersion is the version ID of an object written while versioning was
	// off or suspended; such an object stores an empty VersionID.
	nullVersion = "null"
)

func versionOf(obj *object) string {
	if obj.VersionID == "" {
		return nullVersion
	}

	return obj.VersionID
}

func noSuchVersion() error {
	return fail(http.StatusNotFound, "NoSuchVersion", "The specified version does not exist.")
}

// deleteMarkerError is the error for reading a delete marker: NoSuchKey when
// it is the latest version and no version was named, MethodNotAllowed when the
// marker was named by its version ID.
func deleteMarkerError(marker *object, named bool) error {
	headers := map[string]string{"x-amz-delete-marker": "true", "x-amz-version-id": versionOf(marker)}
	if named {
		headers["Last-Modified"] = time.UnixMilli(marker.LastModified).UTC().Format(http.TimeFormat)
		return &awserror.Error{
			Code:       "MethodNotAllowed",
			Message:    "The specified method is not allowed against this resource.",
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    headers,
		}
	}

	return &awserror.Error{
		Code:       "NoSuchKey",
		Message:    "The specified key does not exist.",
		StatusCode: http.StatusNotFound,
		Headers:    headers,
	}
}

// versioning returns the versioning status of the bucket at bucketKey.
func versioning(tx storage.ReadTx, bucketKey string) (string, error) {
	var meta bucketMeta
	if _, err := readJSON(tx, bucketKey, &meta); err != nil {
		return "", err
	}

	return meta.Versioning, nil
}

// reported is the version ID a response names: none for a bucket that never
// had versioning, and "null" for a null version in one that did.
func reported(status string, obj *object) *string {
	if status == "" {
		return nil
	}

	return new(versionOf(obj))
}

func history(tx storage.ReadTx, bucket, key string) ([]object, error) {
	var h []object
	_, err := readJSON(tx, bucket+"/v/"+key, &h)
	return h, err
}

func saveHistory(tx storage.WriteTx, bucket, key string, h []object) error {
	if len(h) == 0 {
		tx.Delete(bucket + "/v/" + key)
		return nil
	}

	return writeJSON(tx, bucket+"/v/"+key, h)
}

// bodyKey is where the bytes of a noncurrent version are stored.
func bodyKey(bucket, key string, obj *object) string {
	return bucket + "/bb/" + versionOf(obj) + "/" + key
}

// archive moves the current object of key, if any, to the front of h.
func archive(tx storage.WriteTx, bucket, key string, h []object) ([]object, error) {
	var current object
	found, err := readJSON(tx, bucket+"/o/"+key, &current)
	if err != nil || !found {
		return h, err
	}

	body, _ := tx.Get(bucket + "/db/" + key)
	tx.Put(bodyKey(bucket, key, &current), body)
	tx.Delete(bucket + "/o/" + key)
	tx.Delete(bucket + "/db/" + key)
	return append([]object{current}, h...), nil
}

// dropNull removes the null version from h, as a write or delete while
// versioning is suspended replaces it.
func dropNull(tx storage.WriteTx, bucket, key string, h []object) []object {
	return slices.DeleteFunc(h, func(o object) bool {
		if o.VersionID != "" {
			return false
		}

		tx.Delete(bodyKey(bucket, key, &o))
		return true
	})
}

// promote makes the newest history entry current when the key has no current
// object and that entry is an object rather than a delete marker.
func promote(tx storage.WriteTx, bucket, key string, h []object) ([]object, error) {
	if _, found := tx.Get(bucket + "/o/" + key); found || len(h) == 0 || h[0].DeleteMarker {
		return h, nil
	}

	next := h[0]
	body, _ := tx.Get(bodyKey(bucket, key, &next))
	tx.Delete(bodyKey(bucket, key, &next))
	tx.Put(bucket+"/db/"+key, body)
	return h[1:], writeJSON(tx, bucket+"/o/"+key, &next)
}

// putCurrent stores obj as the current version of key. With versioning on, the
// previous current version is kept as a noncurrent one; obj gets a new version
// ID when versioning is enabled and replaces the null version when it is
// suspended.
func putCurrent(tx storage.WriteTx, bucket, key string, obj *object, body []byte) (string, error) {
	status, err := versioning(tx, bucket)
	if err != nil {
		return "", err
	}

	obj.VersionID, obj.DeleteMarker = "", false
	if status != "" {
		h, loadErr := history(tx, bucket, key)
		if loadErr != nil {
			return "", loadErr
		}

		if h, err = archive(tx, bucket, key, h); err != nil {
			return "", err
		}

		if status == versioningEnabled {
			obj.VersionID = rand.Text()
		} else {
			h = dropNull(tx, bucket, key, h)
		}

		if saveErr := saveHistory(tx, bucket, key, h); saveErr != nil {
			return "", saveErr
		}
	}

	tx.Put(bucket+"/db/"+key, body)
	return status, writeJSON(tx, bucket+"/o/"+key, obj)
}

// removal is the outcome of a DeleteObject.
type removal struct {
	version string // the version ID named or created, when versioning applies
	marker  bool   // a delete marker was created or removed
	removed bool   // something was deleted or a marker was created
}

// deleteCurrent deletes key without naming a version: an unversioned bucket
// removes the object, and a versioned one hides it behind a new delete marker.
func deleteCurrent(tx storage.WriteTx, bucket, key string, now int64) (removal, error) {
	status, err := versioning(tx, bucket)
	if err != nil {
		return removal{}, err
	}

	if status == "" {
		_, found := tx.Get(bucket + "/o/" + key)
		tx.Delete(bucket + "/o/" + key)
		tx.Delete(bucket + "/db/" + key)
		return removal{removed: found}, nil
	}

	h, err := history(tx, bucket, key)
	if err != nil {
		return removal{}, err
	}

	if h, err = archive(tx, bucket, key, h); err != nil {
		return removal{}, err
	}

	marker := object{DeleteMarker: true, LastModified: now}
	if status == versioningEnabled {
		marker.VersionID = rand.Text()
	} else {
		h = dropNull(tx, bucket, key, h)
	}

	return removal{version: versionOf(&marker), marker: true, removed: true},
		saveHistory(tx, bucket, key, append([]object{marker}, h...))
}

// deleteVersion permanently deletes one version of key. Deleting the current
// version, or the delete marker hiding the key, makes the next version current.
// A version that does not exist is not an error.
func deleteVersion(tx storage.WriteTx, bucket, key, version string) (removal, error) {
	out := removal{version: version}
	h, err := history(tx, bucket, key)
	if err != nil {
		return out, err
	}

	var current object
	found, err := readJSON(tx, bucket+"/o/"+key, &current)
	switch {
	case err != nil:
		return out, err
	case found && versionOf(&current) == version:
		tx.Delete(bucket + "/o/" + key)
		tx.Delete(bucket + "/db/" + key)
		out.removed = true
	default:
		i := slices.IndexFunc(h, func(o object) bool { return versionOf(&o) == version })
		if i < 0 {
			return out, nil
		}

		out.marker, out.removed = h[i].DeleteMarker, true
		tx.Delete(bodyKey(bucket, key, &h[i]))
		h = slices.Delete(h, i, i+1)
	}

	if h, err = promote(tx, bucket, key, h); err != nil {
		return out, err
	}

	return out, saveHistory(tx, bucket, key, h)
}

// locate finds a version of key and where its bytes are: the current object
// when version is empty, otherwise the named version. Delete markers cannot
// be read.
func locate(tx storage.ReadTx, bucket, key, version string) (obj object, data string, err error) {
	found, err := readJSON(tx, bucket+"/o/"+key, &obj)
	if err != nil {
		return obj, "", err
	}

	if found && (version == "" || versionOf(&obj) == version) {
		return obj, bucket + "/db/" + key, nil
	}

	h, err := history(tx, bucket, key)
	if err != nil {
		return obj, "", err
	}

	if version == "" {
		if !found && len(h) > 0 && h[0].DeleteMarker {
			return obj, "", deleteMarkerError(&h[0], false)
		}

		return obj, "", noSuchKey()
	}

	for i := range h {
		if versionOf(&h[i]) != version {
			continue
		}

		if h[i].DeleteMarker {
			return obj, "", deleteMarkerError(&h[i], true)
		}

		return h[i], bodyKey(bucket, key, &h[i]), nil
	}

	return obj, "", noSuchVersion()
}

// rewrite applies fn to one version of key, located as locate does, and saves it.
func rewrite(tx storage.WriteTx, bucket, key, version string, fn func(*object)) (*object, error) {
	obj, data, err := locate(tx, bucket, key, version)
	if err != nil {
		return nil, err
	}

	fn(&obj)
	if data == bucket+"/db/"+key {
		return &obj, writeJSON(tx, bucket+"/o/"+key, &obj)
	}

	h, err := history(tx, bucket, key)
	if err != nil {
		return nil, err
	}

	for i := range h {
		if versionOf(&h[i]) == versionOf(&obj) {
			h[i] = obj
		}
	}

	return &obj, saveHistory(tx, bucket, key, h)
}

func (s *Service) PutBucketVersioning(
	ctx context.Context,
	in *api.PutBucketVersioningRequest,
) (*api.PutBucketVersioningOutput, error) {
	cfg := in.VersioningConfiguration
	if cfg == nil {
		return nil, fail(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.")
	}

	status := common.Value(cfg.Status)
	if status != versioningEnabled && status != versioningSuspended {
		return nil, fail(http.StatusBadRequest, "IllegalVersioningConfigurationException",
			"The Versioning element must be specified")
	}

	switch common.Value(cfg.MFADelete) {
	case "", "Disabled":
	case "Enabled":
		return nil, fail(http.StatusNotImplemented, "NotImplemented", "TinyStack does not support MFA delete")
	default:
		return nil, fail(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.")
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Versioning = status })
	if err != nil {
		return nil, err
	}

	return &api.PutBucketVersioningOutput{}, nil
}

func (s *Service) GetBucketVersioning(
	ctx context.Context,
	in *api.GetBucketVersioningRequest,
) (*api.GetBucketVersioningOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var status string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		status, resolveErr = versioning(tx, key)
		return resolveErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetBucketVersioningOutput{}
	if status != "" {
		out.Status = &status
	}

	return out, nil
}

// entry is one version or delete marker in a version listing.
type entry struct {
	key    string
	obj    object
	latest bool
}

// versionPage is one ListObjectVersions page before encoding.
type versionPage struct {
	lastKey, lastVersion string
	entries              []entry
	prefixes             []string
	more                 bool
}

// resume is the index of the first entry after the key and version ID
// markers. An unknown version ID marker resumes after the marker key.
func resume(versions []entry, keyMarker, versionMarker string) int {
	if versionMarker != "" {
		i := slices.IndexFunc(versions, func(e entry) bool {
			return e.key == keyMarker && versionOf(&e.obj) == versionMarker
		})
		if i >= 0 {
			return i + 1
		}
	}

	i := slices.IndexFunc(versions, func(e entry) bool { return e.key > keyMarker })
	if i < 0 {
		return len(versions)
	}

	return i
}

// versionsPage walks every version in key order, newest first within a key,
// grouping keys that share a prefix up to the delimiter, until limit entries
// are collected.
func versionsPage(versions []entry, prefix, delimiter, keyMarker, versionMarker string, limit int64) versionPage {
	var p versionPage
	seen := map[string]bool{}
	for i := resume(versions, keyMarker, versionMarker); i < len(versions); i++ {
		e := &versions[i]
		// A common prefix returned on an earlier page covers its keys.
		if delimiter != "" && strings.HasSuffix(keyMarker, delimiter) && strings.HasPrefix(e.key, keyMarker) {
			continue
		}

		rollup := ""
		if j := strings.Index(e.key[len(prefix):], delimiter); delimiter != "" && j >= 0 {
			rollup = e.key[:len(prefix)+j+len(delimiter)]
		}

		if rollup != "" && seen[rollup] {
			continue
		}

		if int64(len(p.entries)+len(p.prefixes)) == limit {
			p.more = true
			break
		}

		if rollup != "" {
			seen[rollup] = true
			p.prefixes = append(p.prefixes, rollup)
			p.lastKey, p.lastVersion = rollup, ""
			continue
		}

		p.entries = append(p.entries, *e)
		p.lastKey, p.lastVersion = e.key, versionOf(&e.obj)
	}

	return p
}

// ListObjectVersions lists every version and delete marker in key order,
// newest first within a key. A bucket without versioning lists its objects as
// null versions.
func (s *Service) ListObjectVersions(
	ctx context.Context,
	in *api.ListObjectVersionsRequest,
) (*api.ListObjectVersionsOutput, error) {
	m, err := request.Regional(ctx)
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

	encoding := common.Value(in.EncodingType)
	if encoding != "" && encoding != encodingURL {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Invalid Encoding Method specified in Request")
	}

	if in.VersionIdMarker != nil && in.KeyMarker == nil {
		return nil, fail(http.StatusBadRequest, "InvalidArgument",
			"A version-id marker cannot be specified without a key marker.")
	}

	prefix := common.Value(in.Prefix)
	var versions []entry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		bucket, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		var keys []string
		for _, kind := range []string{"/o/", "/v/"} {
			for _, k := range tx.Keys(bucket + kind + prefix) {
				keys = append(keys, strings.TrimPrefix(k, bucket+kind))
			}
		}

		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			var current object
			found, readErr := readJSON(tx, bucket+"/o/"+key, &current)
			if readErr != nil {
				return readErr
			}

			h, readErr := history(tx, bucket, key)
			if readErr != nil {
				return readErr
			}

			if found {
				h = append([]object{current}, h...)
			}

			for i := range h {
				versions = append(versions, entry{key: key, obj: h[i], latest: i == 0})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	p := versionsPage(
		versions,
		prefix,
		common.Value(in.Delimiter),
		common.Value(in.KeyMarker),
		common.Value(in.VersionIdMarker),
		limit,
	)
	encode := func(s string) *string {
		if encoding == encodingURL {
			s = url.QueryEscape(s)
		}

		return new(s)
	}

	out := &api.ListObjectVersionsOutput{
		Name:            in.Bucket,
		Prefix:          encode(prefix),
		MaxKeys:         new(limit),
		IsTruncated:     new(p.more),
		VersionIdMarker: in.VersionIdMarker,
		EncodingType:    in.EncodingType,
	}

	if in.KeyMarker != nil {
		out.KeyMarker = encode(*in.KeyMarker)
	}

	if in.Delimiter != nil {
		out.Delimiter = encode(*in.Delimiter)
	}

	if p.more {
		out.NextKeyMarker = encode(p.lastKey)
		if p.lastVersion != "" {
			out.NextVersionIdMarker = &p.lastVersion
		}
	}

	owner := &api.Owner{ID: new(canonicalID(m.AccountID))}
	for _, e := range p.entries {
		modified := new(time.UnixMilli(e.obj.LastModified).UTC())
		if e.obj.DeleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, api.DeleteMarkerEntry{
				Key: encode(e.key), VersionId: new(versionOf(&e.obj)), IsLatest: new(e.latest),
				LastModified: modified, Owner: owner,
			})
			continue
		}

		out.Versions = append(out.Versions, api.ObjectVersion{
			Key: encode(e.key), VersionId: new(versionOf(&e.obj)), IsLatest: new(e.latest),
			LastModified: modified, ETag: new(e.obj.ETag), Size: new(e.obj.Size),
			StorageClass: new(storageStandard), Owner: owner,
		})
	}

	for _, rollup := range p.prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, api.CommonPrefix{Prefix: encode(rollup)})
	}

	return out, nil
}

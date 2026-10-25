// Package s3 implements S3 buckets and objects without HTTP dependencies.
//
// Bucket names share one partition-wide namespace. The index record
// "aws/s3/buckets/<name>" maps each name to its owning account and region and
// is written in the same transaction as the bucket's regional metadata under
// "<account>/<region>/s3/<name>". Objects live below that regional key: object
// metadata under ".../o/<key>" and immutable body references under ".../db/<key>", with
// noncurrent versions and delete markers as described in versions.go. Historical
// and multipart body references use "/bb/" and "/pb/". S3 owns legacy raw-body
// migration, transactional reference counts and collection; storage owns file mechanics.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	indexPrefix   = "aws/s3/buckets/"
	defaultRegion = "us-east-1"
	maxBuckets    = 10000
)

var _ api.API = (*Service)(nil)

type Service struct {
	store   *bodyStore
	blobs   *storage.BlobStore
	blobErr error
	// queues, topics, functions, and events receive bucket notifications,
	// which deliveries records.
	queues     *sqs.Service
	topics     *sns.Service
	functions  *lambda.Service
	events     *eventbridge.Service
	deliveries *delivery.Log
	// authorizer checks notifications to queues, topics, and functions against their resource policies;
	// see UseServiceAuthorizer.
	accountBlocksPublicPolicy func(context.Context) (bool, error)
	authorizer                catalog.ServiceAuthorizer
	now                       func() time.Time
	maxObjectBytes            int64
}

// New owns the supplied file facility, or creates a temporary one for standalone use.
// With supplied files, call Resume before serving requests. Stop closes the facility.
func New(
	store storage.Store,
	queues *sqs.Service,
	topics *sns.Service,
	functions *lambda.Service,
	events *eventbridge.Service,
	deliveries *delivery.Log,
	files ...*storage.BlobStore,
) *Service {
	var blobs *storage.BlobStore
	var blobErr error
	if len(files) > 0 {
		blobs = files[0]
	} else {
		blobs, blobErr = storage.OpenBlobs("")
	}

	s := &Service{
		blobs: blobs, blobErr: blobErr, maxObjectBytes: 256 << 20,
		store:      &bodyStore{Store: store, blobs: blobs},
		queues:     queues,
		topics:     topics,
		functions:  functions,
		events:     events,
		deliveries: deliveries,
		now:        time.Now,
	}

	if len(files) == 0 {
		s.blobErr = s.Resume(context.Background())
	}

	return s
}

// bucketIndex is the partition-wide record that reserves a bucket name.
type bucketIndex struct {
	Account   string
	Region    string
	CreatedAt int64
}

// bucketMeta is the bucket's regional metadata.
type bucketMeta struct {
	Tags          map[string]string              `json:",omitempty"`
	Notifications *api.NotificationConfiguration `json:",omitempty"`
	// Encryption is the default encryption, stored only: TinyStack keeps
	// objects as sent. Nil means SSE-S3.
	Encryption        *api.ServerSideEncryptionConfiguration `json:",omitempty"`
	PublicAccessBlock *api.PublicAccessBlockConfiguration    `json:",omitempty"`
	Lifecycle         *lifecycle                             `json:",omitempty"`
	Website           *api.WebsiteConfiguration              `json:",omitempty"`
	// ObjectLock is the bucket's Object Lock configuration, set only on
	// buckets created with Object Lock enabled.
	ObjectLock *api.ObjectLockConfiguration `json:",omitempty"`
	// Versioning is "", "Enabled", or "Suspended".
	Versioning string `json:",omitempty"`
	// Policy is the bucket policy, evaluated by the gateway.
	Policy string `json:",omitempty"`
	// Ownership is the ObjectOwnership setting, if the bucket has one.
	Ownership string         `json:",omitempty"`
	CORS      []api.CORSRule `json:",omitempty"`
	CreatedAt int64
}

// fail builds an S3 error with its HTTP status. Modeled errors take their
// status from the generated error bindings instead.
func fail(status int, code, message string) error {
	return &awserror.Error{Code: code, Message: message, StatusCode: status}
}

func malformedXML() error {
	return fail(http.StatusBadRequest, "MalformedXML",
		"The XML you provided was not well-formed or did not validate against our published schema")
}

func noSuchBucket() error { return awserror.New("NoSuchBucket", "The specified bucket does not exist") }

func accessDenied() error { return fail(http.StatusForbidden, "AccessDenied", "Access Denied") }

// canonicalID is the stable owner ID TinyStack reports for an account.
func canonicalID(account string) string {
	sum := sha256.Sum256([]byte("tinystack:" + account))
	return hex.EncodeToString(sum[:])
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validBucketName applies the general purpose bucket naming rules.
func validBucketName(name string) bool {
	if !bucketName.MatchString(name) || strings.Contains(name, "..") || net.ParseIP(name) != nil {
		return false
	}

	for _, prefix := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}

	for _, suffix := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}

	return true
}

func readJSON(tx storage.ReadTx, key string, v any) (bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return false, nil
	}

	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}

	return true, nil
}

func writeJSON(tx storage.WriteTx, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}

	tx.Put(key, data)
	return nil
}

// resolve finds the caller's bucket through the name index and returns its
// regional key. Cross-account requests require gateway resource authorization;
// buckets in another region must be addressed through that region.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func resolve(tx storage.ReadTx, m request.Metadata, name, expectedOwner *string) (string, error) {
	if strings.HasSuffix(common.Value(name), "-s3alias") && m.Operation != "" &&
		!strings.Contains(m.Operation, "Object") &&
		!strings.Contains(m.Operation, "Multipart") &&
		!strings.Contains(m.Operation, "Part") {
		return "", fail(400, "InvalidRequest", "Access point aliases support object operations only")
	}

	resolvedName, err := aliasBucket(tx, m.Scope, common.Value(name))
	if err != nil {
		return "", err
	}

	name = new(resolvedName)
	var index bucketIndex
	found, err := readJSON(tx, indexPrefix+common.Value(name), &index)
	if err != nil {
		return "", err
	}

	if !found {
		return "", noSuchBucket()
	}

	if (index.Account != m.AccountID && !m.ResourceAuthorization) ||
		(expectedOwner != nil && *expectedOwner != index.Account) {
		return "", accessDenied()
	}

	if index.Region != m.Region {
		return "", fail(
			http.StatusMovedPermanently,
			"PermanentRedirect",
			"The bucket you are attempting to access must be addressed using the specified endpoint. "+
				"The bucket is in "+index.Region+".",
		)
	}

	return index.Account + "/" + m.Region + "/s3/" + *name, nil
}

func (s *Service) CreateBucket(ctx context.Context, in *api.CreateBucketRequest) (*api.CreateBucketOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Bucket)
	if !validBucketName(name) {
		return nil, fail(http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.")
	}

	// "private" is the default canned ACL, so it asks for nothing TinyStack lacks.
	if acl := common.Value(in.ACL); acl != "" && acl != "private" {
		return nil, fail(http.StatusNotImplemented, "NotImplemented", "TinyStack only supports the private canned ACL.")
	}

	var tags map[string]string
	constraint := ""
	if in.CreateBucketConfiguration != nil {
		constraint = common.Value(in.CreateBucketConfiguration.LocationConstraint)
		tags, err = tagMap(in.CreateBucketConfiguration.Tags)
		if err != nil {
			return nil, err
		}
	}

	switch {
	case m.Region == defaultRegion && constraint != "":
		return nil, fail(http.StatusBadRequest, "InvalidLocationConstraint",
			"The specified location-constraint is not valid")
	case m.Region != defaultRegion && constraint != m.Region:
		return nil, fail(http.StatusBadRequest, "IllegalLocationConstraintException",
			"The "+constraint+" location constraint is incompatible for the region specific endpoint this "+
				"request was sent to.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var index bucketIndex
		found, readErr := readJSON(tx, indexPrefix+name, &index)
		if readErr != nil {
			return readErr
		}

		switch {
		case found && index.Account != m.AccountID:
			return awserror.New("BucketAlreadyExists", "The requested bucket name is not available.")
		case found && index.Region == defaultRegion && m.Region == defaultRegion:
			return nil // us-east-1 keeps the legacy behavior of succeeding again.
		case found:
			return awserror.New(
				"BucketAlreadyOwnedByYou",
				"Your previous request to create the named bucket succeeded.",
			)
		}

		now := s.now().UnixMilli()
		index = bucketIndex{Account: m.AccountID, Region: m.Region, CreatedAt: now}
		if guardErr := writeJSON(tx, indexPrefix+name, &index); guardErr != nil {
			return guardErr
		}

		meta := bucketMeta{CreatedAt: now, Tags: tags}
		if common.Value(in.ObjectLockEnabledForBucket) {
			meta.Versioning = versioningEnabled
			meta.ObjectLock = &api.ObjectLockConfiguration{
				ObjectLockEnabled: new(lockEnabled),
			}
		}

		newBucketAccess(&meta)
		return writeJSON(tx, m.AccountID+"/"+m.Region+"/s3/"+name, &meta)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateBucketOutput{Location: new("/" + name)}, nil
}

func (s *Service) DeleteBucket(ctx context.Context, in *api.DeleteBucketRequest) (*api.DeleteBucketOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		if len(tx.Keys(key+"/o/")) > 0 || len(tx.Keys(key+"/v/")) > 0 || len(tx.Keys(key+"/u/")) > 0 {
			return fail(http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty")
		}

		used, referenceErr := bucketHasAccessPoints(tx, m.Scope, common.Value(in.Bucket))
		if referenceErr != nil {
			return referenceErr
		}

		if used {
			return fail(409, "BucketHasAccessPointsAttached", "The bucket has access points attached")
		}

		tx.Delete(indexPrefix + *in.Bucket)
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketOutput{}, nil
}

func (s *Service) HeadBucket(ctx context.Context, in *api.HeadBucketRequest) (*api.HeadBucketOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		return resolveErr
	})
	if err != nil {
		return nil, err
	}

	return &api.HeadBucketOutput{BucketRegion: new(m.Region)}, nil
}

// ListBuckets lists the caller's buckets in every region, by name.
func (s *Service) ListBuckets(ctx context.Context, in *api.ListBucketsRequest) (*api.ListBucketsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(maxBuckets)
	if in.MaxBuckets != nil {
		if limit = *in.MaxBuckets; limit < 1 || limit > maxBuckets {
			return nil, fail(http.StatusBadRequest, "InvalidArgument", "max-buckets must be from 1 to 10000")
		}
	}

	after := ""
	if in.ContinuationToken != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*in.ContinuationToken)
		if decodeErr != nil || len(b) == 0 {
			return nil, fail(http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")
		}

		after = string(b)
	}

	var buckets []api.Bucket
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix + common.Value(in.Prefix)) {
			name := strings.TrimPrefix(k, indexPrefix)
			if name <= after {
				continue
			}

			var index bucketIndex
			if _, readErr := readJSON(tx, k, &index); readErr != nil {
				return readErr
			}

			if index.Account != m.AccountID || (in.BucketRegion != nil && index.Region != *in.BucketRegion) {
				continue
			}

			buckets = append(buckets, api.Bucket{
				Name:         new(name),
				CreationDate: new(time.UnixMilli(index.CreatedAt).UTC()),
				BucketRegion: new(index.Region),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListBucketsOutput{Owner: &api.Owner{ID: new(canonicalID(m.AccountID))}, Prefix: in.Prefix}
	if int64(len(buckets)) > limit {
		buckets = buckets[:limit]
		out.ContinuationToken = new(base64.RawURLEncoding.EncodeToString([]byte(*buckets[limit-1].Name)))
	}

	out.Buckets = buckets
	return out, nil
}

// UseMaxObjectBytes sets the upload and completed-object bound configured by the app.
func (s *Service) UseMaxObjectBytes(limit int64) { s.maxObjectBytes = limit }

// UseServiceAuthorizer makes notifications require the destination's resource policy to allow s3.amazonaws.com.
// Call it before serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// Stop collects orphan files, closes the facility and removes its temporary directory in memory mode.
func (s *Service) Stop() {
	if s.blobs != nil {
		_ = s.collect(context.Background())
		_ = s.blobs.Close()
	}
}

// UseAccountPublicAccessBlock connects the S3 Control account setting lookup before serving.
func (s *Service) UseAccountPublicAccessBlock(lookup func(context.Context) (bool, error)) {
	s.accountBlocksPublicPolicy = lookup
}

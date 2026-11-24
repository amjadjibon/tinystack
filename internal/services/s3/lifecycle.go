package s3

import (
	"context"
	"crypto/rand"
	"math"
	"net/http"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	maxLifecycleRules = 1000
	// minimumSize128K is the default TransitionDefaultMinimumObjectSize.
	minimumSize128K       = "all_storage_classes_128K"
	storageGlacier        = "GLACIER"
	storageDeepArchive    = "DEEP_ARCHIVE"
	storageOneZoneIA      = "ONEZONE_IA"
	configurationEnabled  = "Enabled"
	configurationDisabled = "Disabled"
)

var transitionClasses = map[string]bool{
	storageGlacier:        true,
	"STANDARD_IA":         true,
	storageOneZoneIA:      true,
	"INTELLIGENT_TIERING": true,
	storageDeepArchive:    true,
	"GLACIER_IR":          true,
}

// lifecycle is a bucket's stored lifecycle configuration.
type lifecycle struct {
	MinimumSize string
	Rules       []api.LifecycleRule
}

// count is how many of the conditions hold.
func count(conditions ...bool) int {
	n := 0
	for _, c := range conditions {
		if c {
			n++
		}
	}

	return n
}

// checkLifecycleRule validates one rule: its ID and status, one way of
// selecting objects, and at least one well-formed action.
func checkLifecycleRule(r *api.LifecycleRule, ids map[string]bool) error {
	id := common.Value(r.ID)
	switch {
	case len(id) > 255:
		return invalidArgument("ID length should not exceed allowed limit of 255")
	case id != "" && ids[id]:
		return invalidArgument("Rule ID must be unique. Found same ID for more than one rule")
	case !enabledDisabled(r.Status):
		return malformedXML()
	case r.Prefix != nil && r.Filter != nil:
		return malformedXML()
	case r.Expiration == nil && len(r.Transitions) == 0 && r.NoncurrentVersionExpiration == nil &&
		len(r.NoncurrentVersionTransitions) == 0 && r.AbortIncompleteMultipartUpload == nil:
		return invalidArgument("At least one action needs to be specified in a rule")
	}

	ids[id] = true
	if err := checkLifecycleFilter(r); err != nil {
		return err
	}

	if e := r.Expiration; e != nil {
		if count(e.Date != nil, e.Days != nil, e.ExpiredObjectDeleteMarker != nil) != 1 {
			return malformedXML()
		}

		if e.Days != nil && !validLifecycleDays(e.Days, 1) {
			return invalidArgument("'Days' for Expiration action must be a positive integer")
		}

		if !validLifecycleDate(e.Date) {
			return invalidArgument("The Expiration date must be midnight UTC")
		}
	}

	for _, t := range r.Transitions {
		if count(t.Date != nil, t.Days != nil) != 1 ||
			(t.Days != nil && !validLifecycleDays(t.Days, 0)) || !validLifecycleDate(t.Date) {
			return malformedXML()
		}

		if !transitionClasses[common.Value(t.StorageClass)] {
			return malformedXML()
		}
	}

	for _, t := range r.NoncurrentVersionTransitions {
		if !transitionClasses[common.Value(t.StorageClass)] || !validLifecycleDays(t.NoncurrentDays, 1) ||
			!validRetainedVersions(r.Filter, t.NewerNoncurrentVersions) {
			return malformedXML()
		}
	}

	if e := r.NoncurrentVersionExpiration; e != nil && (!validLifecycleDays(e.NoncurrentDays, 1) ||
		!validRetainedVersions(r.Filter, e.NewerNoncurrentVersions)) {
		return invalidArgument("'NoncurrentDays' for NoncurrentVersionExpiration action must be a positive integer")
	}

	if a := r.AbortIncompleteMultipartUpload; a != nil && !validLifecycleDays(a.DaysAfterInitiation, 1) {
		return invalidArgument(
			"'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a positive integer",
		)
	}

	return nil
}

func validLifecycleDays(days *int64, minDays int64) bool {
	return days != nil && *days >= minDays && *days <= math.MaxInt32
}

func validLifecycleDate(t *time.Time) bool {
	return t == nil || t.Equal(t.UTC().Truncate(lifecycleDay))
}

func validRetainedVersions(filter *api.LifecycleRuleFilter, retained *int64) bool {
	return retained == nil || (filter != nil && *retained >= 1 && *retained <= 100)
}

func checkLifecycleFilter(r *api.LifecycleRule) error {
	if len(common.Value(r.Prefix)) > maxKeyBytes {
		return malformedXML()
	}

	f := r.Filter
	if f == nil {
		return nil
	}

	if count(
		f.Prefix != nil,
		f.Tag != nil,
		f.ObjectSizeGreaterThan != nil,
		f.ObjectSizeLessThan != nil,
		f.And != nil,
	) > 1 {
		return malformedXML()
	}

	prefix, low, high := f.Prefix, f.ObjectSizeGreaterThan, f.ObjectSizeLessThan
	var tags []api.Tag
	if f.Tag != nil {
		tags = []api.Tag{*f.Tag}
	}

	if a := f.And; a != nil {
		prefix, low, high, tags = a.Prefix, a.ObjectSizeGreaterThan, a.ObjectSizeLessThan, a.Tags
		if count(prefix != nil, low != nil, high != nil)+len(tags) < 2 {
			return malformedXML()
		}
	}

	const maximumS3ObjectSize = 5 << 40
	if len(common.Value(prefix)) > maxKeyBytes ||
		(low != nil && (*low < 0 || *low > maximumS3ObjectSize)) ||
		(high != nil && (*high < 0 || *high > maximumS3ObjectSize || (low != nil && *high <= *low))) {
		return invalidArgument("Invalid lifecycle filter prefix or object size")
	}

	if len(tags) > 0 || low != nil || high != nil {
		if r.AbortIncompleteMultipartUpload != nil ||
			(r.Expiration != nil && common.Value(r.Expiration.ExpiredObjectDeleteMarker)) {
			return invalidArgument("Upload abort and expired delete marker actions require a prefix or empty filter")
		}
	}

	return configurationTags(tags)
}

func (s *Service) PutBucketLifecycleConfiguration(
	ctx context.Context,
	in *api.PutBucketLifecycleConfigurationRequest,
) (*api.PutBucketLifecycleConfigurationOutput, error) {
	cfg := in.LifecycleConfiguration
	if cfg == nil || len(cfg.Rules) == 0 || len(cfg.Rules) > maxLifecycleRules {
		return nil, malformedXML()
	}

	rules := slices.Clone(cfg.Rules)
	ids := map[string]bool{}
	for i := range rules {
		if rules[i].ID == nil {
			rules[i].ID = new(rand.Text())
		}

		if err := checkLifecycleRule(&rules[i], ids); err != nil {
			return nil, err
		}
	}

	size := common.Value(in.TransitionDefaultMinimumObjectSize)
	switch size {
	case "":
		size = minimumSize128K
	case minimumSize128K, "varies_by_storage_class":
	default:
		return nil, invalidArgument("Invalid TransitionDefaultMinimumObjectSize " + size)
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Lifecycle = &lifecycle{Rules: rules, MinimumSize: size}
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketLifecycleConfigurationOutput{TransitionDefaultMinimumObjectSize: new(size)}, nil
}

func (s *Service) GetBucketLifecycleConfiguration(
	ctx context.Context,
	in *api.GetBucketLifecycleConfigurationRequest,
) (*api.GetBucketLifecycleConfigurationOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if meta.Lifecycle == nil {
		return nil, fail(http.StatusNotFound, "NoSuchLifecycleConfiguration",
			"The lifecycle configuration does not exist")
	}

	return &api.GetBucketLifecycleConfigurationOutput{
		Rules: meta.Lifecycle.Rules, TransitionDefaultMinimumObjectSize: new(meta.Lifecycle.MinimumSize),
	}, nil
}

func (s *Service) DeleteBucketLifecycle(
	ctx context.Context,
	in *api.DeleteBucketLifecycleRequest,
) (*api.DeleteBucketLifecycleOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Lifecycle = nil })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketLifecycleOutput{}, nil
}

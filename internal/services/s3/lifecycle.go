package s3

import (
	"context"
	"net/http"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
)

const (
	maxLifecycleRules = 1000
	// minimumSize128K is the default TransitionDefaultMinimumObjectSize.
	minimumSize128K = "all_storage_classes_128K"
)

var transitionClasses = map[string]bool{
	"GLACIER": true, "STANDARD_IA": true, "ONEZONE_IA": true, "INTELLIGENT_TIERING": true, "DEEP_ARCHIVE": true,
	"GLACIER_IR": true,
}

// lifecycle is a bucket's stored lifecycle configuration. TinyStack keeps the
// rules but never applies them: no object expires or changes storage class.
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
	id := value(r.ID)
	switch {
	case len(id) > 255:
		return invalidArgument("ID length should not exceed allowed limit of 255")
	case id != "" && ids[id]:
		return invalidArgument("Rule ID must be unique. Found same ID for more than one rule")
	case value(r.Status) != "Enabled" && value(r.Status) != "Disabled":
		return malformedXML()
	case r.Prefix != nil && r.Filter != nil:
		return malformedXML()
	case r.Expiration == nil && len(r.Transitions) == 0 && r.NoncurrentVersionExpiration == nil &&
		len(r.NoncurrentVersionTransitions) == 0 && r.AbortIncompleteMultipartUpload == nil:
		return invalidArgument("At least one action needs to be specified in a rule")
	}

	ids[id] = true
	if f := r.Filter; f != nil &&
		count(
			f.Prefix != nil,
			f.Tag != nil,
			f.ObjectSizeGreaterThan != nil,
			f.ObjectSizeLessThan != nil,
			f.And != nil,
		) > 1 {
		return malformedXML()
	}

	if e := r.Expiration; e != nil {
		if count(e.Date != nil, e.Days != nil, e.ExpiredObjectDeleteMarker != nil) != 1 {
			return malformedXML()
		}

		if e.Days != nil && *e.Days <= 0 {
			return invalidArgument("'Days' for Expiration action must be a positive integer")
		}
	}

	for _, t := range r.Transitions {
		if count(t.Date != nil, t.Days != nil) != 1 || value(t.Days) < 0 {
			return malformedXML()
		}

		if !transitionClasses[value(t.StorageClass)] {
			return malformedXML()
		}
	}

	for _, t := range r.NoncurrentVersionTransitions {
		if !transitionClasses[value(t.StorageClass)] || value(t.NoncurrentDays) < 0 {
			return malformedXML()
		}
	}

	if e := r.NoncurrentVersionExpiration; e != nil && value(e.NoncurrentDays) <= 0 {
		return invalidArgument("'NoncurrentDays' for NoncurrentVersionExpiration action must be a positive integer")
	}

	if a := r.AbortIncompleteMultipartUpload; a != nil && value(a.DaysAfterInitiation) <= 0 {
		return invalidArgument(
			"'DaysAfterInitiation' for AbortIncompleteMultipartUpload action must be a positive integer",
		)
	}

	return nil
}

func (s *Service) PutBucketLifecycleConfiguration(
	ctx context.Context,
	in *api.PutBucketLifecycleConfigurationRequest,
) (*api.PutBucketLifecycleConfigurationOutput, error) {
	cfg := in.LifecycleConfiguration
	if cfg == nil || len(cfg.Rules) == 0 || len(cfg.Rules) > maxLifecycleRules {
		return nil, malformedXML()
	}

	ids := map[string]bool{}
	for i := range cfg.Rules {
		if err := checkLifecycleRule(&cfg.Rules[i], ids); err != nil {
			return nil, err
		}
	}

	size := value(in.TransitionDefaultMinimumObjectSize)
	switch size {
	case "":
		size = minimumSize128K
	case minimumSize128K, "varies_by_storage_class":
	default:
		return nil, invalidArgument("Invalid TransitionDefaultMinimumObjectSize " + size)
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Lifecycle = &lifecycle{Rules: cfg.Rules, MinimumSize: size}
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

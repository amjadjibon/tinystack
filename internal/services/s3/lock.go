package s3

import (
	"context"
	"net/http"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	lockModeGovernance = "GOVERNANCE"
	lockModeCompliance = "COMPLIANCE"
	lockLegalHoldOn    = "ON"
	lockLegalHoldOff   = "OFF"
	// lockEnabled is the only ObjectLockEnabled value.
	lockEnabled = "Enabled"
)

func requireObjectLock(meta *bucketMeta) error {
	if meta == nil || meta.ObjectLock == nil {
		return fail(http.StatusBadRequest, "InvalidRequest", "Bucket is missing Object Lock Configuration")
	}

	return nil
}

func (s *Service) PutObjectLockConfiguration(
	ctx context.Context,
	in *api.PutObjectLockConfigurationRequest,
) (*api.PutObjectLockConfigurationOutput, error) {
	if in.ObjectLockConfiguration == nil || common.Value(in.ObjectLockConfiguration.ObjectLockEnabled) != lockEnabled {
		return nil, malformedXML()
	}

	if rule := in.ObjectLockConfiguration.Rule; rule != nil && rule.DefaultRetention != nil {
		def := rule.DefaultRetention
		mode := common.Value(def.Mode)
		if mode != lockModeGovernance && mode != lockModeCompliance {
			return nil, fail(
				http.StatusBadRequest,
				"MalformedXML",
				"Default retention mode must be GOVERNANCE or COMPLIANCE",
			)
		}

		hasDays := def.Days != nil && *def.Days > 0
		hasYears := def.Years != nil && *def.Years > 0
		if (hasDays && hasYears) || (!hasDays && !hasYears) {
			return nil, fail(
				http.StatusBadRequest,
				"InvalidArgument",
				"Default retention rule must specify either Days or Years, but not both",
			)
		}
	}

	err := s.editBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) error {
		// Object Lock can be turned on for an existing bucket once versioning is enabled.
		if meta.ObjectLock == nil && meta.Versioning != versioningEnabled {
			return fail(http.StatusConflict, "InvalidBucketState",
				"Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration")
		}

		meta.ObjectLock = in.ObjectLockConfiguration
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutObjectLockConfigurationOutput{}, nil
}

func (s *Service) GetObjectLockConfiguration(
	ctx context.Context,
	in *api.GetObjectLockConfigurationRequest,
) (*api.GetObjectLockConfigurationOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out api.GetObjectLockConfigurationOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, key)
		if readErr != nil {
			return readErr
		}

		if reqErr := requireObjectLock(meta); reqErr != nil {
			return reqErr
		}

		out.ObjectLockConfiguration = meta.ObjectLock
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func (s *Service) PutObjectRetention(
	ctx context.Context,
	in *api.PutObjectRetentionRequest,
) (*api.PutObjectRetentionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.Retention == nil {
		return nil, malformedXML()
	}

	mode := common.Value(in.Retention.Mode)
	if mode != lockModeGovernance && mode != lockModeCompliance {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Invalid retention mode")
	}

	if in.Retention.RetainUntilDate == nil || !in.Retention.RetainUntilDate.After(s.now()) {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "The retain until date must be in the future")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, key)
		if readErr != nil {
			return readErr
		}

		if reqErr := requireObjectLock(meta); reqErr != nil {
			return reqErr
		}

		versionId := common.Value(in.VersionId)
		obj, _, locateErr := locate(tx, key, *in.Key, versionId)
		if locateErr != nil {
			return locateErr
		}

		switch obj.ObjectLockMode {
		case lockModeCompliance:
			if mode != lockModeCompliance {
				return fail(http.StatusForbidden, "AccessDenied", "Compliance retention mode cannot be changed")
			}

			if obj.ObjectLockRetainUntilDate != nil &&
				in.Retention.RetainUntilDate.Before(*obj.ObjectLockRetainUntilDate) {
				return fail(
					http.StatusForbidden,
					"AccessDenied",
					"Retention period cannot be shortened in COMPLIANCE mode",
				)
			}
		case lockModeGovernance:
			if obj.ObjectLockRetainUntilDate != nil &&
				in.Retention.RetainUntilDate.Before(*obj.ObjectLockRetainUntilDate) {
				if !common.Value(in.BypassGovernanceRetention) {
					return fail(
						http.StatusForbidden,
						"AccessDenied",
						"Governance retention period cannot be shortened "+
							"without bypass governance retention",
					)
				}
			}
		}

		_, rewriteErr := rewrite(tx, key, *in.Key, versionId, func(o *object) {
			o.ObjectLockMode = mode
			o.ObjectLockRetainUntilDate = in.Retention.RetainUntilDate
		})
		return rewriteErr
	})
	if err != nil {
		return nil, err
	}

	return &api.PutObjectRetentionOutput{}, nil
}

func (s *Service) GetObjectRetention(
	ctx context.Context,
	in *api.GetObjectRetentionRequest,
) (*api.GetObjectRetentionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out api.GetObjectRetentionOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, key)
		if readErr != nil {
			return readErr
		}

		if reqErr := requireObjectLock(meta); reqErr != nil {
			return reqErr
		}

		versionId := common.Value(in.VersionId)
		obj, _, locateErr := locate(tx, key, *in.Key, versionId)
		if locateErr != nil {
			return locateErr
		}

		if obj.ObjectLockMode == "" || obj.ObjectLockRetainUntilDate == nil {
			return fail(
				http.StatusNotFound,
				"NoSuchObjectLockConfiguration",
				"The specified object does not have a ObjectLock "+
					"configuration",
			)
		}

		out.Retention = &api.ObjectLockRetention{
			Mode:            new(obj.ObjectLockMode),
			RetainUntilDate: obj.ObjectLockRetainUntilDate,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func (s *Service) PutObjectLegalHold(
	ctx context.Context,
	in *api.PutObjectLegalHoldRequest,
) (*api.PutObjectLegalHoldOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.LegalHold == nil {
		return nil, malformedXML()
	}

	status := common.Value(in.LegalHold.Status)
	if status != lockLegalHoldOn && status != lockLegalHoldOff {
		return nil, fail(http.StatusBadRequest, "InvalidArgument", "Invalid legal hold status")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, key)
		if readErr != nil {
			return readErr
		}

		if reqErr := requireObjectLock(meta); reqErr != nil {
			return reqErr
		}

		versionId := common.Value(in.VersionId)
		_, _, locateErr := locate(tx, key, *in.Key, versionId)
		if locateErr != nil {
			return locateErr
		}

		_, rewriteErr := rewrite(tx, key, *in.Key, versionId, func(o *object) {
			o.ObjectLockLegalHoldStatus = status
		})
		return rewriteErr
	})
	if err != nil {
		return nil, err
	}

	return &api.PutObjectLegalHoldOutput{}, nil
}

func (s *Service) GetObjectLegalHold(
	ctx context.Context,
	in *api.GetObjectLegalHoldRequest,
) (*api.GetObjectLegalHoldOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out api.GetObjectLegalHoldOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, key)
		if readErr != nil {
			return readErr
		}

		if reqErr := requireObjectLock(meta); reqErr != nil {
			return reqErr
		}

		versionId := common.Value(in.VersionId)
		obj, _, locateErr := locate(tx, key, *in.Key, versionId)
		if locateErr != nil {
			return locateErr
		}

		if obj.ObjectLockLegalHoldStatus == "" {
			return fail(
				http.StatusNotFound,
				"NoSuchObjectLockConfiguration",
				"The specified object does not have a ObjectLock "+
					"configuration",
			)
		}

		out.LegalHold = &api.ObjectLockLegalHold{
			Status: new(obj.ObjectLockLegalHoldStatus),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func applyObjectLock(
	meta *bucketMeta,
	obj *object,
	mode *string,
	retainUntil *time.Time,
	legalHold *string,
	now time.Time,
) error {
	hasMode := mode != nil && *mode != ""
	hasRetain := retainUntil != nil && !retainUntil.IsZero()
	hasHold := legalHold != nil && *legalHold != ""

	if (hasMode || hasRetain || hasHold) && (meta == nil || meta.ObjectLock == nil) {
		return fail(http.StatusBadRequest, "InvalidRequest", "Bucket is missing Object Lock Configuration")
	}

	if hasMode != hasRetain {
		return fail(
			http.StatusBadRequest,
			"InvalidArgument",
			"Object lock mode and retain-until-date must both be provided",
		)
	}

	if hasMode {
		m := *mode
		if m != lockModeGovernance && m != lockModeCompliance {
			return fail(http.StatusBadRequest, "InvalidArgument", "Invalid object lock mode")
		}

		if !retainUntil.After(now) {
			return fail(http.StatusBadRequest, "InvalidArgument", "The retain until date must be in the future")
		}

		obj.ObjectLockMode = m
		obj.ObjectLockRetainUntilDate = retainUntil
	} else if meta != nil && meta.ObjectLock != nil && meta.ObjectLock.Rule != nil {
		rule := meta.ObjectLock.Rule.DefaultRetention
		if rule != nil && common.Value(rule.Mode) != "" {
			obj.ObjectLockMode = common.Value(rule.Mode)
			var until time.Time
			if rule.Days != nil && *rule.Days > 0 {
				until = now.Add(time.Duration(*rule.Days) * 24 * time.Hour)
			} else if rule.Years != nil && *rule.Years > 0 {
				until = now.AddDate(int(*rule.Years), 0, 0)
			}

			if !until.IsZero() {
				obj.ObjectLockRetainUntilDate = &until
			}
		}
	}

	if hasHold {
		h := *legalHold
		if h != lockLegalHoldOn && h != lockLegalHoldOff {
			return fail(http.StatusBadRequest, "InvalidArgument", "Invalid legal hold status")
		}

		obj.ObjectLockLegalHoldStatus = h
	}

	return nil
}

func checkLock(obj *object, bypassGovernance bool, now time.Time) error {
	if obj.ObjectLockLegalHoldStatus == lockLegalHoldOn {
		return accessDenied()
	}

	if obj.ObjectLockRetainUntilDate != nil && obj.ObjectLockRetainUntilDate.After(now) {
		if obj.ObjectLockMode == lockModeCompliance {
			return accessDenied()
		}

		if obj.ObjectLockMode == lockModeGovernance && !bypassGovernance {
			return accessDenied()
		}
	}

	return nil
}

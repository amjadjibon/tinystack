package s3_test

import (
	"context"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func newTestService(t *testing.T) *s3.Service {
	t.Helper()
	blobs, err := storage.OpenBlobs(t.TempDir())
	if err != nil {
		t.Fatalf("OpenBlobs: %v", err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	svc := s3.New(memory.New(nil, nil), nil, nil, nil, nil, nil, blobs)
	t.Cleanup(svc.Stop)
	return svc
}

func testContext() context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{
			AccountID: "123456789012",
			Region:    "us-east-1",
		},
	})
}

func TestObjectLockLifecycle(t *testing.T) {
	svc := newTestService(t)
	ctx := testContext()

	// 1. Bucket without object lock
	_, err := svc.CreateBucket(ctx, &api.CreateBucketRequest{
		Bucket: new("plain-bucket"),
	})
	if err != nil {
		t.Fatalf("CreateBucket plain-bucket: %v", err)
	}

	_, err = svc.GetObjectLockConfiguration(ctx, &api.GetObjectLockConfigurationRequest{
		Bucket: new("plain-bucket"),
	})
	if err == nil || awserror.From(err).Code != "ObjectLockConfigurationNotFoundError" {
		t.Fatalf("GetObjectLockConfiguration on plain bucket: expected ObjectLockConfigurationNotFoundError, got %v",
			err)
	}

	// 2. Bucket with object lock enabled
	_, err = svc.CreateBucket(ctx, &api.CreateBucketRequest{
		Bucket:                     new("locked-bucket"),
		ObjectLockEnabledForBucket: new(true),
	})
	if err != nil {
		t.Fatalf("CreateBucket locked-bucket: %v", err)
	}

	// Check versioning is enabled
	vOut, err := svc.GetBucketVersioning(ctx, &api.GetBucketVersioningRequest{
		Bucket: new("locked-bucket"),
	})
	if err != nil {
		t.Fatalf("GetBucketVersioning: %v", err)
	}
	if common.Value(vOut.Status) != "Enabled" {
		t.Fatalf("expected versioning Enabled, got %v", common.Value(vOut.Status))
	}

	// Suspending versioning should fail
	_, err = svc.PutBucketVersioning(ctx, &api.PutBucketVersioningRequest{
		Bucket: new("locked-bucket"),
		VersioningConfiguration: &api.VersioningConfiguration{
			Status: new("Suspended"),
		},
	})
	if err == nil || awserror.From(err).Code != "InvalidBucketState" {
		t.Fatalf("PutBucketVersioning Suspended: expected InvalidBucketState, got %v", err)
	}

	// 3. PutObjectLockConfiguration
	days := int64(30)
	_, err = svc.PutObjectLockConfiguration(ctx, &api.PutObjectLockConfigurationRequest{
		Bucket: new("locked-bucket"),
		ObjectLockConfiguration: &api.ObjectLockConfiguration{
			ObjectLockEnabled: new("Enabled"),
			Rule: &api.ObjectLockRule{
				DefaultRetention: &api.DefaultRetention{
					Mode: new("GOVERNANCE"),
					Days: &days,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("PutObjectLockConfiguration: %v", err)
	}

	cfgOut, err := svc.GetObjectLockConfiguration(ctx, &api.GetObjectLockConfigurationRequest{
		Bucket: new("locked-bucket"),
	})
	if err != nil {
		t.Fatalf("GetObjectLockConfiguration: %v", err)
	}
	if cfgOut.ObjectLockConfiguration == nil || cfgOut.ObjectLockConfiguration.Rule == nil ||
		common.Value(cfgOut.ObjectLockConfiguration.Rule.DefaultRetention.Mode) != "GOVERNANCE" {
		t.Fatalf("unexpected GetObjectLockConfiguration output: %+v", cfgOut)
	}

	// 4. PutObject inherits default retention
	putOut, err := svc.PutObject(ctx, &api.PutObjectRequest{
		Bucket: new("locked-bucket"),
		Key:    new("obj-default"),
		Body:   []byte("hello default retention"),
	})
	if err != nil {
		t.Fatalf("PutObject obj-default: %v", err)
	}
	defaultVersion := common.Value(putOut.VersionId)

	headOut, err := svc.HeadObject(ctx, &api.HeadObjectRequest{
		Bucket:    new("locked-bucket"),
		Key:       new("obj-default"),
		VersionId: &defaultVersion,
	})
	if err != nil {
		t.Fatalf("HeadObject obj-default: %v", err)
	}
	if common.Value(headOut.ObjectLockMode) != "GOVERNANCE" || headOut.ObjectLockRetainUntilDate == nil {
		t.Fatalf("obj-default did not inherit default retention: mode=%v, until=%v",
			common.Value(headOut.ObjectLockMode), headOut.ObjectLockRetainUntilDate)
	}

	// 5. Deleting version without bypass fails AccessDenied
	_, err = svc.DeleteObject(ctx, &api.DeleteObjectRequest{
		Bucket:    new("locked-bucket"),
		Key:       new("obj-default"),
		VersionId: &defaultVersion,
	})
	if err == nil || awserror.From(err).Code != "AccessDenied" {
		t.Fatalf("DeleteObject without bypass: expected AccessDenied, got %v", err)
	}

	// Deleting version with bypass succeeds
	_, err = svc.DeleteObject(ctx, &api.DeleteObjectRequest{
		Bucket:                    new("locked-bucket"),
		Key:                       new("obj-default"),
		VersionId:                 &defaultVersion,
		BypassGovernanceRetention: new(true),
	})
	if err != nil {
		t.Fatalf("DeleteObject with bypass: expected success, got %v", err)
	}

	// 6. COMPLIANCE mode retention cannot be shortened
	futureDate := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	putOutComp, err := svc.PutObject(ctx, &api.PutObjectRequest{
		Bucket:                    new("locked-bucket"),
		Key:                       new("obj-compliance"),
		Body:                      []byte("hello compliance"),
		ObjectLockMode:            new("COMPLIANCE"),
		ObjectLockRetainUntilDate: &futureDate,
	})
	if err != nil {
		t.Fatalf("PutObject obj-compliance: %v", err)
	}
	compVersion := common.Value(putOutComp.VersionId)

	shorterDate := futureDate.Add(-2 * 24 * time.Hour)
	_, err = svc.PutObjectRetention(ctx, &api.PutObjectRetentionRequest{
		Bucket:    new("locked-bucket"),
		Key:       new("obj-compliance"),
		VersionId: &compVersion,
		Retention: &api.ObjectLockRetention{
			Mode:            new("COMPLIANCE"),
			RetainUntilDate: &shorterDate,
		},
	})
	if err == nil || awserror.From(err).Code != "AccessDenied" {
		t.Fatalf("PutObjectRetention shorten compliance: expected AccessDenied, got %v", err)
	}

	// Trying to switch mode from COMPLIANCE to GOVERNANCE fails
	_, err = svc.PutObjectRetention(ctx, &api.PutObjectRetentionRequest{
		Bucket:    new("locked-bucket"),
		Key:       new("obj-compliance"),
		VersionId: &compVersion,
		Retention: &api.ObjectLockRetention{
			Mode:            new("GOVERNANCE"),
			RetainUntilDate: &futureDate,
		},
	})
	if err == nil || awserror.From(err).Code != "AccessDenied" {
		t.Fatalf("PutObjectRetention change compliance mode: expected AccessDenied, got %v", err)
	}

	// Even with bypass, COMPLIANCE cannot be deleted
	_, err = svc.DeleteObject(ctx, &api.DeleteObjectRequest{
		Bucket:                    new("locked-bucket"),
		Key:                       new("obj-compliance"),
		VersionId:                 &compVersion,
		BypassGovernanceRetention: new(true),
	})
	if err == nil || awserror.From(err).Code != "AccessDenied" {
		t.Fatalf("DeleteObject compliance with bypass: expected AccessDenied, got %v", err)
	}

	// 7. Legal Hold
	_, err = svc.PutObjectLegalHold(ctx, &api.PutObjectLegalHoldRequest{
		Bucket:    new("locked-bucket"),
		Key:       new("obj-compliance"),
		VersionId: &compVersion,
		LegalHold: &api.ObjectLockLegalHold{
			Status: new("ON"),
		},
	})
	if err != nil {
		t.Fatalf("PutObjectLegalHold ON: %v", err)
	}

	holdOut, err := svc.GetObjectLegalHold(ctx, &api.GetObjectLegalHoldRequest{
		Bucket:    new("locked-bucket"),
		Key:       new("obj-compliance"),
		VersionId: &compVersion,
	})
	if err != nil {
		t.Fatalf("GetObjectLegalHold: %v", err)
	}
	if common.Value(holdOut.LegalHold.Status) != "ON" {
		t.Fatalf("expected ON, got %v", common.Value(holdOut.LegalHold.Status))
	}

	// Delete without versionId just creates a delete marker (allowed)
	delOut, err := svc.DeleteObject(ctx, &api.DeleteObjectRequest{
		Bucket: new("locked-bucket"),
		Key:    new("obj-compliance"),
	})
	if err != nil {
		t.Fatalf("DeleteObject without versionId: %v", err)
	}
	if !common.Value(delOut.DeleteMarker) {
		t.Fatalf("expected DeleteMarker true, got false")
	}

	// 8. DeleteObjects with locked version records error and continues
	delObjsOut, err := svc.DeleteObjects(ctx, &api.DeleteObjectsRequest{
		Bucket: new("locked-bucket"),
		Delete: &api.Delete{
			Objects: []api.ObjectIdentifier{
				{Key: new("obj-compliance"), VersionId: &compVersion},
				{Key: new("non-existent-key")},
			},
		},
	})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(delObjsOut.Errors) != 1 || common.Value(delObjsOut.Errors[0].Code) != "AccessDenied" {
		t.Fatalf("expected 1 AccessDenied error, got %+v", delObjsOut.Errors)
	}
}

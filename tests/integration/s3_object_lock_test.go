package integration_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3ObjectLock(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	// 1. Bucket without Object Lock
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("plain-bucket")}); err != nil {
		t.Fatal(err)
	}

	_, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{
		Bucket: aws.String("plain-bucket"),
	})
	if code, _ := s3Code(err); code != "ObjectLockConfigurationNotFoundError" {
		t.Fatalf("expected ObjectLockConfigurationNotFoundError on plain bucket, got %q (%v)", code, err)
	}

	// 2. Bucket with Object Lock enabled
	if _, lockCreateErr := c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket:                     aws.String("lock-bucket"),
		ObjectLockEnabledForBucket: aws.Bool(true),
	}); lockCreateErr != nil {
		t.Fatal(lockCreateErr)
	}

	vOut, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("lock-bucket")})
	if err != nil {
		t.Fatal(err)
	}

	if vOut.Status != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("expected Enabled versioning on locked bucket, got %v", vOut.Status)
	}

	// Suspending versioning on locked bucket fails
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String("lock-bucket"),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusSuspended,
		},
	})
	if code, _ := s3Code(err); code != "InvalidBucketState" {
		t.Fatalf("expected InvalidBucketState when suspending locked bucket, got %q (%v)", code, err)
	}

	// 3. PutObjectLockConfiguration
	_, err = c.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String("lock-bucket"),
		ObjectLockConfiguration: &s3types.ObjectLockConfiguration{
			ObjectLockEnabled: s3types.ObjectLockEnabledEnabled,
			Rule: &s3types.ObjectLockRule{
				DefaultRetention: &s3types.DefaultRetention{
					Mode: s3types.ObjectLockRetentionModeGovernance,
					Days: aws.Int32(30),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("PutObjectLockConfiguration: %v", err)
	}

	cfgOut, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{
		Bucket: aws.String("lock-bucket"),
	})
	if err != nil {
		t.Fatalf("GetObjectLockConfiguration: %v", err)
	}

	if cfgOut.ObjectLockConfiguration == nil || cfgOut.ObjectLockConfiguration.Rule == nil ||
		cfgOut.ObjectLockConfiguration.Rule.DefaultRetention.Mode != s3types.ObjectLockRetentionModeGovernance {
		t.Fatalf("unexpected GetObjectLockConfiguration output: %+v", cfgOut.ObjectLockConfiguration)
	}

	// 4. Object inherits bucket default retention
	putOut, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("lock-bucket"),
		Key:    aws.String("file-default"),
		Body:   strings.NewReader("content"),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	versionID := putOut.VersionId

	headOut, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-default"),
		VersionId: versionID,
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if headOut.ObjectLockMode != s3types.ObjectLockModeGovernance || headOut.ObjectLockRetainUntilDate == nil {
		t.Fatalf("expected GOVERNANCE mode and retain date, got %v, %v",
			headOut.ObjectLockMode, headOut.ObjectLockRetainUntilDate)
	}

	getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-default"),
		VersionId: versionID,
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}

	_, _ = io.Copy(io.Discard, getOut.Body)
	_ = getOut.Body.Close()

	if getOut.ObjectLockMode != s3types.ObjectLockModeGovernance || getOut.ObjectLockRetainUntilDate == nil {
		t.Fatalf("expected GOVERNANCE mode on GetObject, got %v", getOut.ObjectLockMode)
	}

	// 5. Blocked delete without bypass
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-default"),
		VersionId: versionID,
	})
	if code, _ := s3Code(err); code != "AccessDenied" {
		t.Fatalf("expected AccessDenied deleting locked version, got %q (%v)", code, err)
	}

	// Governance bypass allows deletion
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:                    aws.String("lock-bucket"),
		Key:                       aws.String("file-default"),
		VersionId:                 versionID,
		BypassGovernanceRetention: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("DeleteObject with bypass failed: %v", err)
	}

	// 6. COMPLIANCE mode non-shortening
	retainDate := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	putComp, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:                    aws.String("lock-bucket"),
		Key:                       aws.String("file-comp"),
		Body:                      strings.NewReader("compliance body"),
		ObjectLockMode:            s3types.ObjectLockModeCompliance,
		ObjectLockRetainUntilDate: &retainDate,
	})
	if err != nil {
		t.Fatal(err)
	}

	compVersion := putComp.VersionId

	shorterDate := retainDate.Add(-2 * 24 * time.Hour)
	_, err = c.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-comp"),
		VersionId: compVersion,
		Retention: &s3types.ObjectLockRetention{
			Mode:            s3types.ObjectLockRetentionModeCompliance,
			RetainUntilDate: &shorterDate,
		},
	})
	if code, _ := s3Code(err); code != "AccessDenied" {
		t.Fatalf("expected AccessDenied shortening compliance retention, got %q (%v)", code, err)
	}

	// Changing compliance mode to governance fails
	_, err = c.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-comp"),
		VersionId: compVersion,
		Retention: &s3types.ObjectLockRetention{
			Mode:            s3types.ObjectLockRetentionModeGovernance,
			RetainUntilDate: &retainDate,
		},
	})
	if code, _ := s3Code(err); code != "AccessDenied" {
		t.Fatalf("expected AccessDenied changing compliance mode, got %q (%v)", code, err)
	}

	// Legal hold
	_, err = c.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-comp"),
		VersionId: compVersion,
		LegalHold: &s3types.ObjectLockLegalHold{
			Status: s3types.ObjectLockLegalHoldStatusOn,
		},
	})
	if err != nil {
		t.Fatalf("PutObjectLegalHold: %v", err)
	}

	holdOut, err := c.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{
		Bucket:    aws.String("lock-bucket"),
		Key:       aws.String("file-comp"),
		VersionId: compVersion,
	})
	if err != nil {
		t.Fatalf("GetObjectLegalHold: %v", err)
	}

	if holdOut.LegalHold == nil || holdOut.LegalHold.Status != s3types.ObjectLockLegalHoldStatusOn {
		t.Fatalf("expected LegalHold Status ON, got %+v", holdOut.LegalHold)
	}

	// Delete without versionId adds delete marker (allowed)
	delOut, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("lock-bucket"),
		Key:    aws.String("file-comp"),
	})
	if err != nil {
		t.Fatalf("DeleteObject without versionId: %v", err)
	}

	if !aws.ToBool(delOut.DeleteMarker) {
		t.Fatalf("expected DeleteMarker to be true")
	}

	// DeleteObjects with versionId fails with AccessDenied
	delObjsOut, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("lock-bucket"),
		Delete: &s3types.Delete{
			Objects: []s3types.ObjectIdentifier{
				{Key: aws.String("file-comp"), VersionId: compVersion},
			},
		},
	})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}

	if len(delObjsOut.Errors) != 1 || aws.ToString(delObjsOut.Errors[0].Code) != "AccessDenied" {
		t.Fatalf("expected AccessDenied in DeleteObjects errors, got %+v", delObjsOut.Errors)
	}
}

func TestS3ObjectLockOnExistingBucket(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("later-lock")}); err != nil {
		t.Fatal(err)
	}

	enable := &s3.PutObjectLockConfigurationInput{
		Bucket:                  aws.String("later-lock"),
		ObjectLockConfiguration: &s3types.ObjectLockConfiguration{ObjectLockEnabled: s3types.ObjectLockEnabledEnabled},
	}
	_, err := c.PutObjectLockConfiguration(ctx, enable)
	if code, _ := s3Code(err); code != "InvalidBucketState" {
		t.Fatalf("Object Lock without versioning: %q (%v)", code, err)
	}

	// Object Lock can be turned on once versioning is enabled.
	if _, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String("later-lock"),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutObjectLockConfiguration(ctx, enable); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("later-lock")})
	if err != nil || got.ObjectLockConfiguration.ObjectLockEnabled != s3types.ObjectLockEnabledEnabled {
		t.Fatalf("%+v %v", got, err)
	}
}

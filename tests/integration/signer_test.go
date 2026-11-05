package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/signer"
	"github.com/aws/aws-sdk-go-v2/service/signer/types"
	"github.com/aws/smithy-go"
)

func signerClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func signerError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestSignerProfiles(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := signerClient(s, "test", "us-east-1")
	put, err := c.PutSigningProfile(ctx, &sdk.PutSigningProfileInput{
		ProfileName: new("ProfileA"),
		PlatformId:  new("AWSLambda-SHA384-ECDSA"), Tags: map[string]string{"team": "core"},
		SignatureValidityPeriod: &types.SignatureValidityPeriod{Type: types.ValidityTypeDays, Value: 10},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(*put.ProfileVersionArn, "/"+*put.ProfileVersion) {
		t.Fatal(put)
	}

	get, err := c.GetSigningProfile(ctx, &sdk.GetSigningProfileInput{ProfileName: new("ProfileA")})
	if err != nil || get.Status != types.SigningProfileStatusActive ||
		get.SignatureValidityPeriod.Type != types.ValidityTypeDays {
		t.Fatalf("get: %#v %v", get, err)
	}

	version2, err := c.PutSigningProfile(
		ctx,
		&sdk.PutSigningProfileInput{ProfileName: new("ProfileA"), PlatformId: new("AWSLambda-SHA384-ECDSA")},
	)
	if err != nil || *version2.ProfileVersion == *put.ProfileVersion {
		t.Fatalf("version: %#v %v", version2, err)
	}

	_, err = c.RevokeSigningProfile(ctx, &sdk.RevokeSigningProfileInput{
		ProfileName:    new("ProfileA"),
		ProfileVersion: put.ProfileVersion, Reason: new("old version"), EffectiveTime: new(time.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}

	get, err = c.GetSigningProfile(ctx, &sdk.GetSigningProfileInput{ProfileName: new("ProfileA")})
	if err != nil || get.Status != types.SigningProfileStatusActive {
		t.Fatalf("old revocation: %#v %v", get, err)
	}

	for _, name := range []string{"ProfileB", "ProfileC"} {
		_, err = c.PutSigningProfile(
			ctx,
			&sdk.PutSigningProfileInput{ProfileName: new(name), PlatformId: new("AWSLambda-SHA384-ECDSA")},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CancelSigningProfile(ctx, &sdk.CancelSigningProfileInput{ProfileName: new("ProfileB")})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListSigningProfiles(
		ctx,
		&sdk.ListSigningProfilesInput{MaxResults: new(int32(1)), IncludeCanceled: true},
	)
	if err != nil || len(page.Profiles) != 1 || page.NextToken == nil {
		t.Fatalf("page: %#v %v", page, err)
	}

	next, err := c.ListSigningProfiles(
		ctx,
		&sdk.ListSigningProfilesInput{MaxResults: new(int32(1)), IncludeCanceled: true, NextToken: page.NextToken},
	)
	if err != nil || *next.Profiles[0].ProfileName == *page.Profiles[0].ProfileName {
		t.Fatalf("next: %#v %v", next, err)
	}

	list, err := c.ListSigningProfiles(ctx, &sdk.ListSigningProfilesInput{})
	if err != nil || len(list.Profiles) != 2 {
		t.Fatalf("list: %#v %v", list, err)
	}

	list, err = c.ListSigningProfiles(
		ctx,
		&sdk.ListSigningProfilesInput{Statuses: []types.SigningProfileStatus{types.SigningProfileStatusCanceled}},
	)
	if err != nil || len(list.Profiles) != 1 {
		t.Fatalf("canceled: %#v %v", list, err)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: put.Arn, Tags: map[string]string{"owner": "test"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: put.Arn})
	if err != nil || tags.Tags["owner"] != "test" {
		t.Fatalf("tags: %#v %v", tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: put.Arn, TagKeys: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: put.Arn})
	if err != nil || len(tags.Tags) != 0 {
		t.Fatalf("untag: %#v %v", tags, err)
	}

	taggingClient := resourcegroupstaggingapi.New(resourcegroupstaggingapi.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"),
	})
	tagged, err := taggingClient.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{*put.Arn}, Tags: map[string]string{"shared": "yes"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("shared tags: %#v %v", tagged, err)
	}

	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: put.Arn})
	if err != nil || tags.Tags["shared"] != "yes" {
		t.Fatalf("shared tag read: %#v %v", tags, err)
	}

	_, err = c.RevokeSigningProfile(ctx, &sdk.RevokeSigningProfileInput{
		ProfileName:    new("ProfileC"),
		ProfileVersion: new("missing"), Reason: new("test"), EffectiveTime: new(time.Now()),
	})
	signerError(t, err, "ResourceNotFoundException")
	current, err := c.GetSigningProfile(ctx, &sdk.GetSigningProfileInput{ProfileName: new("ProfileC")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RevokeSigningProfile(ctx, &sdk.RevokeSigningProfileInput{
		ProfileName:    new("ProfileC"),
		ProfileVersion: current.ProfileVersion, Reason: new("test"), EffectiveTime: new(time.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}

	current, err = c.GetSigningProfile(ctx, &sdk.GetSigningProfileInput{ProfileName: new("ProfileC")})
	if err != nil || current.Status != types.SigningProfileStatusRevoked || current.RevocationRecord == nil {
		t.Fatalf("revoke: %#v %v", current, err)
	}

	for _, other := range []*sdk.Client{
		signerClient(s, "test", "us-west-2"),
		signerClient(s, "111111111111", "us-east-1"),
	} {
		isolated, e := other.ListSigningProfiles(ctx, &sdk.ListSigningProfilesInput{IncludeCanceled: true})
		if e != nil || len(isolated.Profiles) != 0 {
			t.Fatalf("isolation: %#v %v", isolated, e)
		}

		_, e = other.GetSigningProfile(ctx, &sdk.GetSigningProfileInput{ProfileName: new("ProfileA")})
		signerError(t, e, "ResourceNotFoundException")
	}

	_, err = c.PutSigningProfile(
		ctx,
		&sdk.PutSigningProfileInput{ProfileName: new("bad-name"), PlatformId: new("AWSLambda-SHA384-ECDSA")},
	)
	signerError(t, err, "ValidationException")
	_, err = c.ListSigningProfiles(ctx, &sdk.ListSigningProfilesInput{NextToken: new("!bad")})
	signerError(t, err, "ValidationException")
	_, err = c.PutSigningProfile(
		ctx,
		&sdk.PutSigningProfileInput{
			ProfileName: new("IoTProfile"),
			PlatformId:  new("AWSIoTDeviceManagement-SHA256-ECDSA"),
		},
	)
	signerError(t, err, "ValidationException")
	ac := acm.New(acm.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	cert, err := ac.RequestCertificate(ctx, &acm.RequestCertificateInput{DomainName: new("signer.example.com")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutSigningProfile(ctx, &sdk.PutSigningProfileInput{
		ProfileName: new("IoTProfile"), PlatformId: new("AWSIoTDeviceManagement-SHA256-ECDSA"),
		SigningMaterial: &types.SigningMaterial{CertificateArn: cert.CertificateArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = signerClient(s, "test", "us-west-2").PutSigningProfile(ctx, &sdk.PutSigningProfileInput{
		ProfileName: new("IoTProfile"),
		PlatformId: new(
			"AWSIoTDeviceManagement-SHA256-ECDSA",
		),
		SigningMaterial: &types.SigningMaterial{CertificateArn: cert.CertificateArn},
	})
	signerError(t, err, "ValidationException")
}

func TestSignerPermissionsAndPlatforms(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := signerClient(s, "test", "us-east-1")
	platforms, err := c.ListSigningPlatforms(ctx, &sdk.ListSigningPlatformsInput{})
	if err != nil || len(platforms.Platforms) != 5 {
		t.Fatalf("platforms: %#v %v", platforms, err)
	}

	filtered, err := c.ListSigningPlatforms(ctx, &sdk.ListSigningPlatformsInput{Partner: new("AWSLambda")})
	if err != nil || len(filtered.Platforms) != 1 {
		t.Fatalf("platform filter: %#v %v", filtered, err)
	}

	_, err = c.ListSigningPlatforms(ctx, &sdk.ListSigningPlatformsInput{MaxResults: new(int32(26))})
	signerError(t, err, "ValidationException")

	for _, p := range platforms.Platforms {
		detail, e := c.GetSigningPlatform(ctx, &sdk.GetSigningPlatformInput{PlatformId: p.PlatformId})
		if e != nil || detail.SigningConfiguration == nil || detail.SigningImageFormat == nil {
			t.Fatalf("platform: %#v %v", detail, e)
		}
	}

	p, err := c.PutSigningProfile(
		ctx,
		&sdk.PutSigningProfileInput{ProfileName: new("Permissions"), PlatformId: new("AWSLambda-SHA384-ECDSA")},
	)
	if err != nil {
		t.Fatal(err)
	}

	add := &sdk.AddProfilePermissionInput{
		ProfileName: new("Permissions"), StatementId: new("allow"),
		Action: new("signer:StartSigningJob"), Principal: new("123456789012"), ProfileVersion: p.ProfileVersion,
	}

	permission, err := c.AddProfilePermission(ctx, add)
	if err != nil {
		t.Fatal(err)
	}

	add.StatementId = new("second")
	add.RevisionId = new("stale")
	_, err = c.AddProfilePermission(ctx, add)
	signerError(t, err, "ConflictException")
	add.RevisionId = permission.RevisionId
	second, err := c.AddProfilePermission(ctx, add)
	if err != nil {
		t.Fatal(err)
	}

	list, err := c.ListProfilePermissions(ctx, &sdk.ListProfilePermissionsInput{ProfileName: new("Permissions")})
	if err != nil || len(list.Permissions) != 2 || *list.RevisionId != *second.RevisionId {
		t.Fatalf("permissions: %#v %v", list, err)
	}

	_, err = c.RemoveProfilePermission(
		ctx,
		&sdk.RemoveProfilePermissionInput{
			ProfileName: new("Permissions"),
			StatementId: new("allow"),
			RevisionId:  permission.RevisionId,
		},
	)
	signerError(t, err, "ConflictException")
	removed, err := c.RemoveProfilePermission(
		ctx,
		&sdk.RemoveProfilePermissionInput{
			ProfileName: new("Permissions"),
			StatementId: new("allow"),
			RevisionId:  list.RevisionId,
		},
	)
	if err != nil || *removed.RevisionId == *list.RevisionId {
		t.Fatalf("remove: %#v %v", removed, err)
	}
}

func TestSignerJobs(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := signerClient(s, "test", "us-east-1")
	objects := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	for _, name := range []string{"signer-source", "signer-destination"} {
		_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := objects.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  new("signer-source"),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		t.Fatal(err)
	}

	object, err := objects.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("signer-source"),
			Key:    new("code.zip"),
			Body:   bytes.NewReader([]byte("source bytes")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutSigningProfile(
		ctx,
		&sdk.PutSigningProfileInput{ProfileName: new("JobProfile"), PlatformId: new("AWSLambda-SHA384-ECDSA")},
	)
	if err != nil {
		t.Fatal(err)
	}

	input := &sdk.StartSigningJobInput{
		ProfileName:        new("JobProfile"),
		ClientRequestToken: new("token"),
		Source: &types.Source{
			S3: &types.S3Source{BucketName: new("signer-source"), Key: new("code.zip"), Version: object.VersionId},
		},
		Destination: &types.Destination{
			S3: &types.S3Destination{BucketName: new("signer-destination"), Prefix: new("signed/")},
		},
	}

	started, err := c.StartSigningJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := c.StartSigningJob(ctx, input)
	if err != nil || *replay.JobId != *started.JobId {
		t.Fatalf("replay: %#v %v", replay, err)
	}

	changed := *input
	changed.ProfileName = new("changed")
	_, err = c.StartSigningJob(ctx, &changed)
	wantAccessAnalyzerError(t, err, "ConflictException")
	detail, err := c.DescribeSigningJob(ctx, &sdk.DescribeSigningJobInput{JobId: started.JobId})
	if err != nil || detail.Status != types.SigningStatusSucceeded || detail.SignedObject == nil {
		t.Fatalf("job: %#v %v", detail, err)
	}

	signed, err := objects.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: detail.SignedObject.S3.BucketName, Key: detail.SignedObject.S3.Key},
	)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(signed.Body)
	signed.Body.Close()
	if err != nil || !bytes.HasPrefix(data, []byte("source bytes")) ||
		!bytes.Contains(data, []byte("NOT CRYPTOGRAPHIC")) {
		t.Fatalf("artifact: %s %v", data, err)
	}

	_, err = c.RevokeSignature(ctx, &sdk.RevokeSignatureInput{JobId: started.JobId, Reason: new("test")})
	if err != nil {
		t.Fatal(err)
	}

	list, err := c.ListSigningJobs(ctx, &sdk.ListSigningJobsInput{IsRevoked: true})
	if err != nil || len(list.Jobs) != 1 {
		t.Fatalf("revoked: %#v %v", list, err)
	}

	input.Source.S3.Version = new("missing")
	_, err = c.StartSigningJob(ctx, input)
	signerError(t, err, "ConflictException")
	input.ClientRequestToken = new("missing-object")
	failed, err := c.StartSigningJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	detail, err = c.DescribeSigningJob(ctx, &sdk.DescribeSigningJobInput{JobId: failed.JobId})
	if err != nil || detail.Status != types.SigningStatusFailed || detail.SignedObject != nil {
		t.Fatalf("failed: %#v %v", detail, err)
	}

	page, err := c.ListSigningJobs(ctx, &sdk.ListSigningJobsInput{MaxResults: new(int32(1))})
	if err != nil || page.NextToken == nil {
		t.Fatalf("jobs page: %#v %v", page, err)
	}

	_, err = signerClient(
		s,
		"111111111111",
		"us-east-1",
	).DescribeSigningJob(ctx, &sdk.DescribeSigningJobInput{JobId: started.JobId})
	signerError(t, err, "ResourceNotFoundException")
	_, err = c.CancelSigningProfile(ctx, &sdk.CancelSigningProfileInput{ProfileName: new("JobProfile")})
	if err != nil {
		t.Fatal(err)
	}

	input.ClientRequestToken = new("canceled")
	_, err = c.StartSigningJob(ctx, input)
	signerError(t, err, "ConflictException")
}

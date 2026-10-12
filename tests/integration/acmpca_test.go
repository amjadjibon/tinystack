package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/acmpca"
	"github.com/aws/aws-sdk-go-v2/service/acmpca/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func acmpcaClient(s *httptest.Server, key string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func privateCAInput() *sdk.CreateCertificateAuthorityInput {
	return &sdk.CreateCertificateAuthorityInput{
		CertificateAuthorityType: types.CertificateAuthorityTypeRoot,
		CertificateAuthorityConfiguration: &types.CertificateAuthorityConfiguration{
			KeyAlgorithm:     types.KeyAlgorithmRsa2048,
			SigningAlgorithm: types.SigningAlgorithmSha256withrsa,
			Subject: &types.ASN1Subject{
				CommonName: new("root.example.com"), Organization: new("Example"), Country: new("US"),
			},
		},
		Tags: []types.Tag{{Key: new("Environment"), Value: new("Test")}},
	}
}

func TestACMPCASDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := acmpcaClient(s, "test")

	if _, err := s3Client(
		s,
		"test",
		"us-east-1",
	).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("crl-bucket")}); err != nil {
		t.Fatal(err)
	}

	in := privateCAInput()
	in.RevocationConfiguration = &types.RevocationConfiguration{
		CrlConfiguration: &types.CrlConfiguration{
			Enabled: new(true), S3BucketName: new("crl-bucket"), ExpirationInDays: new(int32(30)),
		},
	}
	in.IdempotencyToken = new("lifecycle")

	created, err := c.CreateCertificateAuthority(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	arn := created.CertificateAuthorityArn
	if !strings.HasPrefix(aws.ToString(arn), "arn:aws:acm-pca:us-east-1:") {
		t.Fatalf("unexpected ARN %s", aws.ToString(arn))
	}

	again, err := c.CreateCertificateAuthority(ctx, in)
	if err != nil || aws.ToString(again.CertificateAuthorityArn) != aws.ToString(arn) {
		t.Fatalf("an idempotent create made another authority: %+v, %v", again, err)
	}

	desc, err := c.DescribeCertificateAuthority(
		ctx,
		&sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: arn},
	)
	if err != nil {
		t.Fatal(err)
	}

	ca := desc.CertificateAuthority
	if ca.Status != types.CertificateAuthorityStatusPendingCertificate ||
		ca.Type != types.CertificateAuthorityTypeRoot ||
		ca.UsageMode != types.CertificateAuthorityUsageModeGeneralPurpose ||
		aws.ToString(ca.CertificateAuthorityConfiguration.Subject.CommonName) != "root.example.com" ||
		ca.CertificateAuthorityConfiguration.KeyAlgorithm != types.KeyAlgorithmRsa2048 ||
		aws.ToInt32(ca.RevocationConfiguration.CrlConfiguration.ExpirationInDays) != 30 ||
		ca.CreatedAt == nil {
		t.Fatalf("unexpected authority %+v", ca)
	}

	listed, err := c.ListCertificateAuthorities(ctx, &sdk.ListCertificateAuthoritiesInput{})
	if err != nil || len(listed.CertificateAuthorities) != 1 ||
		aws.ToString(listed.CertificateAuthorities[0].Arn) != aws.ToString(arn) {
		t.Fatalf("unexpected list %+v, %v", listed, err)
	}

	// A pending authority has no certificate: its revocation settings change, its status does not.
	if _, err = c.UpdateCertificateAuthority(ctx, &sdk.UpdateCertificateAuthorityInput{
		CertificateAuthorityArn: arn,
		RevocationConfiguration: &types.RevocationConfiguration{
			OcspConfiguration: &types.OcspConfiguration{Enabled: new(true)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	desc, _ = c.DescribeCertificateAuthority(ctx, &sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: arn})
	if rev := desc.CertificateAuthority.RevocationConfiguration; !aws.ToBool(rev.OcspConfiguration.Enabled) ||
		!aws.ToBool(rev.CrlConfiguration.Enabled) {
		t.Fatalf("unexpected revocation configuration %+v", rev)
	}

	_, err = c.UpdateCertificateAuthority(ctx, &sdk.UpdateCertificateAuthorityInput{
		CertificateAuthorityArn: arn, Status: types.CertificateAuthorityStatusActive,
	})
	var state *types.InvalidStateException
	if !errors.As(err, &state) {
		t.Fatalf("want InvalidStateException, got %v", err)
	}

	// Tags
	if _, err = c.TagCertificateAuthority(ctx, &sdk.TagCertificateAuthorityInput{
		CertificateAuthorityArn: arn, Tags: []types.Tag{{Key: new("Project"), Value: new("Pki")}},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{CertificateAuthorityArn: arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("unexpected tags %+v, %v", tags, err)
	}

	if _, err = c.UntagCertificateAuthority(ctx, &sdk.UntagCertificateAuthorityInput{
		CertificateAuthorityArn: arn, Tags: []types.Tag{{Key: new("Environment")}},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err = c.ListTags(ctx, &sdk.ListTagsInput{CertificateAuthorityArn: arn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "Project" {
		t.Fatalf("unexpected tags %+v, %v", tags, err)
	}

	// Resource Groups Tagging API tags through the same operations.
	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(arn)}, Tags: map[string]string{"rgta": "works"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("TagResources failed: %+v, %v", tagged, err)
	}

	if tags, err = c.ListTags(
		ctx,
		&sdk.ListTagsInput{CertificateAuthorityArn: arn},
	); err != nil ||
		len(tags.Tags) != 2 {
		t.Fatalf("unexpected tags %+v, %v", tags, err)
	}

	// Delete keeps the authority for its restoration period; restore returns it.
	before := time.Now()
	if _, err = c.DeleteCertificateAuthority(ctx, &sdk.DeleteCertificateAuthorityInput{
		CertificateAuthorityArn: arn, PermanentDeletionTimeInDays: new(int32(7)),
	}); err != nil {
		t.Fatal(err)
	}

	desc, err = c.DescribeCertificateAuthority(
		ctx,
		&sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: arn},
	)
	if err != nil || desc.CertificateAuthority.Status != types.CertificateAuthorityStatusDeleted ||
		desc.CertificateAuthority.RestorableUntil.Before(before.Add(6*24*time.Hour)) {
		t.Fatalf("unexpected deleted authority %+v, %v", desc, err)
	}

	listed, err = c.ListCertificateAuthorities(ctx, &sdk.ListCertificateAuthoritiesInput{})
	if err != nil || len(listed.CertificateAuthorities) != 1 ||
		listed.CertificateAuthorities[0].Status != types.CertificateAuthorityStatusDeleted {
		t.Fatalf("a deleted authority is listed with its status: %+v, %v", listed, err)
	}

	if _, err = c.RestoreCertificateAuthority(
		ctx,
		&sdk.RestoreCertificateAuthorityInput{CertificateAuthorityArn: arn},
	); err != nil {
		t.Fatal(err)
	}

	desc, err = c.DescribeCertificateAuthority(
		ctx,
		&sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: arn},
	)
	if err != nil || desc.CertificateAuthority.Status != types.CertificateAuthorityStatusPendingCertificate ||
		desc.CertificateAuthority.RestorableUntil != nil {
		t.Fatalf("unexpected restored authority %+v, %v", desc, err)
	}
}

func TestACMPCAModeledErrors(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := acmpcaClient(s, "test")

	arn := "arn:aws:acm-pca:us-east-1:000000000000:certificate-authority/12345678-1234-1234-1234-123456789012"
	_, err := c.DescribeCertificateAuthority(ctx, &sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: &arn})
	var missing *types.ResourceNotFoundException
	if !errors.As(err, &missing) {
		t.Fatalf("want ResourceNotFoundException, got %v", err)
	}

	_, err = c.DescribeCertificateAuthority(
		ctx,
		&sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: new("nope")},
	)
	var badARN *types.InvalidArnException
	if !errors.As(err, &badARN) {
		t.Fatalf("want InvalidArnException, got %v", err)
	}

	in := privateCAInput()
	in.CertificateAuthorityConfiguration.SigningAlgorithm = types.SigningAlgorithmSha256withecdsa
	_, err = c.CreateCertificateAuthority(ctx, in)
	var badArgs *types.InvalidArgsException
	if !errors.As(err, &badArgs) {
		t.Fatalf("want InvalidArgsException, got %v", err)
	}

	// Fields outside the supported slice are rejected instead of ignored.
	in = privateCAInput()
	in.CertificateAuthorityConfiguration.CsrExtensions = &types.CsrExtensions{}
	if _, err = c.CreateCertificateAuthority(ctx, in); err == nil {
		t.Fatal("an unsupported CsrExtensions member was accepted")
	}

	// Another account does not see the authority.
	created, err := c.CreateCertificateAuthority(ctx, privateCAInput())
	if err != nil {
		t.Fatal(err)
	}

	other := acmpcaClient(s, "team-b")
	_, err = other.DescribeCertificateAuthority(ctx, &sdk.DescribeCertificateAuthorityInput{
		CertificateAuthorityArn: created.CertificateAuthorityArn,
	})
	if !errors.As(err, &missing) {
		t.Fatalf("want ResourceNotFoundException, got %v", err)
	}
}

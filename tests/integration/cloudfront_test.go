package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/smithy-go"
)

func cloudfrontClient(s *httptest.Server) *cloudfront.Client {
	return cloudfront.New(cloudfront.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func sampleConfig(ref, comment string, enabled bool) *cftypes.DistributionConfig {
	return &cftypes.DistributionConfig{
		CallerReference: aws.String(ref),
		Comment:         aws.String(comment),
		Enabled:         aws.Bool(enabled),
		Origins: &cftypes.Origins{
			Quantity: aws.Int32(1),
			Items: []cftypes.Origin{
				{
					Id:         aws.String("my-origin"),
					DomainName: aws.String("mybucket.s3.amazonaws.com"),
					S3OriginConfig: &cftypes.S3OriginConfig{
						OriginAccessIdentity: aws.String(""),
					},
				},
			},
		},
		DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
			TargetOriginId:       aws.String("my-origin"),
			ViewerProtocolPolicy: cftypes.ViewerProtocolPolicyAllowAll,
		},
	}
}

func TestCloudFrontDistributionLifecycle(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()

	// 1. Create distribution
	created, err := c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: sampleConfig("ref-1", "Test distribution 1", true),
	})
	if err != nil {
		t.Fatalf("CreateDistribution: %v", err)
	}

	dist := created.Distribution
	if dist == nil || dist.Id == nil || *dist.Id == "" {
		t.Fatalf("expected non-empty ID: %+v", dist)
	}
	id := *dist.Id
	if dist.DomainName == nil || *dist.DomainName == "" {
		t.Fatalf("expected domain name: %+v", dist)
	}
	if dist.Status == nil || *dist.Status != "Deployed" {
		t.Fatalf("expected Deployed status: %+v", dist)
	}
	if created.ETag == nil || *created.ETag == "" {
		t.Fatalf("expected non-empty ETag")
	}
	etag := *created.ETag

	// 2. Get distribution
	got, err := c.GetDistribution(ctx, &cloudfront.GetDistributionInput{
		Id: aws.String(id),
	})
	if err != nil {
		t.Fatalf("GetDistribution: %v", err)
	}
	if *got.Distribution.Id != id {
		t.Fatalf("got ID %s, expected %s", *got.Distribution.Id, id)
	}
	if got.ETag == nil || *got.ETag != etag {
		t.Fatalf("expected ETag %s, got %v", etag, got.ETag)
	}

	// 3. Get distribution config
	gotConfig, err := c.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(id),
	})
	if err != nil {
		t.Fatalf("GetDistributionConfig: %v", err)
	}
	if *gotConfig.DistributionConfig.Comment != "Test distribution 1" {
		t.Fatalf("unexpected comment: %v", gotConfig.DistributionConfig.Comment)
	}
	if gotConfig.ETag == nil || *gotConfig.ETag != etag {
		t.Fatalf("expected config ETag %s, got %v", etag, gotConfig.ETag)
	}

	// 4. Duplicate caller reference should fail
	_, err = c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: sampleConfig("ref-1", "Duplicate ref", true),
	})
	if err == nil {
		t.Fatal("expected DistributionAlreadyExists error")
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() != "DistributionAlreadyExists" {
		t.Fatalf("expected DistributionAlreadyExists, got %s", ae.ErrorCode())
	}

	// 5. Delete while enabled should fail
	_, err = c.DeleteDistribution(ctx, &cloudfront.DeleteDistributionInput{
		Id:      aws.String(id),
		IfMatch: aws.String(etag),
	})
	if err == nil {
		t.Fatal("expected DistributionNotDisabled error")
	}
	if errors.As(err, &ae) && ae.ErrorCode() != "DistributionNotDisabled" {
		t.Fatalf("expected DistributionNotDisabled, got %s", ae.ErrorCode())
	}

	// 6. Update with bad IfMatch should fail
	badCfg := sampleConfig("ref-1", "Updated comment", false)
	_, err = c.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
		Id:                 aws.String(id),
		IfMatch:            aws.String("wrong-etag"),
		DistributionConfig: badCfg,
	})
	if err == nil {
		t.Fatal("expected InvalidIfMatchVersion error")
	}
	if errors.As(err, &ae) && ae.ErrorCode() != "InvalidIfMatchVersion" {
		t.Fatalf("expected InvalidIfMatchVersion, got %s", ae.ErrorCode())
	}

	// 7. Update to disabled with correct ETag
	updateCfg := sampleConfig("ref-1", "Updated comment", false)
	updated, err := c.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
		Id:                 aws.String(id),
		IfMatch:            aws.String(etag),
		DistributionConfig: updateCfg,
	})
	if err != nil {
		t.Fatalf("UpdateDistribution: %v", err)
	}
	if *updated.Distribution.DistributionConfig.Comment != "Updated comment" {
		t.Fatalf("expected comment to be updated: %v", updated.Distribution.DistributionConfig.Comment)
	}
	if *updated.Distribution.DistributionConfig.Enabled != false {
		t.Fatalf("expected disabled distribution")
	}
	newETag := *updated.ETag

	// 8. Delete with new ETag
	_, err = c.DeleteDistribution(ctx, &cloudfront.DeleteDistributionInput{
		Id:      aws.String(id),
		IfMatch: aws.String(newETag),
	})
	if err != nil {
		t.Fatalf("DeleteDistribution: %v", err)
	}

	// 9. Get after delete should fail with NoSuchDistribution
	_, err = c.GetDistribution(ctx, &cloudfront.GetDistributionInput{
		Id: aws.String(id),
	})
	if err == nil {
		t.Fatal("expected NoSuchDistribution error after delete")
	}
	if errors.As(err, &ae) && ae.ErrorCode() != "NoSuchDistribution" {
		t.Fatalf("expected NoSuchDistribution, got %s", ae.ErrorCode())
	}
}

func TestCloudFrontDistributionWithTags(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()

	created, err := c.CreateDistributionWithTags(ctx, &cloudfront.CreateDistributionWithTagsInput{
		DistributionConfigWithTags: &cftypes.DistributionConfigWithTags{
			DistributionConfig: sampleConfig("ref-tags", "Tagged distribution", true),
			Tags: &cftypes.Tags{
				Items: []cftypes.Tag{
					{Key: aws.String("Env"), Value: aws.String("Production")},
					{Key: aws.String("Owner"), Value: aws.String("Infra")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateDistributionWithTags: %v", err)
	}
	if created.Distribution == nil || created.Distribution.Id == nil {
		t.Fatalf("expected distribution returned")
	}
}

func TestCloudFrontListDistributions(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()

	// Empty list
	list0, err := c.ListDistributions(ctx, &cloudfront.ListDistributionsInput{})
	if err != nil {
		t.Fatalf("ListDistributions empty: %v", err)
	}
	if list0.DistributionList == nil || *list0.DistributionList.Quantity != 0 {
		t.Fatalf("expected 0 quantity, got %v", list0.DistributionList.Quantity)
	}

	// Create 2 distributions
	_, err = c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: sampleConfig("list-ref-1", "Dist 1", true),
	})
	if err != nil {
		t.Fatalf("CreateDistribution 1: %v", err)
	}
	_, err = c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: sampleConfig("list-ref-2", "Dist 2", true),
	})
	if err != nil {
		t.Fatalf("CreateDistribution 2: %v", err)
	}

	// List both
	list2, err := c.ListDistributions(ctx, &cloudfront.ListDistributionsInput{})
	if err != nil {
		t.Fatalf("ListDistributions 2: %v", err)
	}
	if *list2.DistributionList.Quantity != 2 {
		t.Fatalf("expected 2 quantity, got %d", *list2.DistributionList.Quantity)
	}
	if len(list2.DistributionList.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list2.DistributionList.Items))
	}
}

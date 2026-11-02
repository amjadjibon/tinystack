package integration_test

import (
	"context"
	"errors"
	"maps"
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

func TestCloudFrontInvalidations(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()

	// Create distribution
	createdDist, err := c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: sampleConfig("inv-dist-ref", "Invalidation test dist", true),
	})
	if err != nil {
		t.Fatalf("CreateDistribution: %v", err)
	}
	distID := *createdDist.Distribution.Id

	// 1. Create Invalidation
	createdInv, err := c.CreateInvalidation(ctx, &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(distID),
		InvalidationBatch: &cftypes.InvalidationBatch{
			CallerReference: aws.String("inv-ref-1"),
			Paths: &cftypes.Paths{
				Quantity: aws.Int32(2),
				Items:    []string{"/*", "/index.html"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateInvalidation: %v", err)
	}
	inv := createdInv.Invalidation
	if inv == nil || inv.Id == nil || *inv.Id == "" {
		t.Fatalf("expected non-empty Invalidation ID")
	}
	if inv.Status == nil || *inv.Status != "Completed" {
		t.Fatalf("expected Completed status, got %v", inv.Status)
	}
	invID := *inv.Id

	// 2. Get Invalidation
	gotInv, err := c.GetInvalidation(ctx, &cloudfront.GetInvalidationInput{
		DistributionId: aws.String(distID),
		Id:             aws.String(invID),
	})
	if err != nil {
		t.Fatalf("GetInvalidation: %v", err)
	}
	if *gotInv.Invalidation.Id != invID {
		t.Fatalf("expected ID %s, got %s", invID, *gotInv.Invalidation.Id)
	}
	if *gotInv.Invalidation.Status != "Completed" {
		t.Fatalf("expected Completed status")
	}

	// 3. Idempotent CreateInvalidation with same CallerReference
	idempotentInv, err := c.CreateInvalidation(ctx, &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(distID),
		InvalidationBatch: &cftypes.InvalidationBatch{
			CallerReference: aws.String("inv-ref-1"),
			Paths: &cftypes.Paths{
				Quantity: aws.Int32(2),
				Items:    []string{"/*", "/index.html"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateInvalidation idempotent: %v", err)
	}
	if *idempotentInv.Invalidation.Id != invID {
		t.Fatalf("expected same ID %s, got %s", invID, *idempotentInv.Invalidation.Id)
	}

	// 4. Get non-existent invalidation
	_, err = c.GetInvalidation(ctx, &cloudfront.GetInvalidationInput{
		DistributionId: aws.String(distID),
		Id:             aws.String("non-existent-inv"),
	})
	if err == nil {
		t.Fatal("expected NoSuchInvalidation error")
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() != "NoSuchInvalidation" {
		t.Fatalf("expected NoSuchInvalidation, got %s", ae.ErrorCode())
	}

	// 5. List invalidations
	listInv, err := c.ListInvalidations(ctx, &cloudfront.ListInvalidationsInput{
		DistributionId: aws.String(distID),
	})
	if err != nil {
		t.Fatalf("ListInvalidations: %v", err)
	}
	if listInv.InvalidationList == nil || *listInv.InvalidationList.Quantity != 1 {
		t.Fatalf("expected 1 invalidation in list")
	}
	if *listInv.InvalidationList.Items[0].Id != invID {
		t.Fatalf("expected invalidation ID %s in list, got %s", invID, *listInv.InvalidationList.Items[0].Id)
	}
}

func TestCloudFrontOriginAccessControl(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()

	// 1. Create OAC
	oacConfig := &cftypes.OriginAccessControlConfig{
		Name:                          aws.String("my-s3-oac"),
		Description:                   aws.String("OAC for S3 origin"),
		OriginAccessControlOriginType: cftypes.OriginAccessControlOriginTypesS3,
		SigningBehavior:               cftypes.OriginAccessControlSigningBehaviorsAlways,
		SigningProtocol:               cftypes.OriginAccessControlSigningProtocolsSigv4,
	}
	created, err := c.CreateOriginAccessControl(ctx, &cloudfront.CreateOriginAccessControlInput{
		OriginAccessControlConfig: oacConfig,
	})
	if err != nil {
		t.Fatalf("CreateOriginAccessControl: %v", err)
	}
	if created.OriginAccessControl == nil || created.OriginAccessControl.Id == nil {
		t.Fatal("expected OriginAccessControl with ID")
	}
	if created.ETag == nil || *created.ETag == "" {
		t.Fatal("expected ETag")
	}
	oacID := *created.OriginAccessControl.Id
	etag := *created.ETag

	// 2. Duplicate Name fails
	_, err = c.CreateOriginAccessControl(ctx, &cloudfront.CreateOriginAccessControlInput{
		OriginAccessControlConfig: oacConfig,
	})
	if err == nil {
		t.Fatal("expected error on duplicate name")
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() != "OriginAccessControlAlreadyExists" {
		t.Fatalf("expected OriginAccessControlAlreadyExists, got %s", ae.ErrorCode())
	}

	// 3. Get OAC
	got, err := c.GetOriginAccessControl(ctx, &cloudfront.GetOriginAccessControlInput{
		Id: aws.String(oacID),
	})
	if err != nil {
		t.Fatalf("GetOriginAccessControl: %v", err)
	}
	if *got.OriginAccessControl.Id != oacID {
		t.Fatalf("expected ID %s, got %s", oacID, *got.OriginAccessControl.Id)
	}
	if *got.ETag != etag {
		t.Fatalf("expected ETag %s, got %s", etag, *got.ETag)
	}

	// 4. Get OAC Config
	gotCfg, err := c.GetOriginAccessControlConfig(ctx, &cloudfront.GetOriginAccessControlConfigInput{
		Id: aws.String(oacID),
	})
	if err != nil {
		t.Fatalf("GetOriginAccessControlConfig: %v", err)
	}
	if *gotCfg.OriginAccessControlConfig.Name != "my-s3-oac" {
		t.Fatalf("expected name my-s3-oac, got %s", *gotCfg.OriginAccessControlConfig.Name)
	}

	// 5. Update OAC with bad ETag fails
	badUpdateCfg := *oacConfig
	badUpdateCfg.Description = aws.String("Updated desc")
	_, err = c.UpdateOriginAccessControl(ctx, &cloudfront.UpdateOriginAccessControlInput{
		Id:                        aws.String(oacID),
		IfMatch:                   aws.String("wrong-etag"),
		OriginAccessControlConfig: &badUpdateCfg,
	})
	if err == nil {
		t.Fatal("expected error on mismatched IfMatch")
	}

	// 6. Update OAC with correct ETag succeeds
	updated, err := c.UpdateOriginAccessControl(ctx, &cloudfront.UpdateOriginAccessControlInput{
		Id:                        aws.String(oacID),
		IfMatch:                   aws.String(etag),
		OriginAccessControlConfig: &badUpdateCfg,
	})
	if err != nil {
		t.Fatalf("UpdateOriginAccessControl: %v", err)
	}
	if *updated.OriginAccessControl.OriginAccessControlConfig.Description != "Updated desc" {
		t.Fatalf("expected updated description")
	}
	etag = *updated.ETag

	// 7. List OACs
	listRes, err := c.ListOriginAccessControls(ctx, &cloudfront.ListOriginAccessControlsInput{})
	if err != nil {
		t.Fatalf("ListOriginAccessControls: %v", err)
	}
	if listRes.OriginAccessControlList == nil || *listRes.OriginAccessControlList.Quantity != 1 {
		t.Fatalf("expected 1 OAC in list, got %v", listRes.OriginAccessControlList)
	}
	if *listRes.OriginAccessControlList.Items[0].Id != oacID {
		t.Fatalf("expected OAC ID %s in list", oacID)
	}

	// 8. Test In-Use Deletion protection
	distCfg := sampleConfig("oac-dist-ref", "OAC test dist", false)
	distCfg.Origins.Items[0].OriginAccessControlId = aws.String(oacID)
	createdDist, err := c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: distCfg,
	})
	if err != nil {
		t.Fatalf("CreateDistribution with OAC: %v", err)
	}
	distID := *createdDist.Distribution.Id
	distETag := *createdDist.ETag

	// Attempt to delete OAC should fail because it is in use
	_, err = c.DeleteOriginAccessControl(ctx, &cloudfront.DeleteOriginAccessControlInput{
		Id:      aws.String(oacID),
		IfMatch: aws.String(etag),
	})
	if err == nil {
		t.Fatal("expected OriginAccessControlInUse error")
	}
	if errors.As(err, &ae) && ae.ErrorCode() != "OriginAccessControlInUse" {
		t.Fatalf("expected OriginAccessControlInUse, got %s", ae.ErrorCode())
	}

	// Delete distribution
	_, err = c.DeleteDistribution(ctx, &cloudfront.DeleteDistributionInput{
		Id:      aws.String(distID),
		IfMatch: aws.String(distETag),
	})
	if err != nil {
		t.Fatalf("DeleteDistribution: %v", err)
	}

	// Now delete OAC succeeds
	_, err = c.DeleteOriginAccessControl(ctx, &cloudfront.DeleteOriginAccessControlInput{
		Id:      aws.String(oacID),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		t.Fatalf("DeleteOriginAccessControl: %v", err)
	}

	// Get after delete returns NoSuchOriginAccessControl
	_, err = c.GetOriginAccessControl(ctx, &cloudfront.GetOriginAccessControlInput{
		Id: aws.String(oacID),
	})
	if err == nil {
		t.Fatal("expected NoSuchOriginAccessControl error")
	}
	if errors.As(err, &ae) && ae.ErrorCode() != "NoSuchOriginAccessControl" {
		t.Fatalf("expected NoSuchOriginAccessControl, got %s", ae.ErrorCode())
	}
}

// Invalidation records are stored apart from distributions. ListDistributions and the resource inventory read every
// record of the distribution kind, and failed once an invalidation existed.
func TestCloudFrontListingsSurviveInvalidations(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()

	created, err := c.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: sampleConfig("listing-ref", "Listing test dist", true),
	})
	if err != nil {
		t.Fatalf("CreateDistribution: %v", err)
	}

	if _, err = c.CreateInvalidation(ctx, &cloudfront.CreateInvalidationInput{
		DistributionId: created.Distribution.Id,
		InvalidationBatch: &cftypes.InvalidationBatch{
			CallerReference: new("listing-invalidation"),
			Paths:           &cftypes.Paths{Quantity: new(int32(1)), Items: []string{"/*"}},
		},
	}); err != nil {
		t.Fatalf("CreateInvalidation: %v", err)
	}

	listed, err := c.ListDistributions(ctx, &cloudfront.ListDistributionsInput{})
	if err != nil {
		t.Fatalf("ListDistributions: %v", err)
	}

	if items := listed.DistributionList.Items; len(items) != 1 || *items[0].Id != *created.Distribution.Id {
		t.Fatalf("distributions = %+v", items)
	}

	distributions := 0
	for _, r := range genericInventory(t, s) {
		if r.Service == "cloudfront" && r.Type == "distribution" {
			distributions++
		}
	}

	if distributions != 1 {
		t.Fatalf("the inventory lists %d distributions, want 1", distributions)
	}
}

func TestCloudFrontDistributionTags(t *testing.T) {
	s := server(t)
	c := cloudfrontClient(s)
	ctx := context.Background()
	tag := func(k, v string) cftypes.Tag { return cftypes.Tag{Key: new(k), Value: new(v)} }

	created, err := c.CreateDistributionWithTags(ctx, &cloudfront.CreateDistributionWithTagsInput{
		DistributionConfigWithTags: &cftypes.DistributionConfigWithTags{
			DistributionConfig: sampleConfig("tags-ref", "Tagged distribution", false),
			Tags:               &cftypes.Tags{Items: []cftypes.Tag{tag("env", "dev")}},
		},
	})
	if err != nil {
		t.Fatalf("CreateDistributionWithTags: %v", err)
	}

	arn := created.Distribution.ARN
	tags := func() map[string]string {
		t.Helper()
		out, listErr := c.ListTagsForResource(ctx, &cloudfront.ListTagsForResourceInput{Resource: arn})
		if listErr != nil {
			t.Fatalf("ListTagsForResource: %v", listErr)
		}

		got := map[string]string{}
		for _, tg := range out.Tags.Items {
			got[*tg.Key] = *tg.Value
		}

		return got
	}

	if got := tags(); !maps.Equal(got, map[string]string{"env": "dev"}) {
		t.Fatalf("tags after creation = %v", got)
	}

	if _, err = c.TagResource(ctx, &cloudfront.TagResourceInput{
		Resource: arn, Tags: &cftypes.Tags{Items: []cftypes.Tag{tag("env", "prod"), tag("team", "web")}},
	}); err != nil {
		t.Fatalf("TagResource: %v", err)
	}

	if got := tags(); !maps.Equal(got, map[string]string{"env": "prod", "team": "web"}) {
		t.Fatalf("tags after TagResource = %v", got)
	}

	if _, err = c.UntagResource(ctx, &cloudfront.UntagResourceInput{
		Resource: arn, TagKeys: &cftypes.TagKeys{Items: []string{"env"}},
	}); err != nil {
		t.Fatalf("UntagResource: %v", err)
	}

	if got := tags(); !maps.Equal(got, map[string]string{"team": "web"}) {
		t.Fatalf("tags after UntagResource = %v", got)
	}

	// Tagging is not a configuration change, so the ETag stays valid.
	got, err := c.GetDistribution(ctx, &cloudfront.GetDistributionInput{Id: created.Distribution.Id})
	if err != nil {
		t.Fatalf("GetDistribution: %v", err)
	}

	if *got.ETag != *created.ETag {
		t.Fatalf("ETag changed from %s to %s", *created.ETag, *got.ETag)
	}

	inventoried := false
	for _, r := range genericInventory(t, s) {
		if r.ARN == *arn {
			inventoried = maps.Equal(r.Tags, map[string]string{"team": "web"})
		}
	}

	if !inventoried {
		t.Fatal("the inventory does not list the distribution with its tags")
	}

	_, err = c.ListTagsForResource(ctx, &cloudfront.ListTagsForResourceInput{Resource: new(*arn + "-missing")})
	wantCode(t, err, "NoSuchResource")

	_, err = c.CreateDistributionWithTags(ctx, &cloudfront.CreateDistributionWithTagsInput{
		DistributionConfigWithTags: &cftypes.DistributionConfigWithTags{
			DistributionConfig: sampleConfig("reserved-tag-ref", "Reserved tag", false),
			Tags:               &cftypes.Tags{Items: []cftypes.Tag{tag("aws:team", "web")}},
		},
	})
	wantCode(t, err, "InvalidArgument")
}

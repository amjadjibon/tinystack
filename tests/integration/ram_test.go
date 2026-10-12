package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/ram"
	"github.com/aws/aws-sdk-go-v2/service/ram/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func ramClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func TestRAMSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ramClient(s, "test", "us-east-1")
	taggingC := taggingClient(s, "us-east-1")

	// 1. CreateResourceShare
	resArn1 := "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-00000001"
	resArn2 := "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-00000002"
	principal1 := "123456789012"
	principal2 := "210987654321"

	created, err := c.CreateResourceShare(ctx, &sdk.CreateResourceShareInput{
		Name:                    aws.String("my-share"),
		AllowExternalPrincipals: aws.Bool(true),
		Principals:              []string{principal1},
		ResourceArns:            []string{resArn1},
		Tags: []types.Tag{
			{Key: aws.String("Env"), Value: aws.String("Test")},
		},
	})
	if err != nil {
		t.Fatalf("CreateResourceShare failed: %v", err)
	}

	share := created.ResourceShare
	if share == nil {
		t.Fatal("expected non-nil ResourceShare")
	}
	shareArn := aws.ToString(share.ResourceShareArn)
	if !strings.HasPrefix(shareArn, "arn:aws:ram:us-east-1:") {
		t.Fatalf("unexpected share ARN: %s", shareArn)
	}
	if aws.ToString(share.Name) != "my-share" {
		t.Fatalf("expected name my-share, got %s", aws.ToString(share.Name))
	}
	if share.Status != types.ResourceShareStatusActive {
		t.Fatalf("expected ACTIVE status, got %v", share.Status)
	}

	// 2. GetResourceShares
	sharesOut, err := c.GetResourceShares(ctx, &sdk.GetResourceSharesInput{
		ResourceOwner:     types.ResourceOwnerSelf,
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShares failed: %v", err)
	}
	if len(sharesOut.ResourceShares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(sharesOut.ResourceShares))
	}
	if len(sharesOut.ResourceShares[0].Tags) != 1 ||
		aws.ToString(sharesOut.ResourceShares[0].Tags[0].Key) != "Env" {
		t.Fatalf("unexpected tags: %v", sharesOut.ResourceShares[0].Tags)
	}

	// 3. UpdateResourceShare
	updated, err := c.UpdateResourceShare(ctx, &sdk.UpdateResourceShareInput{
		ResourceShareArn: aws.String(shareArn),
		Name:             aws.String("my-renamed-share"),
	})
	if err != nil {
		t.Fatalf("UpdateResourceShare failed: %v", err)
	}
	if aws.ToString(updated.ResourceShare.Name) != "my-renamed-share" {
		t.Fatalf("expected my-renamed-share, got %s", aws.ToString(updated.ResourceShare.Name))
	}

	// 4. AssociateResourceShare
	assocOut, err := c.AssociateResourceShare(ctx, &sdk.AssociateResourceShareInput{
		ResourceShareArn: aws.String(shareArn),
		Principals:       []string{principal2},
		ResourceArns:     []string{resArn2},
	})
	if err != nil {
		t.Fatalf("AssociateResourceShare failed: %v", err)
	}
	if len(assocOut.ResourceShareAssociations) != 2 {
		t.Fatalf("expected 2 associations, got %d", len(assocOut.ResourceShareAssociations))
	}

	// 5. GetResourceShareAssociations
	resAssocs, err := c.GetResourceShareAssociations(ctx, &sdk.GetResourceShareAssociationsInput{
		AssociationType:   types.ResourceShareAssociationTypeResource,
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShareAssociations resource failed: %v", err)
	}
	if len(resAssocs.ResourceShareAssociations) != 2 {
		t.Fatalf("expected 2 resource associations, got %d", len(resAssocs.ResourceShareAssociations))
	}

	princAssocs, err := c.GetResourceShareAssociations(ctx, &sdk.GetResourceShareAssociationsInput{
		AssociationType:   types.ResourceShareAssociationTypePrincipal,
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShareAssociations principal failed: %v", err)
	}
	if len(princAssocs.ResourceShareAssociations) != 2 {
		t.Fatalf("expected 2 principal associations, got %d", len(princAssocs.ResourceShareAssociations))
	}

	// 6. DisassociateResourceShare
	disassocOut, err := c.DisassociateResourceShare(ctx, &sdk.DisassociateResourceShareInput{
		ResourceShareArn: aws.String(shareArn),
		Principals:       []string{principal2},
		ResourceArns:     []string{resArn2},
	})
	if err != nil {
		t.Fatalf("DisassociateResourceShare failed: %v", err)
	}
	if len(disassocOut.ResourceShareAssociations) != 2 {
		t.Fatalf("expected 2 disassociations, got %d", len(disassocOut.ResourceShareAssociations))
	}
	for _, a := range disassocOut.ResourceShareAssociations {
		if a.Status != types.ResourceShareAssociationStatusDisassociated {
			t.Fatalf("expected DISASSOCIATED status, got %v", a.Status)
		}
	}

	// 7. TagResource
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceShareArn: aws.String(shareArn),
		Tags: []types.Tag{
			{Key: aws.String("Project"), Value: aws.String("TinyStack")},
		},
	})
	if err != nil {
		t.Fatalf("TagResource failed: %v", err)
	}

	// Verify via Resource Groups Tagging API
	rgtaOut, err := taggingC.GetResources(ctx, &rgta.GetResourcesInput{
		ResourceTypeFilters: []string{"ram:resource-share"},
	})
	if err != nil {
		t.Fatalf("RGTA GetResources failed: %v", err)
	}
	if len(rgtaOut.ResourceTagMappingList) != 1 {
		t.Fatalf("expected 1 tagged resource in RGTA, got %d", len(rgtaOut.ResourceTagMappingList))
	}
	if len(rgtaOut.ResourceTagMappingList[0].Tags) != 2 {
		t.Fatalf("expected 2 tags in RGTA, got %d", len(rgtaOut.ResourceTagMappingList[0].Tags))
	}

	// 8. UntagResource
	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceShareArn: aws.String(shareArn),
		TagKeys:          []string{"Env"},
	})
	if err != nil {
		t.Fatalf("UntagResource failed: %v", err)
	}

	// Verify untag via GetResourceShares
	sharesAfterUntag, err := c.GetResourceShares(ctx, &sdk.GetResourceSharesInput{
		ResourceOwner:     types.ResourceOwnerSelf,
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShares after untag failed: %v", err)
	}
	if len(sharesAfterUntag.ResourceShares[0].Tags) != 1 ||
		aws.ToString(sharesAfterUntag.ResourceShares[0].Tags[0].Key) != "Project" {
		t.Fatalf("unexpected tags after untag: %v", sharesAfterUntag.ResourceShares[0].Tags)
	}

	// 9. DeleteResourceShare
	delOut, err := c.DeleteResourceShare(ctx, &sdk.DeleteResourceShareInput{
		ResourceShareArn: aws.String(shareArn),
	})
	if err != nil {
		t.Fatalf("DeleteResourceShare failed: %v", err)
	}
	if !aws.ToBool(delOut.ReturnValue) {
		t.Fatal("expected ReturnValue true on DeleteResourceShare")
	}

	// Verify status is DELETED
	deletedShares, err := c.GetResourceShares(ctx, &sdk.GetResourceSharesInput{
		ResourceOwner:       types.ResourceOwnerSelf,
		ResourceShareStatus: types.ResourceShareStatusDeleted,
		ResourceShareArns:   []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShares DELETED failed: %v", err)
	}
	if len(deletedShares.ResourceShares) != 1 ||
		deletedShares.ResourceShares[0].Status != types.ResourceShareStatusDeleted {
		t.Fatalf("expected deleted share, got: %v", deletedShares.ResourceShares)
	}
}

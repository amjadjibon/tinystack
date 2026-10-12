package ram_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ram"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ram"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
}

func testService() *ram.Service {
	return ram.New(memory.New(nil, nil))
}

func TestResourceShareLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// 1. Create Resource Share
	created, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name: new("test-share"),
		Tags: []api.Tag{
			{Key: new("env"), Value: new("dev")},
		},
		Principals: []string{
			"123456789012",
			"210987654321",
		},
		ResourceArns: []string{
			"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-12345678",
		},
	})
	if err != nil {
		t.Fatalf("CreateResourceShare failed: %v", err)
	}

	share := created.ResourceShare
	if share == nil {
		t.Fatal("expected non-nil ResourceShare")
	}

	if common.Value(share.Name) != "test-share" {
		t.Fatalf("expected test-share, got %v", common.Value(share.Name))
	}

	if common.Value(share.Status) != "ACTIVE" {
		t.Fatalf("expected ACTIVE, got %v", common.Value(share.Status))
	}

	if common.Value(share.AllowExternalPrincipals) != true {
		t.Fatalf(
			"expected AllowExternalPrincipals true by default, got %v",
			common.Value(share.AllowExternalPrincipals),
		)
	}

	if len(share.Tags) != 1 || common.Value(share.Tags[0].Key) != "env" {
		t.Fatalf("unexpected tags: %v", share.Tags)
	}

	shareArn := common.Value(share.ResourceShareArn)

	// 2. Get Resource Shares (SELF)
	getRes, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner: new("SELF"),
	})
	if err != nil {
		t.Fatalf("GetResourceShares failed: %v", err)
	}

	if len(getRes.ResourceShares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(getRes.ResourceShares))
	}

	// 3. Get Resource Shares (OTHER-ACCOUNTS returns empty)
	getOther, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner: new("OTHER-ACCOUNTS"),
	})
	if err != nil {
		t.Fatalf("GetResourceShares OTHER-ACCOUNTS failed: %v", err)
	}

	if len(getOther.ResourceShares) != 0 {
		t.Fatalf("expected 0 shares for OTHER-ACCOUNTS, got %d", len(getOther.ResourceShares))
	}

	// 4. Update Resource Share
	updated, err := svc.UpdateResourceShare(ctx, &api.UpdateResourceShareRequest{
		ResourceShareArn: new(shareArn),
		Name:             new("updated-share"),
	})
	if err != nil {
		t.Fatalf("UpdateResourceShare failed: %v", err)
	}

	if common.Value(updated.ResourceShare.Name) != "updated-share" {
		t.Fatalf("expected updated-share, got %v", common.Value(updated.ResourceShare.Name))
	}

	// 5. Delete Resource Share
	delRes, err := svc.DeleteResourceShare(ctx, &api.DeleteResourceShareRequest{
		ResourceShareArn: new(shareArn),
	})
	if err != nil {
		t.Fatalf("DeleteResourceShare failed: %v", err)
	}

	if !common.Value(delRes.ReturnValue) {
		t.Fatal("expected ReturnValue true")
	}

	// Verify not returned in default ACTIVE list
	getActive, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner: new("SELF"),
	})
	if err != nil {
		t.Fatalf("GetResourceShares failed: %v", err)
	}

	if len(getActive.ResourceShares) != 0 {
		t.Fatalf("expected 0 active shares after delete, got %d", len(getActive.ResourceShares))
	}

	// Verify returned when querying DELETED status
	getDeleted, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner:       new("SELF"),
		ResourceShareStatus: new("DELETED"),
	})
	if err != nil {
		t.Fatalf("GetResourceShares DELETED failed: %v", err)
	}

	if len(getDeleted.ResourceShares) != 1 {
		t.Fatalf("expected 1 deleted share, got %d", len(getDeleted.ResourceShares))
	}
}

func TestExternalPrincipalsValidation(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Creating share with AllowExternalPrincipals=false and external principal fails
	_, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name:                    new("internal-only"),
		AllowExternalPrincipals: new(false),
		Principals:              []string{"999999999999"}, // external
	})
	if err == nil {
		t.Fatal("expected error creating share with external principal when AllowExternalPrincipals=false")
	}

	// Creating share with AllowExternalPrincipals=true works
	created, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name:                    new("allowed-external"),
		AllowExternalPrincipals: new(true),
		Principals:              []string{"999999999999"},
	})
	if err != nil {
		t.Fatalf("CreateResourceShare failed: %v", err)
	}

	shareArn := common.Value(created.ResourceShare.ResourceShareArn)

	// Updating AllowExternalPrincipals to false while external principal is associated fails
	_, err = svc.UpdateResourceShare(ctx, &api.UpdateResourceShareRequest{
		ResourceShareArn:        new(shareArn),
		AllowExternalPrincipals: new(false),
	})
	if err == nil {
		t.Fatal("expected error updating AllowExternalPrincipals to false with active external principals")
	}

	// Disassociating external principal
	_, err = svc.DisassociateResourceShare(ctx, &api.DisassociateResourceShareRequest{
		ResourceShareArn: new(shareArn),
		Principals:       []string{"999999999999"},
	})
	if err != nil {
		t.Fatalf("DisassociateResourceShare failed: %v", err)
	}

	// Updating AllowExternalPrincipals to false now succeeds
	_, err = svc.UpdateResourceShare(ctx, &api.UpdateResourceShareRequest{
		ResourceShareArn:        new(shareArn),
		AllowExternalPrincipals: new(false),
	})
	if err != nil {
		t.Fatalf("UpdateResourceShare should succeed after disassociating external principal: %v", err)
	}

	// Associating external principal now fails
	_, err = svc.AssociateResourceShare(ctx, &api.AssociateResourceShareRequest{
		ResourceShareArn: new(shareArn),
		Principals:       []string{"999999999999"},
	})
	if err == nil {
		t.Fatal("expected error associating external principal when AllowExternalPrincipals=false")
	}
}

func TestAssociationsLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	created, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name: new("assoc-test"),
	})
	if err != nil {
		t.Fatalf("CreateResourceShare failed: %v", err)
	}

	shareArn := common.Value(created.ResourceShare.ResourceShareArn)

	// Associate resources and principals
	resArn := "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-aaaa1111"
	principal := "123456789012"

	assocRes, err := svc.AssociateResourceShare(ctx, &api.AssociateResourceShareRequest{
		ResourceShareArn: new(shareArn),
		ResourceArns:     []string{resArn},
		Principals:       []string{principal},
	})
	if err != nil {
		t.Fatalf("AssociateResourceShare failed: %v", err)
	}

	if len(assocRes.ResourceShareAssociations) != 2 {
		t.Fatalf("expected 2 associations, got %d", len(assocRes.ResourceShareAssociations))
	}

	// GetResourceShareAssociations by RESOURCE
	rAssocs, err := svc.GetResourceShareAssociations(ctx, &api.GetResourceShareAssociationsRequest{
		AssociationType:   new("RESOURCE"),
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShareAssociations failed: %v", err)
	}

	if len(rAssocs.ResourceShareAssociations) != 1 {
		t.Fatalf("expected 1 resource association, got %d", len(rAssocs.ResourceShareAssociations))
	}

	if common.Value(rAssocs.ResourceShareAssociations[0].AssociatedEntity) != resArn {
		t.Fatalf("expected %s, got %s", resArn, common.Value(rAssocs.ResourceShareAssociations[0].AssociatedEntity))
	}

	if common.Value(rAssocs.ResourceShareAssociations[0].Status) != "ASSOCIATED" {
		t.Fatalf("expected ASSOCIATED, got %s", common.Value(rAssocs.ResourceShareAssociations[0].Status))
	}

	// GetResourceShareAssociations by PRINCIPAL
	pAssocs, err := svc.GetResourceShareAssociations(ctx, &api.GetResourceShareAssociationsRequest{
		AssociationType:   new("PRINCIPAL"),
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShareAssociations failed: %v", err)
	}

	if len(pAssocs.ResourceShareAssociations) != 1 {
		t.Fatalf("expected 1 principal association, got %d", len(pAssocs.ResourceShareAssociations))
	}

	// Disassociate
	disassocRes, err := svc.DisassociateResourceShare(ctx, &api.DisassociateResourceShareRequest{
		ResourceShareArn: new(shareArn),
		ResourceArns:     []string{resArn},
	})
	if err != nil {
		t.Fatalf("DisassociateResourceShare failed: %v", err)
	}

	if len(disassocRes.ResourceShareAssociations) != 1 {
		t.Fatalf("expected 1 disassociated entity, got %d", len(disassocRes.ResourceShareAssociations))
	}

	if common.Value(disassocRes.ResourceShareAssociations[0].Status) != "DISASSOCIATED" {
		t.Fatalf("expected DISASSOCIATED, got %s", common.Value(disassocRes.ResourceShareAssociations[0].Status))
	}
}

func TestTagsLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	created, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name: new("tag-test"),
		Tags: []api.Tag{
			{Key: new("k1"), Value: new("v1")},
		},
	})
	if err != nil {
		t.Fatalf("CreateResourceShare failed: %v", err)
	}

	shareArn := common.Value(created.ResourceShare.ResourceShareArn)

	// TagResource
	_, err = svc.TagResource(ctx, &api.TagResourceRequest{
		ResourceShareArn: new(shareArn),
		Tags: []api.Tag{
			{Key: new("k2"), Value: new("v2")},
			{Key: new("k1"), Value: new("v1-updated")},
		},
	})
	if err != nil {
		t.Fatalf("TagResource failed: %v", err)
	}

	// Verify tags via GetResourceShares
	res, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner:     new("SELF"),
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShares failed: %v", err)
	}

	tags := res.ResourceShares[0].Tags
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tags))
	}

	// UntagResource
	_, err = svc.UntagResource(ctx, &api.UntagResourceRequest{
		ResourceShareArn: new(shareArn),
		TagKeys:          []string{"k1"},
	})
	if err != nil {
		t.Fatalf("UntagResource failed: %v", err)
	}

	resAfter, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner:     new("SELF"),
		ResourceShareArns: []string{shareArn},
	})
	if err != nil {
		t.Fatalf("GetResourceShares failed: %v", err)
	}

	tagsAfter := resAfter.ResourceShares[0].Tags
	if len(tagsAfter) != 1 || common.Value(tagsAfter[0].Key) != "k2" {
		t.Fatalf("expected 1 tag k2, got %v", tagsAfter)
	}
}

func TestPagination(t *testing.T) {
	ctx := testContext()
	svc := testService()

	for i := range 5 {
		_, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
			Name: new(fmt.Sprintf("page-share-%d", i)),
		})
		if err != nil {
			t.Fatalf("CreateResourceShare failed: %v", err)
		}
	}

	page1, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner: new("SELF"),
		MaxResults:    new(int64(2)),
	})
	if err != nil {
		t.Fatalf("GetResourceShares page 1 failed: %v", err)
	}

	if len(page1.ResourceShares) != 2 {
		t.Fatalf("expected 2 shares in page 1, got %d", len(page1.ResourceShares))
	}

	if page1.NextToken == nil || *page1.NextToken == "" {
		t.Fatal("expected non-empty nextToken in page 1")
	}

	page2, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner: new("SELF"),
		MaxResults:    new(int64(2)),
		NextToken:     page1.NextToken,
	})
	if err != nil {
		t.Fatalf("GetResourceShares page 2 failed: %v", err)
	}

	if len(page2.ResourceShares) != 2 {
		t.Fatalf("expected 2 shares in page 2, got %d", len(page2.ResourceShares))
	}
}

func TestValidationErrors(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Missing name
	_, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{})
	if err == nil || !strings.Contains(err.Error(), "name must be specified") {
		t.Fatalf("expected name validation error, got %v", err)
	}

	// Malformed ARN
	_, err = svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner:     new("SELF"),
		ResourceShareArns: []string{"bad-arn"},
	})
	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != "MalformedArnException" {
		t.Fatalf("expected MalformedArnException, got %v", err)
	}

	// Unknown resource on update
	_, err = svc.UpdateResourceShare(ctx, &api.UpdateResourceShareRequest{
		ResourceShareArn: new("arn:aws:ram:us-east-1:123456789012:resource-share/00000000-0000-0000-0000-000000000000"),
		Name:             new("new-name"),
	})
	if !errors.As(err, &awsErr) || awsErr.Code != "UnknownResourceException" {
		t.Fatalf("expected UnknownResourceException, got %v", err)
	}
}

func TestInventory(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name: new("inv-share"),
		Tags: []api.Tag{{Key: new("inv-k"), Value: new("inv-v")}},
	})
	if err != nil {
		t.Fatalf("CreateResourceShare failed: %v", err)
	}

	inv, err := svc.Resources(ctx)
	if err != nil {
		t.Fatalf("Resources failed: %v", err)
	}

	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory resource, got %d", len(inv))
	}

	r := inv[0]
	if r.Name != "inv-share" {
		t.Fatalf("expected name inv-share, got %s", r.Name)
	}

	if r.Type != "resource-share" {
		t.Fatalf("expected type resource-share, got %s", r.Type)
	}

	details, ok := r.Details.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any details, got %T", r.Details)
	}

	if details["RecordsOnly"] != true || details["ExecutionReady"] != false {
		t.Fatalf("expected RecordsOnly=true and ExecutionReady=false, got %v", details)
	}

	if r.Tags["inv-k"] != "inv-v" {
		t.Fatalf("expected tag inv-k=inv-v, got %v", r.Tags)
	}
}

func TestCreateResourceShareClientTokenIsIdempotent(t *testing.T) {
	ctx := testContext()
	svc := testService()

	first, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name:        new("shared"),
		ClientToken: new("token-1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	retry, err := svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name:        new("shared"),
		ClientToken: new("token-1"),
	})
	if err != nil || *retry.ResourceShare.ResourceShareArn != *first.ResourceShare.ResourceShareArn {
		t.Fatal("a retried token must return the original share", retry, err)
	}

	if _, err = svc.CreateResourceShare(ctx, &api.CreateResourceShareRequest{
		Name:        new("other"),
		ClientToken: new("token-1"),
	}); err == nil || !strings.Contains(err.Error(), "IdempotentParameterMismatch") {
		t.Fatal("reusing a token with different parameters must fail", err)
	}

	list, err := svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{ResourceOwner: new("SELF")})
	if err != nil || len(list.ResourceShares) != 1 {
		t.Fatal(list, err)
	}

	if _, err = svc.GetResourceShares(ctx, &api.GetResourceSharesRequest{
		ResourceOwner: new("SELF"),
		MaxResults:    new(int64(501)),
	}); err == nil {
		t.Fatal("maxResults above the model maximum must fail")
	}

	if _, err = svc.UpdateResourceShare(ctx, &api.UpdateResourceShareRequest{
		ResourceShareArn: first.ResourceShare.ResourceShareArn,
		Name:             new(""),
	}); err == nil {
		t.Fatal("an empty name must be rejected")
	}
}

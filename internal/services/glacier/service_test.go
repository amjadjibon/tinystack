package glacier_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/glacier"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
}

func testService() *glacier.Service {
	return glacier.New(memory.New(nil, nil))
}

func checkErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %s, got nil", wantCode)
	}

	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) {
		t.Fatalf("expected awserror.Error, got %T: %v", err, err)
	}

	if awsErr.Code != wantCode {
		t.Fatalf("expected error code %s, got %s (%s)", wantCode, awsErr.Code, awsErr.Message)
	}
}

func TestVaultLifecycle(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Create with "-" account ID
	created, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new("test-vault"),
	})
	if err != nil {
		t.Fatalf("CreateVault failed: %v", err)
	}

	if *created.Location != "/123456789012/vaults/test-vault" {
		t.Fatalf("unexpected Location: %s", *created.Location)
	}

	// Idempotent create
	created2, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("123456789012"),
		VaultName: new("test-vault"),
	})
	if err != nil {
		t.Fatalf("CreateVault duplicate failed: %v", err)
	}

	if *created2.Location != "/123456789012/vaults/test-vault" {
		t.Fatalf("unexpected Location on idempotent create: %s", *created2.Location)
	}

	// Describe vault
	desc, err := svc.DescribeVault(ctx, &api.DescribeVaultInput{
		AccountId: new("-"),
		VaultName: new("test-vault"),
	})
	if err != nil {
		t.Fatalf("DescribeVault failed: %v", err)
	}

	if *desc.VaultName != "test-vault" {
		t.Fatalf("expected test-vault, got %s", *desc.VaultName)
	}

	expectedARN := "arn:aws:glacier:us-east-1:123456789012:vaults/test-vault"
	if *desc.VaultARN != expectedARN {
		t.Fatalf("expected ARN %s, got %s", expectedARN, *desc.VaultARN)
	}

	if desc.CreationDate == nil || *desc.CreationDate == "" {
		t.Fatal("expected non-empty CreationDate")
	}

	if *desc.NumberOfArchives != 0 {
		t.Fatalf("expected 0 archives, got %d", *desc.NumberOfArchives)
	}

	if *desc.SizeInBytes != 0 {
		t.Fatalf("expected 0 bytes, got %d", *desc.SizeInBytes)
	}

	if desc.LastInventoryDate != nil {
		t.Fatal("expected nil LastInventoryDate")
	}

	// Delete vault
	_, err = svc.DeleteVault(ctx, &api.DeleteVaultInput{
		AccountId: new("-"),
		VaultName: new("test-vault"),
	})
	if err != nil {
		t.Fatalf("DeleteVault failed: %v", err)
	}

	// Describe after delete returns 404
	_, err = svc.DescribeVault(ctx, &api.DescribeVaultInput{
		AccountId: new("-"),
		VaultName: new("test-vault"),
	})
	checkErrorCode(t, err, "ResourceNotFoundException")

	// Delete non-existent vault returns 404
	_, err = svc.DeleteVault(ctx, &api.DeleteVaultInput{
		AccountId: new("-"),
		VaultName: new("test-vault"),
	})
	checkErrorCode(t, err, "ResourceNotFoundException")
}

func TestValidationRules(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Invalid AccountId
	_, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("999999999999"),
		VaultName: new("valid-vault"),
	})
	checkErrorCode(t, err, "InvalidParameterValueException")

	// Missing AccountId
	_, err = svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new(""),
		VaultName: new("valid-vault"),
	})
	checkErrorCode(t, err, "MissingParameterValueException")

	// Invalid VaultName characters
	badNames := []string{
		"invalid name with spaces",
		"bad/slash",
		"bad$dollar",
		"",
	}
	for _, bad := range badNames {
		_, err = svc.CreateVault(ctx, &api.CreateVaultInput{
			AccountId: new("-"),
			VaultName: new(bad),
		})
		if bad == "" {
			checkErrorCode(t, err, "MissingParameterValueException")
		} else {
			checkErrorCode(t, err, "InvalidParameterValueException")
		}
	}

	// VaultName too long (> 255 chars)
	tooLong := strings.Repeat("a", 256)
	_, err = svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new(tooLong),
	})
	checkErrorCode(t, err, "InvalidParameterValueException")

	// VaultName max length (255 chars) succeeds
	maxName := strings.Repeat("a", 255)
	_, err = svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new(maxName),
	})
	if err != nil {
		t.Fatalf("expected 255 char name to succeed: %v", err)
	}
}

func TestListVaultsPagination(t *testing.T) {
	ctx := testContext()
	svc := testService()

	// Create 3 vaults: v-a, v-b, v-c
	names := []string{"v-c", "v-a", "v-b"}
	for _, n := range names {
		_, err := svc.CreateVault(ctx, &api.CreateVaultInput{
			AccountId: new("-"),
			VaultName: new(n),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Invalid limit (< 1); the documentation gives no maximum.
	_, err := svc.ListVaults(ctx, &api.ListVaultsInput{
		AccountId: new("-"),
		Limit:     new(int64(0)),
	})
	checkErrorCode(t, err, "InvalidParameterValueException")

	// List page 1 with limit 2
	page1, err := svc.ListVaults(ctx, &api.ListVaultsInput{
		AccountId: new("-"),
		Limit:     new(int64(2)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(page1.VaultList) != 2 {
		t.Fatalf("expected 2 vaults, got %d", len(page1.VaultList))
	}

	// ASCII sorted: v-a, then v-b
	if *page1.VaultList[0].VaultName != "v-a" || *page1.VaultList[1].VaultName != "v-b" {
		t.Fatalf("unexpected order: %s, %s", *page1.VaultList[0].VaultName, *page1.VaultList[1].VaultName)
	}

	if page1.Marker == nil || *page1.Marker == "" {
		t.Fatal("expected non-empty marker for next page")
	}

	// List page 2 with marker
	page2, err := svc.ListVaults(ctx, &api.ListVaultsInput{
		AccountId: new("-"),
		Limit:     new(int64(2)),
		Marker:    page1.Marker,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(page2.VaultList) != 1 {
		t.Fatalf("expected 1 vault on page 2, got %d", len(page2.VaultList))
	}

	if *page2.VaultList[0].VaultName != "v-c" {
		t.Fatalf("expected v-c, got %s", *page2.VaultList[0].VaultName)
	}

	if page2.Marker != nil {
		t.Fatalf("expected nil marker on final page, got %s", *page2.Marker)
	}
}

func TestTags(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Add tags
	_, err = svc.AddTagsToVault(ctx, &api.AddTagsToVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
		Tags: map[string]string{
			"env":  "dev",
			"team": "alpha",
		},
	})
	if err != nil {
		t.Fatalf("AddTagsToVault failed: %v", err)
	}

	// List tags
	tagsOut, err := svc.ListTagsForVault(ctx, &api.ListTagsForVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
	})
	if err != nil {
		t.Fatalf("ListTagsForVault failed: %v", err)
	}

	if len(tagsOut.Tags) != 2 || tagsOut.Tags["env"] != "dev" || tagsOut.Tags["team"] != "alpha" {
		t.Fatalf("unexpected tags: %v", tagsOut.Tags)
	}

	// Reserved tag prefix aws:
	_, err = svc.AddTagsToVault(ctx, &api.AddTagsToVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
		Tags:      map[string]string{"aws:reserved": "no"},
	})
	checkErrorCode(t, err, "InvalidParameterValueException")

	// Exceed max 10 tags
	tooMany := map[string]string{}
	for i := range 9 {
		tooMany[string(rune('a'+i))] = "val"
	}
	// Currently has 2 tags, adding 9 unique tags would reach 11
	_, err = svc.AddTagsToVault(ctx, &api.AddTagsToVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
		Tags:      tooMany,
	})
	checkErrorCode(t, err, "LimitExceededException")

	// Remove tag
	_, err = svc.RemoveTagsFromVault(ctx, &api.RemoveTagsFromVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
		TagKeys:   []string{"team"},
	})
	if err != nil {
		t.Fatalf("RemoveTagsFromVault failed: %v", err)
	}

	tagsOut, err = svc.ListTagsForVault(ctx, &api.ListTagsForVaultInput{
		AccountId: new("-"),
		VaultName: new("tag-vault"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(tagsOut.Tags) != 1 || tagsOut.Tags["env"] != "dev" {
		t.Fatalf("expected 1 tag env=dev, got %v", tagsOut.Tags)
	}
}

func TestAccessPolicy(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Get before setting returns 404
	_, err = svc.GetVaultAccessPolicy(ctx, &api.GetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
	})
	checkErrorCode(t, err, "ResourceNotFoundException")

	// Set policy
	samplePolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"glacier:*"}]}`
	_, err = svc.SetVaultAccessPolicy(ctx, &api.SetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
		Policy:    &api.VaultAccessPolicy{Policy: new(samplePolicy)},
	})
	if err != nil {
		t.Fatalf("SetVaultAccessPolicy failed: %v", err)
	}

	// Get policy
	gotPolicy, err := svc.GetVaultAccessPolicy(ctx, &api.GetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
	})
	if err != nil {
		t.Fatalf("GetVaultAccessPolicy failed: %v", err)
	}

	if *gotPolicy.Policy.Policy != samplePolicy {
		t.Fatalf("expected %s, got %s", samplePolicy, *gotPolicy.Policy.Policy)
	}

	// Exceed 20 KB limit
	hugePolicy := strings.Repeat("x", 20*1024+1)
	_, err = svc.SetVaultAccessPolicy(ctx, &api.SetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
		Policy:    &api.VaultAccessPolicy{Policy: new(hugePolicy)},
	})
	checkErrorCode(t, err, "LimitExceededException")

	// Delete policy
	_, err = svc.DeleteVaultAccessPolicy(ctx, &api.DeleteVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
	})
	if err != nil {
		t.Fatalf("DeleteVaultAccessPolicy failed: %v", err)
	}

	// Get after delete returns 404
	_, err = svc.GetVaultAccessPolicy(ctx, &api.GetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
	})
	checkErrorCode(t, err, "ResourceNotFoundException")

	// Idempotent delete
	_, err = svc.DeleteVaultAccessPolicy(ctx, &api.DeleteVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new("policy-vault"),
	})
	if err != nil {
		t.Fatalf("expected delete to be idempotent: %v", err)
	}
}

func TestNotifications(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Get before setting returns 404
	_, err = svc.GetVaultNotifications(ctx, &api.GetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
	})
	checkErrorCode(t, err, "ResourceNotFoundException")

	// Invalid SNS ARN
	_, err = svc.SetVaultNotifications(ctx, &api.SetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
		VaultNotificationConfig: &api.VaultNotificationConfig{
			SNSTopic: new("not-an-arn"),
			Events:   []string{"ArchiveRetrievalCompleted"},
		},
	})
	checkErrorCode(t, err, "InvalidParameterValueException")

	// Invalid event name
	_, err = svc.SetVaultNotifications(ctx, &api.SetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
		VaultNotificationConfig: &api.VaultNotificationConfig{
			SNSTopic: new("arn:aws:sns:us-east-1:123456789012:topic"),
			Events:   []string{"UnknownEvent"},
		},
	})
	checkErrorCode(t, err, "InvalidParameterValueException")

	// Set valid notifications
	snsTopic := "arn:aws:sns:us-east-1:123456789012:topic"
	_, err = svc.SetVaultNotifications(ctx, &api.SetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
		VaultNotificationConfig: &api.VaultNotificationConfig{
			SNSTopic: new(snsTopic),
			Events:   []string{"ArchiveRetrievalCompleted", "InventoryRetrievalCompleted"},
		},
	})
	if err != nil {
		t.Fatalf("SetVaultNotifications failed: %v", err)
	}

	// Get notifications
	gotCfg, err := svc.GetVaultNotifications(ctx, &api.GetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
	})
	if err != nil {
		t.Fatalf("GetVaultNotifications failed: %v", err)
	}

	if *gotCfg.VaultNotificationConfig.SNSTopic != snsTopic {
		t.Fatalf("expected topic %s, got %s", snsTopic, *gotCfg.VaultNotificationConfig.SNSTopic)
	}

	if len(gotCfg.VaultNotificationConfig.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(gotCfg.VaultNotificationConfig.Events))
	}

	// Delete notifications
	_, err = svc.DeleteVaultNotifications(ctx, &api.DeleteVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
	})
	if err != nil {
		t.Fatalf("DeleteVaultNotifications failed: %v", err)
	}

	// Get after delete returns 404
	_, err = svc.GetVaultNotifications(ctx, &api.GetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
	})
	checkErrorCode(t, err, "ResourceNotFoundException")

	// Idempotent delete
	_, err = svc.DeleteVaultNotifications(ctx, &api.DeleteVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new("notify-vault"),
	})
	if err != nil {
		t.Fatalf("expected idempotent delete: %v", err)
	}
}

func TestInventory(t *testing.T) {
	ctx := testContext()
	svc := testService()

	_, err := svc.CreateVault(ctx, &api.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new("inv-vault"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.AddTagsToVault(ctx, &api.AddTagsToVaultInput{
		AccountId: new("-"),
		VaultName: new("inv-vault"),
		Tags:      map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Resources(ctx)
	if err != nil {
		t.Fatalf("Resources failed: %v", err)
	}

	if len(res) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(res))
	}

	r := res[0]
	if r.Type != "vault" || r.Name != "inv-vault" || r.Account != "123456789012" || r.Region != "us-east-1" {
		t.Fatalf("unexpected resource metadata: %+v", r)
	}

	expectedARN := "arn:aws:glacier:us-east-1:123456789012:vaults/inv-vault"
	if r.ARN != expectedARN {
		t.Fatalf("expected ARN %s, got %s", expectedARN, r.ARN)
	}

	if r.Tags["env"] != "prod" {
		t.Fatalf("expected env=prod tag, got %v", r.Tags)
	}

	details, ok := r.Details.(map[string]any)
	if !ok || details["RecordsOnly"] != true || details["ExecutionReady"] != false {
		t.Fatalf("unexpected details: %v", r.Details)
	}
}

package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glacier"
	"github.com/aws/aws-sdk-go-v2/service/glacier/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func glacierClient(s *httptest.Server) *glacier.Client {
	return glacier.New(glacier.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  creds("test"),
		Retryer:      aws.NopRetryer{},
	})
}

func TestGlacierLifecycleAndCrud(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := glacierClient(s)

	vaultName := "test-vault-lifecycle"

	// 1. CreateVault
	_, err := c.CreateVault(ctx, &glacier.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("CreateVault failed: %v", err)
	}

	// Idempotent CreateVault
	_, err = c.CreateVault(ctx, &glacier.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("idempotent CreateVault failed: %v", err)
	}

	// 2. DescribeVault
	desc, err := c.DescribeVault(ctx, &glacier.DescribeVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("DescribeVault failed: %v", err)
	}

	if *desc.VaultName != vaultName {
		t.Fatalf("expected vault name %s, got %s", vaultName, *desc.VaultName)
	}

	expectedARN := fmt.Sprintf("arn:aws:glacier:us-east-1:000000000000:vaults/%s", vaultName)
	if *desc.VaultARN != expectedARN {
		t.Fatalf("expected ARN %s, got %s", expectedARN, *desc.VaultARN)
	}

	if desc.NumberOfArchives != 0 || desc.SizeInBytes != 0 {
		t.Fatalf("expected 0 archives/bytes, got %d / %d", desc.NumberOfArchives, desc.SizeInBytes)
	}

	if desc.CreationDate == nil || *desc.CreationDate == "" {
		t.Fatal("expected non-empty CreationDate")
	}

	// 3. ListVaults
	listOut, err := c.ListVaults(ctx, &glacier.ListVaultsInput{
		AccountId: new("-"),
		Limit:     new(int32(10)),
	})
	if err != nil {
		t.Fatalf("ListVaults failed: %v", err)
	}

	found := false
	for _, v := range listOut.VaultList {
		if *v.VaultName == vaultName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("vault %s not found in ListVaults output", vaultName)
	}

	// 4. Tags
	_, err = c.AddTagsToVault(ctx, &glacier.AddTagsToVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
		Tags: map[string]string{
			"Environment": "Test",
			"Owner":       "GlacierTeam",
		},
	})
	if err != nil {
		t.Fatalf("AddTagsToVault failed: %v", err)
	}

	tagsOut, err := c.ListTagsForVault(ctx, &glacier.ListTagsForVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("ListTagsForVault failed: %v", err)
	}

	if tagsOut.Tags["Environment"] != "Test" || tagsOut.Tags["Owner"] != "GlacierTeam" {
		t.Fatalf("unexpected tags: %v", tagsOut.Tags)
	}

	_, err = c.RemoveTagsFromVault(ctx, &glacier.RemoveTagsFromVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
		TagKeys:   []string{"Environment"},
	})
	if err != nil {
		t.Fatalf("RemoveTagsFromVault failed: %v", err)
	}

	tagsOut2, err := c.ListTagsForVault(ctx, &glacier.ListTagsForVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("ListTagsForVault after remove failed: %v", err)
	}

	if _, exists := tagsOut2.Tags["Environment"]; exists {
		t.Fatal("expected Environment tag to be removed")
	}

	if tagsOut2.Tags["Owner"] != "GlacierTeam" {
		t.Fatalf("expected Owner tag to persist, got: %v", tagsOut2.Tags)
	}

	// 5. Access Policy
	policyDoc := `{"Version":"2012-10-17","Statement":[` +
		`{"Sid":"test","Effect":"Allow","Principal":"*","Action":"glacier:*"}]}`
	_, err = c.SetVaultAccessPolicy(ctx, &glacier.SetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
		Policy: &types.VaultAccessPolicy{
			Policy: new(policyDoc),
		},
	})
	if err != nil {
		t.Fatalf("SetVaultAccessPolicy failed: %v", err)
	}

	getPolicy, err := c.GetVaultAccessPolicy(ctx, &glacier.GetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("GetVaultAccessPolicy failed: %v", err)
	}

	if getPolicy.Policy == nil || *getPolicy.Policy.Policy != policyDoc {
		t.Fatalf("expected policy %s, got %v", policyDoc, getPolicy.Policy)
	}

	_, err = c.DeleteVaultAccessPolicy(ctx, &glacier.DeleteVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("DeleteVaultAccessPolicy failed: %v", err)
	}

	_, err = c.GetVaultAccessPolicy(ctx, &glacier.GetVaultAccessPolicyInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err == nil {
		t.Fatal("expected error getting deleted access policy")
	}
	code(t, err, "ResourceNotFoundException")

	// 6. Notifications
	snsTopic := "arn:aws:sns:us-east-1:000000000000:glacier-topic"
	_, err = c.SetVaultNotifications(ctx, &glacier.SetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
		VaultNotificationConfig: &types.VaultNotificationConfig{
			SNSTopic: new(snsTopic),
			Events:   []string{"ArchiveRetrievalCompleted", "InventoryRetrievalCompleted"},
		},
	})
	if err != nil {
		t.Fatalf("SetVaultNotifications failed: %v", err)
	}

	getNotif, err := c.GetVaultNotifications(ctx, &glacier.GetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("GetVaultNotifications failed: %v", err)
	}

	if getNotif.VaultNotificationConfig == nil || *getNotif.VaultNotificationConfig.SNSTopic != snsTopic {
		t.Fatalf("unexpected notification config: %v", getNotif.VaultNotificationConfig)
	}

	_, err = c.DeleteVaultNotifications(ctx, &glacier.DeleteVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("DeleteVaultNotifications failed: %v", err)
	}

	_, err = c.GetVaultNotifications(ctx, &glacier.GetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err == nil {
		t.Fatal("expected error getting deleted notifications")
	}
	code(t, err, "ResourceNotFoundException")

	// 7. DeleteVault
	_, err = c.DeleteVault(ctx, &glacier.DeleteVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("DeleteVault failed: %v", err)
	}

	_, err = c.DescribeVault(ctx, &glacier.DescribeVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err == nil {
		t.Fatal("expected error describing deleted vault")
	}
	code(t, err, "ResourceNotFoundException")
}

func TestGlacierValidationErrors(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := glacierClient(s)

	// Account mismatch
	_, err := c.CreateVault(ctx, &glacier.CreateVaultInput{
		AccountId: new("123456789012"),
		VaultName: new("some-vault"),
	})
	if err == nil {
		t.Fatal("expected error on account mismatch")
	}
	code(t, err, "InvalidParameterValueException")

	// Invalid vault name
	_, err = c.CreateVault(ctx, &glacier.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new("invalid vault name with spaces!"),
	})
	if err == nil {
		t.Fatal("expected error on invalid vault name")
	}
	code(t, err, "InvalidParameterValueException")

	// Limit out of range
	_, err = c.ListVaults(ctx, &glacier.ListVaultsInput{
		AccountId: new("-"),
		Limit:     new(int32(0)),
	})
	if err == nil {
		t.Fatal("expected error on list limit < 1")
	}
	code(t, err, "InvalidParameterValueException")

	// Valid vault for tag limits
	vName := "tag-limit-vault"
	_, err = c.CreateVault(ctx, &glacier.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new(vName),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 11 tags exceeding limit of 10
	elevenTags := make(map[string]string)
	for i := 1; i <= 11; i++ {
		elevenTags[fmt.Sprintf("key%d", i)] = "val"
	}

	_, err = c.AddTagsToVault(ctx, &glacier.AddTagsToVaultInput{
		AccountId: new("-"),
		VaultName: new(vName),
		Tags:      elevenTags,
	})
	if err == nil {
		t.Fatal("expected limit exceeded error for > 10 tags")
	}
	code(t, err, "LimitExceededException")

	// Invalid SNS ARN
	_, err = c.SetVaultNotifications(ctx, &glacier.SetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new(vName),
		VaultNotificationConfig: &types.VaultNotificationConfig{
			SNSTopic: new("not-an-arn"),
			Events:   []string{"ArchiveRetrievalCompleted"},
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid SNS ARN")
	}
	code(t, err, "InvalidParameterValueException")

	// Invalid Event
	_, err = c.SetVaultNotifications(ctx, &glacier.SetVaultNotificationsInput{
		AccountId: new("-"),
		VaultName: new(vName),
		VaultNotificationConfig: &types.VaultNotificationConfig{
			SNSTopic: new("arn:aws:sns:us-east-1:000000000000:topic"),
			Events:   []string{"NonExistentEvent"},
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid notification event")
	}
	code(t, err, "InvalidParameterValueException")
}

func TestGlacierRGTAIntegration(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := glacierClient(s)
	rg := resourcegroupstaggingapi.New(resourcegroupstaggingapi.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  creds("test"),
	})

	vaultName := "rgta-vault-test"
	_, err := c.CreateVault(ctx, &glacier.CreateVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("CreateVault failed: %v", err)
	}

	vaultARN := fmt.Sprintf("arn:aws:glacier:us-east-1:000000000000:vaults/%s", vaultName)

	// Tag via RGTA
	_, err = rg.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{vaultARN},
		Tags: map[string]string{
			"ManagedBy": "RGTA",
		},
	})
	if err != nil {
		t.Fatalf("RGTA TagResources failed: %v", err)
	}

	// Verify via glacier client
	tagsOut, err := c.ListTagsForVault(ctx, &glacier.ListTagsForVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("ListTagsForVault failed: %v", err)
	}

	if tagsOut.Tags["ManagedBy"] != "RGTA" {
		t.Fatalf("expected RGTA tag, got: %v", tagsOut.Tags)
	}

	// Untag via RGTA
	_, err = rg.UntagResources(ctx, &resourcegroupstaggingapi.UntagResourcesInput{
		ResourceARNList: []string{vaultARN},
		TagKeys:         []string{"ManagedBy"},
	})
	if err != nil {
		t.Fatalf("RGTA UntagResources failed: %v", err)
	}

	tagsOut2, err := c.ListTagsForVault(ctx, &glacier.ListTagsForVaultInput{
		AccountId: new("-"),
		VaultName: new(vaultName),
	})
	if err != nil {
		t.Fatalf("ListTagsForVault after RGTA untag failed: %v", err)
	}

	if _, exists := tagsOut2.Tags["ManagedBy"]; exists {
		t.Fatal("expected tag to be removed by RGTA")
	}
}

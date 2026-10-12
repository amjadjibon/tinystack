package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/verifiedpermissions"
	"github.com/aws/aws-sdk-go-v2/service/verifiedpermissions/types"
)

func verifiedPermissionsClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       region,
			BaseEndpoint: new(s.URL),
			Credentials:  creds(key),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestVerifiedPermissionsLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := verifiedPermissionsClient(s, "test", "us-east-1")

	// 1. Policy Store CRUD
	storeCreated, err := c.CreatePolicyStore(ctx, &sdk.CreatePolicyStoreInput{
		Description: aws.String("Production policy store"),
		ValidationSettings: &types.ValidationSettings{
			Mode: types.ValidationModeOff,
		},
		Tags: map[string]string{
			"Environment": "Production",
			"Service":     "Auth",
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore failed: %v", err)
	}

	storeID := storeCreated.PolicyStoreId
	storeARN := storeCreated.Arn
	if !strings.HasPrefix(aws.ToString(storeARN), "arn:aws:verifiedpermissions:us-east-1:") {
		t.Fatalf("unexpected store ARN: %s", aws.ToString(storeARN))
	}

	// GetPolicyStore
	gotStore, err := c.GetPolicyStore(ctx, &sdk.GetPolicyStoreInput{
		PolicyStoreId: storeID,
	})
	if err != nil || aws.ToString(gotStore.Arn) != aws.ToString(storeARN) {
		t.Fatalf("GetPolicyStore failed: %v, got %+v", err, gotStore)
	}

	if gotStore.ValidationSettings.Mode != types.ValidationModeOff {
		t.Fatalf("expected mode OFF, got %v", gotStore.ValidationSettings.Mode)
	}

	// UpdatePolicyStore
	updatedStore, err := c.UpdatePolicyStore(ctx, &sdk.UpdatePolicyStoreInput{
		PolicyStoreId: storeID,
		Description:   aws.String("Updated policy store"),
		ValidationSettings: &types.ValidationSettings{
			Mode: types.ValidationModeStrict,
		},
	})
	if err != nil || aws.ToString(updatedStore.PolicyStoreId) != aws.ToString(storeID) {
		t.Fatalf("UpdatePolicyStore failed: %v", err)
	}

	gotUpdated, err := c.GetPolicyStore(ctx, &sdk.GetPolicyStoreInput{
		PolicyStoreId: storeID,
	})
	if err != nil || gotUpdated.ValidationSettings.Mode != types.ValidationModeStrict {
		t.Fatalf("expected mode STRICT after update, got %+v, err: %v", gotUpdated, err)
	}

	// ListPolicyStores
	stores, err := c.ListPolicyStores(ctx, &sdk.ListPolicyStoresInput{
		MaxResults: aws.Int32(10),
	})
	if err != nil || len(stores.PolicyStores) != 1 {
		t.Fatalf("ListPolicyStores failed: %v, count: %d", err, len(stores.PolicyStores))
	}

	// 2. Schema CRUD
	cedarJSON := `{"PhotoApp": {"entityTypes": {}, "actions": {}}}`
	putSchema, err := c.PutSchema(ctx, &sdk.PutSchemaInput{
		PolicyStoreId: storeID,
		Definition: &types.SchemaDefinitionMemberCedarJson{
			Value: cedarJSON,
		},
	})
	if err != nil {
		t.Fatalf("PutSchema failed: %v", err)
	}

	if len(putSchema.Namespaces) != 1 || putSchema.Namespaces[0] != "PhotoApp" {
		t.Fatalf("expected namespace PhotoApp, got %v", putSchema.Namespaces)
	}

	gotSchema, err := c.GetSchema(ctx, &sdk.GetSchemaInput{
		PolicyStoreId: storeID,
	})
	if err != nil || aws.ToString(gotSchema.Schema) != cedarJSON {
		t.Fatalf("GetSchema failed: %v, got %v", err, gotSchema)
	}

	// 3. Policy CRUD
	permitStmt := `permit(principal, action == Action::"view", resource);`
	polCreated, err := c.CreatePolicy(ctx, &sdk.CreatePolicyInput{
		PolicyStoreId: storeID,
		Definition: &types.PolicyDefinitionMemberStatic{
			Value: types.StaticPolicyDefinition{
				Description: aws.String("Allow view action"),
				Statement:   aws.String(permitStmt),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicy failed: %v", err)
	}

	if polCreated.Effect != types.PolicyEffectPermit {
		t.Fatalf("expected Permit effect, got %v", polCreated.Effect)
	}

	policyID := polCreated.PolicyId

	// GetPolicy
	gotPolicy, err := c.GetPolicy(ctx, &sdk.GetPolicyInput{
		PolicyStoreId: storeID,
		PolicyId:      policyID,
	})
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}

	statDetail, ok := gotPolicy.Definition.(*types.PolicyDefinitionDetailMemberStatic)
	if !ok || aws.ToString(statDetail.Value.Statement) != permitStmt {
		t.Fatalf("unexpected policy statement: %+v", gotPolicy.Definition)
	}

	// ListPolicies
	pols, err := c.ListPolicies(ctx, &sdk.ListPoliciesInput{
		PolicyStoreId: storeID,
	})
	if err != nil || len(pols.Policies) != 1 {
		t.Fatalf("ListPolicies failed: %v, count: %d", err, len(pols.Policies))
	}

	// UpdatePolicy (change to forbid)
	forbidStmt := `forbid(principal, action == Action::"view", resource);`
	updatedPolicy, err := c.UpdatePolicy(ctx, &sdk.UpdatePolicyInput{
		PolicyStoreId: storeID,
		PolicyId:      policyID,
		Definition: &types.UpdatePolicyDefinitionMemberStatic{
			Value: types.UpdateStaticPolicyDefinition{
				Description: aws.String("Deny view action"),
				Statement:   aws.String(forbidStmt),
			},
		},
	})
	if err != nil || updatedPolicy.Effect != types.PolicyEffectForbid {
		t.Fatalf("UpdatePolicy failed: %v, got %+v", err, updatedPolicy)
	}

	// 4. Tagging
	tagsOut, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: storeARN,
	})
	if err != nil || len(tagsOut.Tags) != 2 {
		t.Fatalf("ListTagsForResource failed: %v, tags: %+v", err, tagsOut)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: storeARN,
		Tags: map[string]string{
			"Owner": "SecurityTeam",
		},
	})
	if err != nil {
		t.Fatalf("TagResource failed: %v", err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: storeARN,
		TagKeys:     []string{"Environment"},
	})
	if err != nil {
		t.Fatalf("UntagResource failed: %v", err)
	}

	tagsOut, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: storeARN,
	})
	if err != nil || len(tagsOut.Tags) != 2 || tagsOut.Tags["Owner"] != "SecurityTeam" {
		t.Fatalf("unexpected tags after update: %+v", tagsOut.Tags)
	}

	// RGTA integration
	taggingClient := rgta.New(
		rgta.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
		},
	)
	rgtaRes, err := taggingClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{aws.ToString(storeARN)},
			Tags:            map[string]string{"RGTA": "True"},
		},
	)
	if err != nil || len(rgtaRes.FailedResourcesMap) != 0 {
		t.Fatalf("RGTA TagResources failed: %v, failed: %+v", err, rgtaRes.FailedResourcesMap)
	}

	tagsOut, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: storeARN,
	})
	if err != nil || tagsOut.Tags["RGTA"] != "True" {
		t.Fatalf("expected RGTA tag on store: %+v", tagsOut.Tags)
	}

	// 5. Clean up / Delete
	_, err = c.DeletePolicy(ctx, &sdk.DeletePolicyInput{
		PolicyStoreId: storeID,
		PolicyId:      policyID,
	})
	if err != nil {
		t.Fatalf("DeletePolicy failed: %v", err)
	}

	_, err = c.GetPolicy(ctx, &sdk.GetPolicyInput{
		PolicyStoreId: storeID,
		PolicyId:      policyID,
	})
	if err == nil {
		t.Fatal("expected GetPolicy to fail after delete")
	}

	_, err = c.DeletePolicyStore(ctx, &sdk.DeletePolicyStoreInput{
		PolicyStoreId: storeID,
	})
	if err != nil {
		t.Fatalf("DeletePolicyStore failed: %v", err)
	}

	_, err = c.GetPolicyStore(ctx, &sdk.GetPolicyStoreInput{
		PolicyStoreId: storeID,
	})
	if err == nil {
		t.Fatal("expected GetPolicyStore to fail after delete")
	}
}

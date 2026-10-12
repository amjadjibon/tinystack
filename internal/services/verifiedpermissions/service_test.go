package verifiedpermissions

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type fixture struct {
	svc   *Service
	store storage.Store
}

func newFixture() fixture {
	store := memory.New(nil, nil)
	return fixture{svc: New(store), store: store}
}

func contextIn(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func testContext() context.Context {
	return contextIn("123456789012", "us-east-1")
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()

	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) {
		t.Fatalf("want error code %q, got error %v", code, err)
	}

	if awsErr.Code != code {
		t.Fatalf("want error code %q, got %q (%v)", code, awsErr.Code, err)
	}
}

func TestPolicyStoreCRUD(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	// Validation checks
	_, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{})
	wantCode(t, err, "ValidationException")

	_, err = f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		ValidationSettings: &api.ValidationSettings{Mode: new("INVALID")},
	})
	wantCode(t, err, "ValidationException")

	// Create valid policy store
	created, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		Description: new("Test store"),
		Tags:        map[string]string{"env": "dev"},
		ValidationSettings: &api.ValidationSettings{
			Mode: new("OFF"),
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore: %v", err)
	}

	if created.PolicyStoreId == nil || *created.PolicyStoreId == "" {
		t.Fatal("expected non-empty PolicyStoreId")
	}

	if created.Arn == nil || *created.Arn == "" {
		t.Fatal("expected non-empty Arn")
	}

	storeID := *created.PolicyStoreId

	// Get policy store
	got, err := f.svc.GetPolicyStore(ctx, &api.GetPolicyStoreInput{
		PolicyStoreId: new(storeID),
	})
	if err != nil {
		t.Fatalf("GetPolicyStore: %v", err)
	}

	if *got.PolicyStoreId != storeID {
		t.Fatalf("want store ID %q, got %q", storeID, *got.PolicyStoreId)
	}

	if got.Description == nil || *got.Description != "Test store" {
		t.Fatalf("want description %q, got %v", "Test store", got.Description)
	}

	if got.ValidationSettings == nil || *got.ValidationSettings.Mode != "OFF" {
		t.Fatalf("want mode OFF, got %v", got.ValidationSettings)
	}

	// Update policy store
	updated, err := f.svc.UpdatePolicyStore(ctx, &api.UpdatePolicyStoreInput{
		PolicyStoreId: new(storeID),
		Description:   new("Updated store"),
		ValidationSettings: &api.ValidationSettings{
			Mode: new("STRICT"),
		},
	})
	if err != nil {
		t.Fatalf("UpdatePolicyStore: %v", err)
	}

	if *updated.PolicyStoreId != storeID {
		t.Fatalf("want store ID %q, got %q", storeID, *updated.PolicyStoreId)
	}

	gotUpdated, err := f.svc.GetPolicyStore(ctx, &api.GetPolicyStoreInput{
		PolicyStoreId: new(storeID),
	})
	if err != nil {
		t.Fatalf("GetPolicyStore after update: %v", err)
	}

	if *gotUpdated.Description != "Updated store" {
		t.Fatalf("want description Updated store, got %q", *gotUpdated.Description)
	}

	if *gotUpdated.ValidationSettings.Mode != "STRICT" {
		t.Fatalf("want mode STRICT, got %q", *gotUpdated.ValidationSettings.Mode)
	}

	// List policy stores
	listed, err := f.svc.ListPolicyStores(ctx, &api.ListPolicyStoresInput{
		MaxResults: new(int64(10)),
	})
	if err != nil {
		t.Fatalf("ListPolicyStores: %v", err)
	}

	if len(listed.PolicyStores) != 1 {
		t.Fatalf("want 1 policy store, got %d", len(listed.PolicyStores))
	}

	// Delete policy store
	_, err = f.svc.DeletePolicyStore(ctx, &api.DeletePolicyStoreInput{
		PolicyStoreId: new(storeID),
	})
	if err != nil {
		t.Fatalf("DeletePolicyStore: %v", err)
	}

	_, err = f.svc.GetPolicyStore(ctx, &api.GetPolicyStoreInput{
		PolicyStoreId: new(storeID),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestPolicyStoreIdempotency(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	clientToken := "unique-token-123"
	input := &api.CreatePolicyStoreInput{
		ClientToken: new(clientToken),
		Description: new("Idempotent store"),
		ValidationSettings: &api.ValidationSettings{
			Mode: new("OFF"),
		},
	}

	first, err := f.svc.CreatePolicyStore(ctx, input)
	if err != nil {
		t.Fatalf("first CreatePolicyStore: %v", err)
	}

	second, err := f.svc.CreatePolicyStore(ctx, input)
	if err != nil {
		t.Fatalf("second CreatePolicyStore: %v", err)
	}

	if *first.PolicyStoreId != *second.PolicyStoreId {
		t.Fatalf(
			"expected same PolicyStoreId on idempotent call, got %q and %q",
			*first.PolicyStoreId,
			*second.PolicyStoreId,
		)
	}

	// Different attributes with same clientToken fails with ConflictException
	mismatchInput := &api.CreatePolicyStoreInput{
		ClientToken: new(clientToken),
		Description: new("Different description"),
		ValidationSettings: &api.ValidationSettings{
			Mode: new("OFF"),
		},
	}

	_, err = f.svc.CreatePolicyStore(ctx, mismatchInput)
	wantCode(t, err, "ConflictException")
}

func TestSchemaCRUD(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	// Put schema on nonexistent store
	_, err := f.svc.PutSchema(ctx, &api.PutSchemaInput{
		PolicyStoreId: new("ps-nonexistent"),
		Definition: &api.SchemaDefinition{
			CedarJson: new(`{"App": {"entityTypes": {}, "actions": {}}}`),
		},
	})
	wantCode(t, err, "ResourceNotFoundException")

	// Create store
	store, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		ValidationSettings: &api.ValidationSettings{Mode: new("OFF")},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore: %v", err)
	}

	storeID := *store.PolicyStoreId

	// Get schema before put
	_, err = f.svc.GetSchema(ctx, &api.GetSchemaInput{PolicyStoreId: new(storeID)})
	wantCode(t, err, "ResourceNotFoundException")

	// Put invalid JSON schema
	_, err = f.svc.PutSchema(ctx, &api.PutSchemaInput{
		PolicyStoreId: new(storeID),
		Definition: &api.SchemaDefinition{
			CedarJson: new(`{not valid json}`),
		},
	})
	wantCode(t, err, "ValidationException")

	// Put valid schema with multiple namespaces
	cedarJSON := `{"Beta": {"actions": {}}, "Alpha": {"entityTypes": {}}}`
	putOut, err := f.svc.PutSchema(ctx, &api.PutSchemaInput{
		PolicyStoreId: new(storeID),
		Definition: &api.SchemaDefinition{
			CedarJson: new(cedarJSON),
		},
	})
	if err != nil {
		t.Fatalf("PutSchema: %v", err)
	}

	if len(putOut.Namespaces) != 2 || putOut.Namespaces[0] != "Alpha" || putOut.Namespaces[1] != "Beta" {
		t.Fatalf("expected sorted namespaces [Alpha, Beta], got %v", putOut.Namespaces)
	}

	// Get schema
	getOut, err := f.svc.GetSchema(ctx, &api.GetSchemaInput{
		PolicyStoreId: new(storeID),
	})
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}

	if *getOut.Schema != cedarJSON {
		t.Fatalf("want schema %q, got %q", cedarJSON, *getOut.Schema)
	}

	if len(getOut.Namespaces) != 2 || getOut.Namespaces[0] != "Alpha" || getOut.Namespaces[1] != "Beta" {
		t.Fatalf("expected sorted namespaces in GetSchema, got %v", getOut.Namespaces)
	}
}

func TestPolicyCRUD(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	store, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		ValidationSettings: &api.ValidationSettings{Mode: new("OFF")},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore: %v", err)
	}

	storeID := *store.PolicyStoreId

	// Reject template-linked policy
	_, err = f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
		PolicyStoreId: new(storeID),
		Definition: &api.PolicyDefinition{
			TemplateLinked: &api.TemplateLinkedPolicyDefinition{
				PolicyTemplateId: new("pt-123"),
			},
		},
	})
	wantCode(t, err, "ValidationException")

	// Reject statement not starting with permit( or forbid(
	_, err = f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
		PolicyStoreId: new(storeID),
		Definition: &api.PolicyDefinition{
			Static: &api.StaticPolicyDefinition{
				Statement: new("when (action == Action::\"view\")"),
			},
		},
	})
	wantCode(t, err, "ValidationException")

	// Create permit policy
	permitStmt := `permit(principal, action == Action::"view", resource);`
	pol1, err := f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
		PolicyStoreId: new(storeID),
		Definition: &api.PolicyDefinition{
			Static: &api.StaticPolicyDefinition{
				Description: new("Read policy"),
				Statement:   new(permitStmt),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicy permit: %v", err)
	}

	if *pol1.Effect != "Permit" {
		t.Fatalf("want effect Permit, got %q", *pol1.Effect)
	}

	if *pol1.PolicyType != "STATIC" {
		t.Fatalf("want policyType STATIC, got %q", *pol1.PolicyType)
	}

	pol1ID := *pol1.PolicyId

	// Create forbid policy
	forbidStmt := `forbid(principal, action == Action::"delete", resource);`
	pol2, err := f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
		PolicyStoreId: new(storeID),
		Definition: &api.PolicyDefinition{
			Static: &api.StaticPolicyDefinition{
				Description: new("Deny delete"),
				Statement:   new(forbidStmt),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicy forbid: %v", err)
	}

	if *pol2.Effect != "Forbid" {
		t.Fatalf("want effect Forbid, got %q", *pol2.Effect)
	}

	// Get policy
	gotPol1, err := f.svc.GetPolicy(ctx, &api.GetPolicyInput{
		PolicyStoreId: new(storeID),
		PolicyId:      new(pol1ID),
	})
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}

	if *gotPol1.Effect != "Permit" {
		t.Fatalf("want effect Permit, got %q", *gotPol1.Effect)
	}

	if *gotPol1.Definition.Static.Statement != permitStmt {
		t.Fatalf("statement mismatch: got %q", *gotPol1.Definition.Static.Statement)
	}

	// List policies
	listed, err := f.svc.ListPolicies(ctx, &api.ListPoliciesInput{
		PolicyStoreId: new(storeID),
	})
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}

	if len(listed.Policies) != 2 {
		t.Fatalf("want 2 policies, got %d", len(listed.Policies))
	}

	// Update policy from permit to forbid
	updatedStmt := `forbid(principal, action == Action::"view", resource);`
	upPol, err := f.svc.UpdatePolicy(ctx, &api.UpdatePolicyInput{
		PolicyStoreId: new(storeID),
		PolicyId:      new(pol1ID),
		Definition: &api.UpdatePolicyDefinition{
			Static: &api.UpdateStaticPolicyDefinition{
				Description: new("Updated deny"),
				Statement:   new(updatedStmt),
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}

	if *upPol.Effect != "Forbid" {
		t.Fatalf("want updated effect Forbid, got %q", *upPol.Effect)
	}

	// Delete policy
	_, err = f.svc.DeletePolicy(ctx, &api.DeletePolicyInput{
		PolicyStoreId: new(storeID),
		PolicyId:      new(pol1ID),
	})
	if err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}

	_, err = f.svc.GetPolicy(ctx, &api.GetPolicyInput{
		PolicyStoreId: new(storeID),
		PolicyId:      new(pol1ID),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestDeletePolicyStoreCascade(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	store, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		ValidationSettings: &api.ValidationSettings{Mode: new("OFF")},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore: %v", err)
	}

	storeID := *store.PolicyStoreId

	// Put schema
	_, err = f.svc.PutSchema(ctx, &api.PutSchemaInput{
		PolicyStoreId: new(storeID),
		Definition: &api.SchemaDefinition{
			CedarJson: new(`{"App": {}}`),
		},
	})
	if err != nil {
		t.Fatalf("PutSchema: %v", err)
	}

	// Create policy
	pol, err := f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
		PolicyStoreId: new(storeID),
		Definition: &api.PolicyDefinition{
			Static: &api.StaticPolicyDefinition{
				Statement: new(`permit(principal, action, resource);`),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	policyID := *pol.PolicyId

	// Delete store
	_, err = f.svc.DeletePolicyStore(ctx, &api.DeletePolicyStoreInput{
		PolicyStoreId: new(storeID),
	})
	if err != nil {
		t.Fatalf("DeletePolicyStore: %v", err)
	}

	// Schema should be gone
	_, err = f.svc.GetSchema(ctx, &api.GetSchemaInput{PolicyStoreId: new(storeID)})
	wantCode(t, err, "ResourceNotFoundException")

	// Policy should be gone
	_, err = f.svc.GetPolicy(ctx, &api.GetPolicyInput{
		PolicyStoreId: new(storeID),
		PolicyId:      new(policyID),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestTags(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	store, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		ValidationSettings: &api.ValidationSettings{Mode: new("OFF")},
		Tags:               map[string]string{"initial": "val"},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore: %v", err)
	}

	arn := *store.Arn

	// List initial tags
	tagList, err := f.svc.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: new(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource: %v", err)
	}

	if tagList.Tags["initial"] != "val" {
		t.Fatalf("want initial=val, got %v", tagList.Tags)
	}

	// TagResource
	_, err = f.svc.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: new(arn),
		Tags:        map[string]string{"env": "staging", "owner": "sec"},
	})
	if err != nil {
		t.Fatalf("TagResource: %v", err)
	}

	tagList, err = f.svc.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: new(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource after tag: %v", err)
	}

	if len(tagList.Tags) != 3 || tagList.Tags["env"] != "staging" || tagList.Tags["owner"] != "sec" {
		t.Fatalf("unexpected tags: %v", tagList.Tags)
	}

	// UntagResource
	_, err = f.svc.UntagResource(ctx, &api.UntagResourceInput{
		ResourceArn: new(arn),
		TagKeys:     []string{"initial", "env"},
	})
	if err != nil {
		t.Fatalf("UntagResource: %v", err)
	}

	tagList, err = f.svc.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: new(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource after untag: %v", err)
	}

	if len(tagList.Tags) != 1 || tagList.Tags["owner"] != "sec" {
		t.Fatalf("want only owner tag, got %v", tagList.Tags)
	}
}

func TestInventory(t *testing.T) {
	ctx := testContext()
	f := newFixture()

	store, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		Description:        new("Inv Store"),
		ValidationSettings: &api.ValidationSettings{Mode: new("OFF")},
		Tags:               map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("CreatePolicyStore: %v", err)
	}

	_, err = f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
		PolicyStoreId: store.PolicyStoreId,
		Definition: &api.PolicyDefinition{
			Static: &api.StaticPolicyDefinition{
				Statement: new(`permit(principal, action, resource);`),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	resources, err := f.svc.Resources(ctx)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}

	if len(resources) != 2 {
		t.Fatalf("want 2 resources in inventory, got %d", len(resources))
	}

	var foundStore, foundPolicy bool
	for _, r := range resources {
		switch r.Type {
		case "policystore":
			foundStore = true
			if r.Tags["env"] != "prod" {
				t.Fatalf("missing store tags in inventory: %v", r.Tags)
			}
		case "policy":
			foundPolicy = true
		}
	}

	if !foundStore || !foundPolicy {
		t.Fatalf(
			"expected both policystore and policy in inventory, got foundStore=%v, foundPolicy=%v",
			foundStore,
			foundPolicy,
		)
	}
}

func TestPolicyEffectAndPageLimits(t *testing.T) {
	f := newFixture()
	ctx := testContext()

	store, err := f.svc.CreatePolicyStore(ctx, &api.CreatePolicyStoreInput{
		ValidationSettings: &api.ValidationSettings{Mode: new("OFF")},
	})
	if err != nil {
		t.Fatal(err)
	}

	for statement, want := range map[string]string{
		"// allow readers\n@id(\"readers\")\npermit (principal, action, resource);": "Permit",
		"  forbid(principal, action, resource);":                                    "Forbid",
	} {
		created, createErr := f.svc.CreatePolicy(ctx, &api.CreatePolicyInput{
			PolicyStoreId: store.PolicyStoreId,
			Definition:    &api.PolicyDefinition{Static: &api.StaticPolicyDefinition{Statement: new(statement)}},
		})
		if createErr != nil || *created.Effect != want {
			t.Fatalf("%q: %v %v", statement, created, createErr)
		}
	}

	_, err = f.svc.ListPolicies(
		ctx,
		&api.ListPoliciesInput{PolicyStoreId: store.PolicyStoreId, MaxResults: new(int64(51))},
	)
	wantCode(t, err, "ValidationException")

	_, err = f.svc.ListPolicyStores(ctx, &api.ListPolicyStoresInput{MaxResults: new(int64(0))})
	wantCode(t, err, "ValidationException")
}

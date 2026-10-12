package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/verifiedpermissions"
	"github.com/aws/aws-sdk-go-v2/service/verifiedpermissions/types"
)

func verifiedPermissionsTemplate(storeDesc, mode, policyStmt, policyDesc string) string {
	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"MyStore": map[string]any{
				"Type": "AWS::VerifiedPermissions::PolicyStore",
				"Properties": map[string]any{
					"Description": storeDesc,
					"ValidationSettings": map[string]any{
						"Mode": mode,
					},
					"Schema": map[string]any{
						"CedarJson": `{"App": {"actions": {}, "entityTypes": {}}}`,
					},
					"Tags": []any{
						map[string]any{"Key": "Environment", "Value": "Dev"},
					},
				},
			},
			"MyPolicy": map[string]any{
				"Type": "AWS::VerifiedPermissions::Policy",
				"Properties": map[string]any{
					"PolicyStoreId": map[string]any{"Ref": "MyStore"},
					"Definition": map[string]any{
						"Static": map[string]any{
							"Statement":   policyStmt,
							"Description": policyDesc,
						},
					},
				},
			},
		},
		"Outputs": map[string]any{
			"StoreRef":  map[string]any{"Value": map[string]any{"Ref": "MyStore"}},
			"StoreArn":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyStore", "Arn"}}},
			"PolicyRef": map[string]any{"Value": map[string]any{"Ref": "MyPolicy"}},
			"PolicyStoreIdAtt": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"MyPolicy", "PolicyStoreId"}},
			},
			"PolicyTypeAtt": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyPolicy", "PolicyType"}}},
		},
	})

	return string(raw)
}

func TestCloudFormationVerifiedPermissionsLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	vpClient := verifiedPermissionsClient(s, "test", "us-east-1")

	// 1. Create Stack
	initialStmt := `permit(principal, action == Action::"view", resource);`
	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName: aws.String("vp-stack"),
			TemplateBody: aws.String(
				verifiedPermissionsTemplate("Initial store", "OFF", initialStmt, "Initial policy"),
			),
		},
	)
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	st := wantStatus(ctx, t, cf, "vp-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	storeID := outputs["StoreRef"]
	storeARN := outputs["StoreArn"]
	policyID := outputs["PolicyRef"]

	if storeID == "" || storeARN == "" || policyID == "" {
		t.Fatalf("unexpected empty outputs: %+v", outputs)
	}

	if !strings.HasPrefix(storeARN, "arn:aws:verifiedpermissions:us-east-1:") {
		t.Fatalf("unexpected store ARN format: %s", storeARN)
	}

	if outputs["PolicyStoreIdAtt"] != storeID {
		t.Fatalf("expected PolicyStoreIdAtt %q, got %q", storeID, outputs["PolicyStoreIdAtt"])
	}

	if outputs["PolicyTypeAtt"] != "STATIC" {
		t.Fatalf("expected PolicyType STATIC, got %q", outputs["PolicyTypeAtt"])
	}

	// Verify resources in Verified Permissions
	store, err := vpClient.GetPolicyStore(ctx, &sdk.GetPolicyStoreInput{
		PolicyStoreId: aws.String(storeID),
	})
	if err != nil || aws.ToString(store.Description) != "Initial store" {
		t.Fatalf("unexpected store in service: %+v, err: %v", store, err)
	}

	schema, err := vpClient.GetSchema(ctx, &sdk.GetSchemaInput{
		PolicyStoreId: aws.String(storeID),
	})
	if err != nil || len(schema.Namespaces) != 1 || schema.Namespaces[0] != "App" {
		t.Fatalf("unexpected schema in service: %+v, err: %v", schema, err)
	}

	policy, err := vpClient.GetPolicy(ctx, &sdk.GetPolicyInput{
		PolicyStoreId: aws.String(storeID),
		PolicyId:      aws.String(policyID),
	})
	if err != nil || policy.Effect != types.PolicyEffectPermit {
		t.Fatalf("unexpected policy in service: %+v, err: %v", policy, err)
	}

	// 2. Update Stack
	updatedStmt := `forbid(principal, action == Action::"view", resource);`
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName: aws.String("vp-stack"),
			TemplateBody: aws.String(
				verifiedPermissionsTemplate("Updated store", "STRICT", updatedStmt, "Updated policy"),
			),
		},
	)
	if err != nil {
		t.Fatalf("UpdateStack failed: %v", err)
	}

	st = wantStatus(ctx, t, cf, "vp-stack", cfntypes.StackStatusUpdateComplete)
	updatedOutputs := outputsOf(&st)

	if updatedOutputs["StoreRef"] != storeID {
		t.Fatalf("store ID changed unexpectedly: %s vs %s", updatedOutputs["StoreRef"], storeID)
	}

	if updatedOutputs["PolicyRef"] != policyID {
		t.Fatalf("policy ID changed unexpectedly: %s vs %s", updatedOutputs["PolicyRef"], policyID)
	}

	store, err = vpClient.GetPolicyStore(ctx, &sdk.GetPolicyStoreInput{
		PolicyStoreId: aws.String(storeID),
	})
	if err != nil || store.ValidationSettings.Mode != types.ValidationModeStrict {
		t.Fatalf("expected updated validation mode STRICT: %+v, err: %v", store, err)
	}

	policy, err = vpClient.GetPolicy(ctx, &sdk.GetPolicyInput{
		PolicyStoreId: aws.String(storeID),
		PolicyId:      aws.String(policyID),
	})
	if err != nil || policy.Effect != types.PolicyEffectForbid {
		t.Fatalf("expected updated policy effect Forbid: %+v, err: %v", policy, err)
	}

	// 3. Delete Stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String("vp-stack"),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify resources removed
	_, err = vpClient.GetPolicy(ctx, &sdk.GetPolicyInput{
		PolicyStoreId: aws.String(storeID),
		PolicyId:      aws.String(policyID),
	})
	if err == nil {
		t.Fatal("expected policy to be deleted")
	}

	_, err = vpClient.GetPolicyStore(ctx, &sdk.GetPolicyStoreInput{
		PolicyStoreId: aws.String(storeID),
	})
	if err == nil {
		t.Fatal("expected policy store to be deleted")
	}
}

func TestCloudFormationVerifiedPermissionsTemplateLinkedFails(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)

	brokenTemplate, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"MyStore": map[string]any{
				"Type": "AWS::VerifiedPermissions::PolicyStore",
				"Properties": map[string]any{
					"ValidationSettings": map[string]any{"Mode": "OFF"},
				},
			},
			"BrokenPolicy": map[string]any{
				"Type": "AWS::VerifiedPermissions::Policy",
				"Properties": map[string]any{
					"PolicyStoreId": map[string]any{"Ref": "MyStore"},
					"Definition": map[string]any{
						"TemplateLinked": map[string]any{
							"PolicyTemplateId": "pt-123",
						},
					},
				},
			},
		},
	})

	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    aws.String("broken-vp-stack"),
			TemplateBody: aws.String(string(brokenTemplate)),
		},
	)
	if err != nil {
		t.Fatalf("CreateStack unexpected error: %v", err)
	}

	wantStatus(ctx, t, cf, "broken-vp-stack", cfntypes.StackStatusRollbackComplete)

	events, err := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{
		StackName: aws.String("broken-vp-stack"),
	})
	if err != nil {
		t.Fatalf("DescribeStackEvents failed: %v", err)
	}

	var foundFailedPolicy bool
	for _, e := range events.StackEvents {
		if aws.ToString(e.LogicalResourceId) == "BrokenPolicy" &&
			e.ResourceStatus == cfntypes.ResourceStatusCreateFailed {
			foundFailedPolicy = true
			if !strings.Contains(aws.ToString(e.ResourceStatusReason), "template-linked") {
				t.Fatalf("expected template-linked failure reason, got %q", aws.ToString(e.ResourceStatusReason))
			}
		}
	}

	if !foundFailedPolicy {
		t.Fatalf("expected BrokenPolicy CreateFailed event in stack events: %+v", events.StackEvents)
	}
}

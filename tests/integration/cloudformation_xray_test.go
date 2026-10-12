package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/xray"
)

func xrayTemplate(filterExpr string, fixedRate float64) string {
	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"MyGroup": map[string]any{
				"Type": "AWS::XRay::Group",
				"Properties": map[string]any{
					"GroupName":        "cfn-test-group",
					"FilterExpression": filterExpr,
					"InsightsConfiguration": map[string]any{
						"InsightsEnabled":      true,
						"NotificationsEnabled": false,
					},
					"Tags": []any{
						map[string]any{"Key": "Env", "Value": "Dev"},
					},
				},
			},
			"MySamplingRule": map[string]any{
				"Type": "AWS::XRay::SamplingRule",
				"Properties": map[string]any{
					"SamplingRule": map[string]any{
						"RuleName":      "cfn-test-rule",
						"Priority":      100,
						"FixedRate":     fixedRate,
						"ReservoirSize": 5,
						"ServiceName":   "cfn-service",
						"ServiceType":   "*",
						"Host":          "*",
						"HTTPMethod":    "POST",
						"URLPath":       "/cfn",
						"ResourceARN":   "*",
						"Version":       1,
					},
					"Tags": []any{
						map[string]any{"Key": "CreatedBy", "Value": "CloudFormation"},
					},
				},
			},
		},
		"Outputs": map[string]any{
			"GroupRef": map[string]any{"Value": map[string]any{"Ref": "MyGroup"}},
			"GroupArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyGroup", "GroupARN"}}},
			"RuleRef":  map[string]any{"Value": map[string]any{"Ref": "MySamplingRule"}},
			"RuleArn":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MySamplingRule", "RuleARN"}}},
		},
	})
	return string(raw)
}

func TestCloudFormationXRayLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	xrayC := xrayClient(s, "test", "us-east-1")

	// 1. Create Stack
	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    aws.String("xray-stack"),
			TemplateBody: aws.String(xrayTemplate("service(\"a\")", 0.1)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, "xray-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	if outputs["GroupRef"] == "" || outputs["GroupRef"] != outputs["GroupArn"] ||
		!strings.HasPrefix(outputs["GroupRef"], "arn:aws:xray:us-east-1:") {
		t.Fatalf("unexpected group outputs: %+v", outputs)
	}

	if outputs["RuleRef"] == "" || outputs["RuleRef"] != outputs["RuleArn"] ||
		!strings.HasPrefix(outputs["RuleRef"], "arn:aws:xray:us-east-1:") {
		t.Fatalf("unexpected rule outputs: %+v", outputs)
	}

	// Verify resources in X-Ray service
	grp, err := xrayC.GetGroup(ctx, &sdk.GetGroupInput{
		GroupName: aws.String("cfn-test-group"),
	})
	if err != nil || aws.ToString(grp.Group.FilterExpression) != "service(\"a\")" {
		t.Fatal(grp, err)
	}

	rules, err := xrayC.GetSamplingRules(ctx, &sdk.GetSamplingRulesInput{})
	if err != nil {
		t.Fatal(err)
	}
	foundRule := false
	for _, rec := range rules.SamplingRuleRecords {
		if aws.ToString(rec.SamplingRule.RuleName) == "cfn-test-rule" {
			foundRule = true
			if rec.SamplingRule.FixedRate != 0.1 {
				t.Fatalf("expected rate 0.1, got %f", rec.SamplingRule.FixedRate)
			}
		}
	}
	if !foundRule {
		t.Fatal("cfn-test-rule not found")
	}

	// 2. Update Stack
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName:    aws.String("xray-stack"),
			TemplateBody: aws.String(xrayTemplate("service(\"b\")", 0.4)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, "xray-stack", cfntypes.StackStatusUpdateComplete)
	updatedOutputs := outputsOf(&st)
	if updatedOutputs["GroupRef"] != outputs["GroupRef"] {
		t.Fatalf("Group was replaced unexpectedly: %s vs %s", updatedOutputs["GroupRef"], outputs["GroupRef"])
	}
	if updatedOutputs["RuleRef"] != outputs["RuleRef"] {
		t.Fatalf("Rule was replaced unexpectedly: %s vs %s", updatedOutputs["RuleRef"], outputs["RuleRef"])
	}

	// Verify updated properties
	grp, err = xrayC.GetGroup(ctx, &sdk.GetGroupInput{
		GroupName: aws.String("cfn-test-group"),
	})
	if err != nil || aws.ToString(grp.Group.FilterExpression) != "service(\"b\")" {
		t.Fatal(grp, err)
	}

	rules, err = xrayC.GetSamplingRules(ctx, &sdk.GetSamplingRulesInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range rules.SamplingRuleRecords {
		if aws.ToString(rec.SamplingRule.RuleName) == "cfn-test-rule" {
			if rec.SamplingRule.FixedRate != 0.4 {
				t.Fatalf("expected updated rate 0.4, got %f", rec.SamplingRule.FixedRate)
			}
		}
	}

	// 3. Delete Stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String("xray-stack"),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify deletion
	_, err = xrayC.GetGroup(ctx, &sdk.GetGroupInput{
		GroupName: aws.String("cfn-test-group"),
	})
	if err == nil {
		t.Fatal("expected GetGroup to fail after stack delete")
	}

	rules, err = xrayC.GetSamplingRules(ctx, &sdk.GetSamplingRulesInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range rules.SamplingRuleRecords {
		if aws.ToString(rec.SamplingRule.RuleName) == "cfn-test-rule" {
			t.Fatal("expected cfn-test-rule to be deleted")
		}
	}
}

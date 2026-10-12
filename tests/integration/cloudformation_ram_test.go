package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/ram"
	"github.com/aws/aws-sdk-go-v2/service/ram/types"
)

func ramShareTemplate(
	name string,
	principals, resources []string,
	allowExternal bool,
	tags map[string]string,
	extraProps map[string]any,
) string {
	props := map[string]any{
		"Name":                    name,
		"AllowExternalPrincipals": allowExternal,
		"Principals":              principals,
		"ResourceArns":            resources,
	}

	if len(tags) > 0 {
		var tagList []map[string]any
		for k, v := range tags {
			tagList = append(tagList, map[string]any{"Key": k, "Value": v})
		}
		props["Tags"] = tagList
	}

	for k, v := range extraProps {
		props[k] = v
	}

	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"MyShare": map[string]any{
				"Type":       "AWS::RAM::ResourceShare",
				"Properties": props,
			},
		},
		"Outputs": map[string]any{
			"ShareRef": map[string]any{"Value": map[string]any{"Ref": "MyShare"}},
			"ShareArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyShare", "Arn"}}},
		},
	})
	return string(raw)
}

func TestCloudFormationRAMLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	ramC := ramClient(s, "test", "us-east-1")

	p1 := "123456789012"
	p2 := "210987654321"
	r1 := "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-11111111"
	r2 := "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-22222222"

	// 1. Create Stack
	tmpl1 := ramShareTemplate(
		"cfn-ram-share",
		[]string{p1},
		[]string{r1},
		true,
		map[string]string{"Env": "Stage"},
		nil,
	)

	_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    aws.String("ram-stack"),
		TemplateBody: aws.String(tmpl1),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	st := wantStatus(ctx, t, cf, "ram-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	shareArn := outputs["ShareArn"]
	if shareArn == "" || shareArn != outputs["ShareRef"] ||
		!strings.HasPrefix(shareArn, "arn:aws:ram:us-east-1:") {
		t.Fatalf("unexpected outputs: %+v", outputs)
	}

	// Verify share exists in RAM
	shares, err := ramC.GetResourceShares(ctx, &sdk.GetResourceSharesInput{
		ResourceOwner:     types.ResourceOwnerSelf,
		ResourceShareArns: []string{shareArn},
	})
	if err != nil || len(shares.ResourceShares) != 1 {
		t.Fatalf("share not found in RAM: %v, %v", shares, err)
	}
	if aws.ToString(shares.ResourceShares[0].Name) != "cfn-ram-share" {
		t.Fatalf("unexpected share name: %s", aws.ToString(shares.ResourceShares[0].Name))
	}

	// Verify associations
	resAssocs, err := ramC.GetResourceShareAssociations(ctx, &sdk.GetResourceShareAssociationsInput{
		AssociationType:   types.ResourceShareAssociationTypeResource,
		ResourceShareArns: []string{shareArn},
		AssociationStatus: types.ResourceShareAssociationStatusAssociated,
	})
	if err != nil || len(resAssocs.ResourceShareAssociations) != 1 {
		t.Fatalf("unexpected resource associations: %v, %v", resAssocs, err)
	}

	// 2. Update Stack - reconcile associations (swap r1->r2 and p1->p2) and rename
	tmpl2 := ramShareTemplate(
		"cfn-ram-share-updated",
		[]string{p2},
		[]string{r2},
		true,
		map[string]string{"Env": "Prod"},
		nil,
	)

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    aws.String("ram-stack"),
		TemplateBody: aws.String(tmpl2),
	})
	if err != nil {
		t.Fatalf("UpdateStack failed: %v", err)
	}

	st = wantStatus(ctx, t, cf, "ram-stack", cfntypes.StackStatusUpdateComplete)
	updatedOutputs := outputsOf(&st)
	if updatedOutputs["ShareArn"] != shareArn {
		t.Fatalf("Share was unexpectedly recreated: %s vs %s", updatedOutputs["ShareArn"], shareArn)
	}

	// Verify updated share name
	shares, err = ramC.GetResourceShares(ctx, &sdk.GetResourceSharesInput{
		ResourceOwner:     types.ResourceOwnerSelf,
		ResourceShareArns: []string{shareArn},
	})
	if err != nil || len(shares.ResourceShares) != 1 {
		t.Fatalf("share not found in RAM: %v, %v", shares, err)
	}
	if aws.ToString(shares.ResourceShares[0].Name) != "cfn-ram-share-updated" {
		t.Fatalf("expected updated name cfn-ram-share-updated, got %s", aws.ToString(shares.ResourceShares[0].Name))
	}

	// Verify associations were reconciled: r2 is ASSOCIATED, r1 is DISASSOCIATED
	resAssocs2, err := ramC.GetResourceShareAssociations(ctx, &sdk.GetResourceShareAssociationsInput{
		AssociationType:   types.ResourceShareAssociationTypeResource,
		ResourceShareArns: []string{shareArn},
		AssociationStatus: types.ResourceShareAssociationStatusAssociated,
	})
	if err != nil || len(resAssocs2.ResourceShareAssociations) != 1 ||
		aws.ToString(resAssocs2.ResourceShareAssociations[0].AssociatedEntity) != r2 {
		t.Fatalf("expected resource association r2, got: %v", resAssocs2)
	}

	// 3. Update Failure - specify unsupported property PermissionArns
	badTmpl := ramShareTemplate(
		"cfn-ram-share-bad",
		[]string{p2},
		[]string{r2},
		true,
		nil,
		map[string]any{
			"PermissionArns": []string{"arn:aws:ram::aws:permission/AWSRAMDefaultPermission"},
		},
	)

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    aws.String("ram-stack"),
		TemplateBody: aws.String(badTmpl),
	})
	if err != nil {
		t.Fatalf("UpdateStack with bad template failed immediately: %v", err)
	}

	wantStatus(ctx, t, cf, "ram-stack", cfntypes.StackStatusUpdateRollbackComplete)

	// 4. Delete Stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String("ram-stack"),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify share is marked DELETED
	delShares, err := ramC.GetResourceShares(ctx, &sdk.GetResourceSharesInput{
		ResourceOwner:       types.ResourceOwnerSelf,
		ResourceShareStatus: types.ResourceShareStatusDeleted,
		ResourceShareArns:   []string{shareArn},
	})
	if err != nil || len(delShares.ResourceShares) != 1 {
		t.Fatalf("expected deleted share in RAM, got: %v, %v", delShares, err)
	}
}

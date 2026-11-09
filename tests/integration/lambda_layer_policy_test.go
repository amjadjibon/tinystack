package integration_test

import (
	"context"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

func TestLambdaLayerPermissions(t *testing.T) {
	s := server(t)
	owner := lambdaClient(s, "test")
	other := lambdaClient(s, "team-b")
	ctx := context.Background()
	layer := publishLayer(t, owner, "shared-policy", "x = 1\n")
	read := func() {
		t.Helper()
		_, err := other.GetLayerVersion(
			ctx,
			&sdk.GetLayerVersionInput{LayerName: layer.LayerArn, VersionNumber: new(layer.Version)},
		)
		mustAuthorize(t, err)
		_, err = other.GetLayerVersionByArn(ctx, &sdk.GetLayerVersionByArnInput{Arn: layer.LayerVersionArn})
		mustAuthorize(t, err)
	}

	_, err := other.GetLayerVersionByArn(ctx, &sdk.GetLayerVersionByArnInput{Arn: layer.LayerVersionArn})
	wantCode(t, err, "AccessDeniedException")
	add := &sdk.AddLayerVersionPermissionInput{
		LayerName:     new("shared-policy"),
		VersionNumber: new(layer.Version),
		StatementId:   new("share"),
		Action:        new("lambda:GetLayerVersion"),
		Principal:     new("111111111111"),
	}

	permission, err := owner.AddLayerVersionPermission(ctx, add)
	mustAuthorize(t, err)
	read()
	policy, err := owner.GetLayerVersionPolicy(
		ctx,
		&sdk.GetLayerVersionPolicyInput{LayerName: new("shared-policy"), VersionNumber: new(layer.Version)},
	)
	mustAuthorize(t, err)
	if *policy.RevisionId != *permission.RevisionId {
		t.Fatal(policy)
	}

	_, err = owner.AddLayerVersionPermission(ctx, add)
	wantCode(t, err, "ResourceConflictException")
	add.StatementId = new("stale")
	add.RevisionId = new("wrong")
	_, err = owner.AddLayerVersionPermission(ctx, add)
	wantCode(t, err, "PreconditionFailedException")
	_, err = other.CreateFunction(
		ctx,
		&sdk.CreateFunctionInput{
			FunctionName: new("layer-user"),
			Runtime:      types.RuntimePython312,
			Role:         new("arn:aws:iam::111111111111:role/lambda"),
			Handler:      new("app.echo"),
			Code:         &types.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e,c): return e\n")},
			Layers:       []string{*layer.LayerVersionArn},
		},
	)
	mustAuthorize(t, err)
	_, err = owner.RemoveLayerVersionPermission(
		ctx,
		&sdk.RemoveLayerVersionPermissionInput{
			LayerName:     new("shared-policy"),
			VersionNumber: new(layer.Version),
			StatementId:   new("share"),
			RevisionId:    new("wrong"),
		},
	)
	wantCode(t, err, "PreconditionFailedException")
	_, err = owner.RemoveLayerVersionPermission(
		ctx,
		&sdk.RemoveLayerVersionPermissionInput{
			LayerName:     new("shared-policy"),
			VersionNumber: new(layer.Version),
			StatementId:   new("share"),
			RevisionId:    permission.RevisionId,
		},
	)
	mustAuthorize(t, err)
	_, err = other.GetLayerVersionByArn(ctx, &sdk.GetLayerVersionByArnInput{Arn: layer.LayerVersionArn})
	wantCode(t, err, "AccessDeniedException")
	_, err = other.UpdateFunctionConfiguration(
		ctx,
		&sdk.UpdateFunctionConfigurationInput{
			FunctionName: new("layer-user"),
			Layers:       []string{*layer.LayerVersionArn},
		},
	)
	wantCode(t, err, "AccessDeniedException")
	_, err = owner.GetLayerVersionPolicy(
		ctx,
		&sdk.GetLayerVersionPolicyInput{LayerName: new("shared-policy"), VersionNumber: new(layer.Version)},
	)
	wantCode(t, err, "ResourceNotFoundException")
	add.StatementId = new("organization")
	add.RevisionId = nil
	add.Principal = new("*")
	add.OrganizationId = new("o-1234567890")
	_, err = owner.AddLayerVersionPermission(ctx, add)
	mustAuthorize(t, err)
	_, err = other.GetLayerVersionByArn(ctx, &sdk.GetLayerVersionByArnInput{Arn: layer.LayerVersionArn})
	wantCode(t, err, "AccessDeniedException")
	add.OrganizationId = nil
	add.StatementId = new("public")
	add.RevisionId = nil
	add.Principal = new("*")
	_, err = owner.AddLayerVersionPermission(ctx, add)
	mustAuthorize(t, err)
	read()
	add.StatementId = new("invalid")
	add.Action = new("lambda:InvokeFunction")
	_, err = owner.AddLayerVersionPermission(ctx, add)
	wantCode(t, err, "InvalidParameterValueException")
	add.Action = new("lambda:GetLayerVersion")
	add.Principal = new("lambda.amazonaws.com")
	_, err = owner.AddLayerVersionPermission(ctx, add)
	wantCode(t, err, "InvalidParameterValueException")
}

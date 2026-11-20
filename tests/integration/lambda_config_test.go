package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
)

func TestLambdaRecursionAndRuntimeManagement(t *testing.T) {
	s := serverWith(t, local.New(local.Options{}))
	c := lambdaClient(s, "test")
	ctx := t.Context()
	full := createConfigurationFunction(t, ctx, c, "with-settings", lambdatypes.RuntimePython312, nil, nil, true)
	name := new("with-settings")

	recursion, err := c.GetFunctionRecursionConfig(ctx, &sdklambda.GetFunctionRecursionConfigInput{FunctionName: name})
	if err != nil || recursion.RecursiveLoop != lambdatypes.RecursiveLoopTerminate {
		t.Fatalf("default recursion config: %v %v", recursion, err)
	}

	setRecursion, err := c.PutFunctionRecursionConfig(ctx, &sdklambda.PutFunctionRecursionConfigInput{
		FunctionName: name, RecursiveLoop: lambdatypes.RecursiveLoopAllow,
	})
	if err != nil || setRecursion.RecursiveLoop != lambdatypes.RecursiveLoopAllow {
		t.Fatalf("put recursion config: %v %v", setRecursion, err)
	}

	recursion, err = c.GetFunctionRecursionConfig(ctx, &sdklambda.GetFunctionRecursionConfigInput{FunctionName: name})
	if err != nil || recursion.RecursiveLoop != lambdatypes.RecursiveLoopAllow {
		t.Fatalf("stored recursion config: %v %v", recursion, err)
	}

	_, err = c.PutFunctionRecursionConfig(ctx, &sdklambda.PutFunctionRecursionConfigInput{
		FunctionName: name, RecursiveLoop: "Loop",
	})
	code(t, err, "InvalidParameterValueException")

	runtimeMode, err := c.GetRuntimeManagementConfig(ctx, &sdklambda.GetRuntimeManagementConfigInput{
		FunctionName: name,
	})
	if err != nil || runtimeMode.UpdateRuntimeOn != lambdatypes.UpdateRuntimeOnAuto ||
		runtimeMode.RuntimeVersionArn != nil {
		t.Fatalf("default runtime config: %v %v", runtimeMode, err)
	}

	arn := new("arn:aws:lambda:us-east-1::runtime:abc123")
	stored, err := c.PutRuntimeManagementConfig(ctx, &sdklambda.PutRuntimeManagementConfigInput{
		FunctionName: name, Qualifier: new("1"), UpdateRuntimeOn: lambdatypes.UpdateRuntimeOnManual,
		RuntimeVersionArn: arn,
	})
	if err != nil || aws.ToString(stored.RuntimeVersionArn) != *arn ||
		aws.ToString(stored.FunctionArn) != *full.FunctionArn {
		t.Fatalf("put manual runtime config: %v %v", stored, err)
	}

	_, err = c.PutRuntimeManagementConfig(ctx, &sdklambda.PutRuntimeManagementConfigInput{
		FunctionName: name, UpdateRuntimeOn: lambdatypes.UpdateRuntimeOnManual,
	})
	code(t, err, "InvalidParameterValueException")

	_, err = c.PutRuntimeManagementConfig(ctx, &sdklambda.PutRuntimeManagementConfigInput{
		FunctionName: name, UpdateRuntimeOn: lambdatypes.UpdateRuntimeOnAuto, RuntimeVersionArn: arn,
	})
	code(t, err, "InvalidParameterValueException")

	_, err = c.PutRuntimeManagementConfig(ctx, &sdklambda.PutRuntimeManagementConfigInput{
		FunctionName: name, UpdateRuntimeOn: lambdatypes.UpdateRuntimeOnFunctionUpdate,
	})
	if err != nil {
		t.Fatal(err)
	}

	version, err := c.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: name, Qualifier: new("1"),
	})
	if err != nil || version.RuntimeVersionConfig == nil ||
		aws.ToString(version.RuntimeVersionConfig.RuntimeVersionArn) != *arn {
		t.Fatalf("runtime version config: %v %v", version, err)
	}

	for _, tc := range []struct {
		qualifier *string
		mode      lambdatypes.UpdateRuntimeOn
		arn       string
	}{
		{nil, lambdatypes.UpdateRuntimeOnFunctionUpdate, ""},
		{new("1"), lambdatypes.UpdateRuntimeOnManual, *arn},
	} {
		out, readErr := c.GetRuntimeManagementConfig(ctx, &sdklambda.GetRuntimeManagementConfigInput{
			FunctionName: name, Qualifier: tc.qualifier,
		})
		if readErr != nil || out.UpdateRuntimeOn != tc.mode || aws.ToString(out.RuntimeVersionArn) != tc.arn {
			t.Fatalf("get runtime config for %q: %v %v", aws.ToString(tc.qualifier), out, readErr)
		}
	}

	other := lambdaClient(s, "team-b")
	_, err = other.GetFunctionRecursionConfig(ctx, &sdklambda.GetFunctionRecursionConfigInput{FunctionName: name})
	code(t, err, "ResourceNotFoundException")
}

func TestLambdaCodeSigningConfig(t *testing.T) {
	s := serverWith(t, local.New(local.Options{}))
	c := lambdaClient(s, "test")
	ctx := t.Context()
	allowed := &lambdatypes.AllowedPublishers{
		SigningProfileVersionArns: []string{
			"arn:aws:signer:us-east-1:000000000000:/signing-profiles/publisher/abcdefghij",
		},
	}
	_, err := c.CreateCodeSigningConfig(ctx, &sdklambda.CreateCodeSigningConfigInput{
		AllowedPublishers: &lambdatypes.AllowedPublishers{SigningProfileVersionArns: []string{"invalid"}},
	})
	code(t, err, "InvalidParameterValueException")

	created, err := c.CreateCodeSigningConfig(ctx, &sdklambda.CreateCodeSigningConfigInput{
		AllowedPublishers: allowed, Description: new("test"), Tags: map[string]string{"team": "lambda"},
	})
	if err != nil || created.CodeSigningConfig == nil ||
		created.CodeSigningConfig.CodeSigningPolicies.UntrustedArtifactOnDeployment != lambdatypes.CodeSigningPolicyWarn {
		t.Fatalf("create code signing config: %v %v", created, err)
	}

	arn := created.CodeSigningConfig.CodeSigningConfigArn
	_, err = c.CreateCodeSigningConfig(ctx, &sdklambda.CreateCodeSigningConfigInput{AllowedPublishers: allowed})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListCodeSigningConfigs(ctx, &sdklambda.ListCodeSigningConfigsInput{MaxItems: new(int32(1))})
	if err != nil || len(page.CodeSigningConfigs) != 1 || page.NextMarker == nil {
		t.Fatalf("first page: %v %v", page, err)
	}

	last, err := c.ListCodeSigningConfigs(ctx, &sdklambda.ListCodeSigningConfigsInput{Marker: page.NextMarker})
	if err != nil || len(last.CodeSigningConfigs) != 1 || last.NextMarker != nil ||
		*page.CodeSigningConfigs[0].CodeSigningConfigArn == *last.CodeSigningConfigs[0].CodeSigningConfigArn {
		t.Fatalf("last page: %v %v", last, err)
	}

	tags, err := c.ListTags(ctx, &sdklambda.ListTagsInput{Resource: arn})
	if err != nil || tags.Tags["team"] != "lambda" {
		t.Fatalf("signing config tags: %v %v", tags, err)
	}

	a := createConfigurationFunction(t, ctx, c, "signed-a", lambdatypes.RuntimePython312, arn, nil, false)
	b := createConfigurationFunction(t, ctx, c, "signed-b", lambdatypes.RuntimePython312, nil, nil, false)
	put, err := c.PutFunctionCodeSigningConfig(ctx, &sdklambda.PutFunctionCodeSigningConfigInput{
		FunctionName: b.FunctionName, CodeSigningConfigArn: arn,
	})
	if err != nil || aws.ToString(put.CodeSigningConfigArn) != *arn {
		t.Fatalf("attach config: %v %v", put, err)
	}

	association, err := c.GetFunctionCodeSigningConfig(ctx, &sdklambda.GetFunctionCodeSigningConfigInput{
		FunctionName: a.FunctionName,
	})
	if err != nil || aws.ToString(association.CodeSigningConfigArn) != *arn {
		t.Fatalf("get association: %v %v", association, err)
	}

	users, err := c.ListFunctionsByCodeSigningConfig(ctx, &sdklambda.ListFunctionsByCodeSigningConfigInput{
		CodeSigningConfigArn: arn, MaxItems: new(int32(1)),
	})
	if err != nil || len(users.FunctionArns) != 1 || users.FunctionArns[0] != *a.FunctionArn ||
		users.NextMarker == nil {
		t.Fatalf("first page of associations: %v %v", users, err)
	}

	users, err = c.ListFunctionsByCodeSigningConfig(ctx, &sdklambda.ListFunctionsByCodeSigningConfigInput{
		CodeSigningConfigArn: arn, Marker: users.NextMarker,
	})
	if err != nil || len(users.FunctionArns) != 1 || users.FunctionArns[0] != *b.FunctionArn ||
		users.NextMarker != nil {
		t.Fatalf("second page of associations: %v %v", users, err)
	}

	_, err = c.DeleteCodeSigningConfig(ctx, &sdklambda.DeleteCodeSigningConfigInput{CodeSigningConfigArn: arn})
	code(t, err, "ResourceConflictException")
	_, err = c.UpdateCodeSigningConfig(ctx, &sdklambda.UpdateCodeSigningConfigInput{
		CodeSigningConfigArn: arn,
		CodeSigningPolicies: &lambdatypes.CodeSigningPolicies{
			UntrustedArtifactOnDeployment: lambdatypes.CodeSigningPolicyEnforce,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stored, err := c.GetCodeSigningConfig(ctx, &sdklambda.GetCodeSigningConfigInput{CodeSigningConfigArn: arn})
	if err != nil || stored.CodeSigningConfig.CodeSigningPolicies.UntrustedArtifactOnDeployment !=
		lambdatypes.CodeSigningPolicyEnforce || aws.ToString(stored.CodeSigningConfig.Description) != "test" ||
		stored.CodeSigningConfig.LastModified == nil {
		t.Fatalf("get code signing config: %v %v", stored, err)
	}

	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: a.FunctionName, ZipFile: zipOf(t, "app.py", "def handler(e,c): return 'new'"),
	})
	code(t, err, "CodeVerificationFailedException")
	layer, err := c.PublishLayerVersion(ctx, &sdklambda.PublishLayerVersionInput{
		LayerName: new("unsigned-layer"),
		Content:   &lambdatypes.LayerVersionContentInput{ZipFile: zipOf(t, "python/helper.py", "")},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: a.FunctionName, Layers: []string{*layer.LayerVersionArn},
	})
	code(t, err, "CodeVerificationFailedException")
	after, err := c.GetFunctionConfiguration(
		ctx,
		&sdklambda.GetFunctionConfigurationInput{FunctionName: a.FunctionName},
	)
	if err != nil || aws.ToString(after.RevisionId) != aws.ToString(a.RevisionId) || len(after.Layers) != 0 {
		t.Fatalf("failed deployments changed the function: %v %v", after, err)
	}

	_, err = c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: new("unsigned"), Runtime: lambdatypes.RuntimePython312, Handler: new("app.handler"),
		Role: new("arn:aws:iam::000000000000:role/lambda"), CodeSigningConfigArn: arn,
		Code: &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def handler(e,c): return e")},
	})
	code(t, err, "CodeVerificationFailedException")
	_, err = c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: new("unsigned")})
	code(t, err, "ResourceNotFoundException")

	_, err = c.DeleteFunctionCodeSigningConfig(ctx, &sdklambda.DeleteFunctionCodeSigningConfigInput{
		FunctionName: a.FunctionName,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: a.FunctionName, ZipFile: zipOf(t, "app.py", "def handler(e,c): return 'new'"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: b.FunctionName})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCodeSigningConfig(ctx, &sdklambda.DeleteCodeSigningConfigInput{CodeSigningConfigArn: arn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetCodeSigningConfig(ctx, &sdklambda.GetCodeSigningConfigInput{CodeSigningConfigArn: arn})
	code(t, err, "ResourceNotFoundException")
	_, err = c.PutFunctionCodeSigningConfig(ctx, &sdklambda.PutFunctionCodeSigningConfigInput{
		FunctionName: a.FunctionName, CodeSigningConfigArn: arn,
	})
	code(t, err, "CodeSigningConfigNotFoundException")
}

func TestLambdaSnapStartConfiguration(t *testing.T) {
	c := lambdaClient(serverWith(t, local.New(local.Options{})), "test")
	ctx := t.Context()
	on := &lambdatypes.SnapStart{ApplyOn: lambdatypes.SnapStartApplyOnPublishedVersions}
	created := createConfigurationFunction(t, ctx, c, "snap-java", lambdatypes.RuntimeJava21, nil, on, false)
	if created.SnapStart == nil || created.SnapStart.ApplyOn != lambdatypes.SnapStartApplyOnPublishedVersions ||
		created.SnapStart.OptimizationStatus != lambdatypes.SnapStartOptimizationStatusOff {
		t.Fatalf("unpublished SnapStart config: %+v", created.SnapStart)
	}

	v, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: created.FunctionName})
	if err != nil || v.SnapStart == nil || v.SnapStart.ApplyOn != lambdatypes.SnapStartApplyOnPublishedVersions ||
		v.SnapStart.OptimizationStatus != lambdatypes.SnapStartOptimizationStatusOn {
		t.Fatalf("published SnapStart config: %+v %v", v, err)
	}

	_, err = c.PutProvisionedConcurrencyConfig(ctx, &sdklambda.PutProvisionedConcurrencyConfigInput{
		FunctionName: created.FunctionName, Qualifier: v.Version, ProvisionedConcurrentExecutions: new(int32(1)),
	})
	code(t, err, "InvalidParameterValueException")
	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: created.FunctionName, Runtime: lambdatypes.RuntimeNodejs22x,
	})
	code(t, err, "InvalidParameterValueException")
	latest, err := c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: created.FunctionName, Runtime: lambdatypes.RuntimeNodejs22x,
		SnapStart: &lambdatypes.SnapStart{ApplyOn: lambdatypes.SnapStartApplyOnNone},
	})
	if err != nil || latest.SnapStart == nil ||
		latest.SnapStart.OptimizationStatus != lambdatypes.SnapStartOptimizationStatusOff {
		t.Fatalf("disable SnapStart: %+v %v", latest, err)
	}

	old, err := c.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: created.FunctionName, Qualifier: v.Version,
	})
	if err != nil || old.SnapStart == nil ||
		old.SnapStart.OptimizationStatus != lambdatypes.SnapStartOptimizationStatusOn {
		t.Fatalf("version SnapStart config: %+v %v", old, err)
	}

	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: created.FunctionName, SnapStart: on,
	})
	code(t, err, "InvalidParameterValueException")
}

func createConfigurationFunction(
	t *testing.T, ctx context.Context, c *sdklambda.Client, name string, runtime lambdatypes.Runtime,
	signing *string, snapStart *lambdatypes.SnapStart, publish bool,
) *sdklambda.CreateFunctionOutput {
	t.Helper()
	fn, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName:         new(name),
		Runtime:              runtime,
		Handler:              new("app.handler"),
		Role:                 new("arn:aws:iam::000000000000:role/lambda"),
		CodeSigningConfigArn: signing,
		SnapStart:            snapStart,
		Code: &lambdatypes.FunctionCode{
			ZipFile: zipOf(t, "app.py", "def handler(e,c): return e"),
		},
		Publish: publish,
	})
	if err != nil {
		t.Fatal(err)
	}

	return fn
}

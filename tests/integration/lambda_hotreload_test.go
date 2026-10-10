package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

func TestLambdaHotReload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	state := t.TempDir()
	s, stop := persistent(t, state, &fakeRuntime{})
	c := lambdaClient(s, "test")
	input := &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("live"), Runtime: types.RuntimePython312, Handler: aws.String("app.echo"),
		Role: aws.String("arn:aws:iam::000000000000:role/lambda"),
		Code: &types.FunctionCode{S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir)},
	}
	created, err := c.CreateFunction(ctx, input)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	if created.CodeSize != 0 || aws.ToString(created.CodeSha256) != sha([]byte(dir)) {
		stop()
		t.Fatalf("unexpected code metadata: %+v", created)
	}

	version, err := c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: input.FunctionName})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, state, &fakeRuntime{})
	defer stop()
	c = lambdaClient(s, "test")
	for _, qualifier := range []string{"$LATEST", aws.ToString(version.Version)} {
		got, getErr := c.GetFunction(ctx, &sdklambda.GetFunctionInput{
			FunctionName: input.FunctionName, Qualifier: aws.String(qualifier),
		})
		if getErr != nil || aws.ToString(got.Code.RepositoryType) != "hot-reload" || got.Code.Location != nil ||
			aws.ToString(got.Configuration.CodeSha256) != sha([]byte(dir)) {
			t.Fatalf("get after restart: %+v, %v", got, getErr)
		}
	}

	_, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: input.FunctionName, Payload: []byte(`{}`)})
	if code, _ := lambdaCode(err); code != "InvalidParameterValueException" ||
		!strings.Contains(err.Error(), "in-process runtime does not support host directories") {
		t.Fatalf("in-process invoke: %v", err)
	}

	zip := zipOf(t, "app.py", "def echo(e,c): return e\n")
	updated, err := c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: input.FunctionName, ZipFile: zip,
	})
	if err != nil || aws.ToString(updated.CodeSha256) != sha(zip) {
		t.Fatalf("update to zip: %+v, %v", updated, err)
	}

	got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: input.FunctionName})
	if err != nil || aws.ToString(got.Code.RepositoryType) != "S3" {
		t.Fatalf("zip location: %+v, %v", got, err)
	}

	if _, err = c.Invoke(
		ctx,
		&sdklambda.InvokeInput{FunctionName: input.FunctionName, Payload: []byte(`{}`)},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: input.FunctionName, S3Bucket: aws.String("hot-reload"), S3Key: aws.String("relative"),
	})
	if code, _ := lambdaCode(err); code != "InvalidParameterValueException" {
		t.Fatalf("invalid directory update: %v", err)
	}

	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: input.FunctionName, S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir),
		DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err = c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: input.FunctionName})
	if err != nil || aws.ToString(got.Configuration.CodeSha256) != sha(zip) {
		t.Fatalf("dry run changed code: %+v, %v", got, err)
	}

	got, err = c.GetFunction(ctx, &sdklambda.GetFunctionInput{
		FunctionName: input.FunctionName, Qualifier: version.Version,
	})
	if err != nil || aws.ToString(got.Code.RepositoryType) != "hot-reload" {
		t.Fatalf("published directory reference changed: %+v, %v", got, err)
	}

	updated, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: input.FunctionName, S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir),
	})
	if err != nil || updated.CodeSize != 0 || aws.ToString(updated.CodeSha256) != sha([]byte(dir)) {
		t.Fatalf("update to directory: %+v, %v", updated, err)
	}

	file := filepath.Join(dir, "app.py")
	if err = os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, source := range map[string]*types.FunctionCode{
		"relative": {S3Bucket: aws.String("hot-reload"), S3Key: aws.String("relative")},
		"missing":  {S3Bucket: aws.String("hot-reload"), S3Key: aws.String(filepath.Join(dir, "missing"))},
		"file":     {S3Bucket: aws.String("hot-reload"), S3Key: aws.String(file)},
		"no-key":   {S3Bucket: aws.String("hot-reload")},
		"zip":      {S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir), ZipFile: zip},
		"version":  {S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir), S3ObjectVersion: aws.String("1")},
		"image":    {S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir), ImageUri: aws.String("image")},
	} {
		input.FunctionName, input.Code = aws.String(name), source
		_, err = c.CreateFunction(ctx, input)
		if code, _ := lambdaCode(err); code != "InvalidParameterValueException" {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}

	_, err = c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("image-package"), PackageType: types.PackageTypeImage, Role: input.Role,
		Code: &types.FunctionCode{S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir)},
	})
	if code, _ := lambdaCode(err); code != "InvalidParameterValueException" {
		t.Fatalf("image package hot reload: %v", err)
	}

	_, err = c.PublishLayerVersion(ctx, &sdklambda.PublishLayerVersionInput{
		LayerName: aws.String("live"),
		Content:   &types.LayerVersionContentInput{S3Bucket: aws.String("hot-reload"), S3Key: aws.String(dir)},
	})
	if code, _ := lambdaCode(err); code != "InvalidParameterValueException" ||
		!strings.Contains(err.Error(), "not supported for layers") {
		t.Fatalf("layer hot reload: %v", err)
	}
}

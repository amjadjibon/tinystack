package integration_test

import (
	"context"
	"reflect"
	"testing"

	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

func TestLambdaImageConfig(t *testing.T) {
	c := lambdaClient(server(t), "test")
	ctx := context.Background()
	config := &types.ImageConfig{
		EntryPoint: []string{"/entry", "argument"}, Command: []string{"app.handler"},
		WorkingDirectory: new("/var/task"),
	}
	created, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: new("image-settings"), Role: new("arn:aws:iam::000000000000:role/lambda"),
		PackageType: types.PackageTypeImage, Code: &types.FunctionCode{ImageUri: new("example/image:latest")},
		ImageConfig: config,
	})
	if err != nil {
		t.Fatal(err)
	}

	if created.ImageConfigResponse == nil || !reflect.DeepEqual(created.ImageConfigResponse.ImageConfig, config) {
		t.Fatalf("create: %+v", created.ImageConfigResponse)
	}

	got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: new("image-settings")})
	if err != nil || !reflect.DeepEqual(got.Configuration.ImageConfigResponse.ImageConfig, config) {
		t.Fatalf("get: %+v %v", got, err)
	}

	config = &types.ImageConfig{Command: []string{"other.handler"}, WorkingDirectory: new("/tmp")}
	updated, err := c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("image-settings"), ImageConfig: config,
	})
	if err != nil || !reflect.DeepEqual(updated.ImageConfigResponse.ImageConfig, config) {
		t.Fatalf("update: %+v %v", updated, err)
	}

	cfg, err := c.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: new("image-settings"),
	})
	if err != nil || !reflect.DeepEqual(cfg.ImageConfigResponse.ImageConfig, config) {
		t.Fatalf("configuration: %+v %v", cfg, err)
	}

	createFunction(t, c, "zip-settings", "app.echo")
	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("zip-settings"), ImageConfig: &types.ImageConfig{},
	})
	if code, _ := lambdaCode(err); code != "InvalidParameterValueException" {
		t.Fatalf("zip update: %v", err)
	}

	_, err = c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: new("zip-invalid"), Role: new("arn:aws:iam::000000000000:role/lambda"),
		Runtime: types.RuntimePython312, Handler: new("app.echo"),
		Code: &types.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e,c): return e")}, ImageConfig: config,
	})
	if code, _ := lambdaCode(err); code != "InvalidParameterValueException" {
		t.Fatalf("zip create: %v", err)
	}
}

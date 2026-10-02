package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// publishLayer publishes a layer version for python3.12 and returns its ARN.
func publishLayer(t *testing.T, c *sdklambda.Client, name, content string) *sdklambda.PublishLayerVersionOutput {
	t.Helper()
	out, err := c.PublishLayerVersion(context.Background(), &sdklambda.PublishLayerVersionInput{
		LayerName: aws.String(name), Description: aws.String("shared code"),
		Content:                 &lambdatypes.LayerVersionContentInput{ZipFile: zipOf(t, "python/lib.py", content)},
		CompatibleRuntimes:      []lambdatypes.Runtime{lambdatypes.RuntimePython312},
		CompatibleArchitectures: []lambdatypes.Architecture{lambdatypes.ArchitectureX8664},
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestLambdaLayers(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	c := lambdaClient(serverWith(t, runtime), "test")
	ctx := context.Background()
	first := publishLayer(t, c, "shared", "x = 1\n")
	second := publishLayer(t, c, "shared", "x = 2\n")
	if first.Version != 1 || second.Version != 2 ||
		aws.ToString(second.LayerVersionArn) != "arn:aws:lambda:us-east-1:000000000000:layer:shared:2" ||
		aws.ToString(second.LayerArn) != "arn:aws:lambda:us-east-1:000000000000:layer:shared" ||
		second.Content.CodeSize == 0 || aws.ToString(second.Content.CodeSha256) == "" {
		t.Fatalf("published: %+v %+v", first, second)
	}

	got, err := c.GetLayerVersionByArn(ctx, &sdklambda.GetLayerVersionByArnInput{Arn: first.LayerVersionArn})
	if err != nil || got.Version != 1 || aws.ToString(got.Description) != "shared code" {
		t.Fatalf("by ARN: %+v %v", got, err)
	}

	versions, err := c.ListLayerVersions(ctx, &sdklambda.ListLayerVersionsInput{LayerName: aws.String("shared")})
	if err != nil || len(versions.LayerVersions) != 2 || versions.LayerVersions[0].Version != 2 {
		t.Fatalf("versions: %+v %v", versions, err)
	}

	layers, err := c.ListLayers(ctx, &sdklambda.ListLayersInput{CompatibleRuntime: lambdatypes.RuntimePython312})
	if err != nil || len(layers.Layers) != 1 || layers.Layers[0].LatestMatchingVersion.Version != 2 {
		t.Fatalf("layers: %+v %v", layers, err)
	}

	layers, err = c.ListLayers(ctx, &sdklambda.ListLayersInput{CompatibleRuntime: lambdatypes.RuntimeNodejs20x})
	if err != nil || len(layers.Layers) != 0 {
		t.Fatalf("another runtime: %+v %v", layers, err)
	}

	created, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("layered"), Runtime: lambdatypes.RuntimePython312,
		Role: aws.String("arn:aws:iam::000000000000:role/lambda"), Handler: aws.String("app.echo"),
		Code:    &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e, c): return e\n")},
		Layers:  []string{aws.ToString(first.LayerVersionArn), aws.ToString(second.LayerVersionArn)},
		Publish: true,
	})
	if err != nil || len(created.Layers) != 2 ||
		aws.ToString(created.Layers[1].Arn) != aws.ToString(second.LayerVersionArn) {
		t.Fatalf("created: %+v %v", created, err)
	}

	if _, err = c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("layered")}); err != nil {
		t.Fatal(err)
	}

	if call := <-runtime.calls; len(call.fn.Layers) != 2 {
		t.Fatalf("the runtime got %d layers", len(call.fn.Layers))
	}

	var invalid *lambdatypes.InvalidParameterValueException
	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("layered"), Layers: []string{"arn:aws:lambda:us-east-1:000000000000:layer:shared:9"},
	})
	if !errors.As(err, &invalid) {
		t.Fatalf("missing layer version: %v", err)
	}

	updated, err := c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("layered"), Layers: []string{},
	})
	if err != nil || len(updated.Layers) != 0 {
		t.Fatalf("removed: %+v %v", updated, err)
	}

	// The published version keeps the layers it was published with, and runs
	// only while they exist.
	if _, err = c.DeleteLayerVersion(ctx, &sdklambda.DeleteLayerVersionInput{
		LayerName: aws.String("shared"), VersionNumber: aws.Int64(1),
	}); err != nil {
		t.Fatal(err)
	}

	var missing *lambdatypes.ResourceNotFoundException
	if _, err = c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: aws.String("layered"), Qualifier: aws.String("1"),
	}); !errors.As(err, &missing) {
		t.Fatalf("deleted layer: %v", err)
	}

	if _, err = c.GetLayerVersion(ctx, &sdklambda.GetLayerVersionInput{
		LayerName: aws.String("shared"), VersionNumber: aws.Int64(1),
	}); !errors.As(err, &missing) {
		t.Fatalf("deleted version: %v", err)
	}

	// Version numbers are never reused.
	if third := publishLayer(t, c, "shared", "x = 3\n"); third.Version != 3 {
		t.Fatalf("next version: %d", third.Version)
	}
}

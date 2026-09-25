package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

func TestLambdaUpdates(t *testing.T) {
	ctx := context.Background()
	runtime := &fakeRuntime{}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	created := createFunction(t, c, "worker", "app.echo")
	if created.RevisionId == nil || created.LastUpdateStatus != lambdatypes.LastUpdateStatusSuccessful {
		t.Fatalf("created = %+v", created)
	}

	arn := *created.FunctionArn

	// New code gets a new hash and revision and releases warm environments.
	code, err := c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: aws.String("worker"),
		ZipFile:      zipOf(t, "app.py", "def echo(e, c): return [e]\n"),
		RevisionId:   created.RevisionId,
	})
	if err != nil {
		t.Fatal(err)
	}

	if *code.CodeSha256 == *created.CodeSha256 || *code.RevisionId == *created.RevisionId ||
		code.CodeSize == created.CodeSize {
		t.Fatalf("code update = %+v", code)
	}

	runtime.mu.Lock()
	released := slices.Contains(runtime.released, arn)
	runtime.mu.Unlock()
	if !released {
		t.Fatal("warm environments were not released")
	}

	// A stale revision is refused.
	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: aws.String("worker"),
		ZipFile:      zipOf(t, "app.py", "x = 1\n"),
		RevisionId:   created.RevisionId,
	})
	if got, status := lambdaCode(err); got != "PreconditionFailedException" || status != http.StatusPreconditionFailed {
		t.Fatalf("stale revision: %s %d", got, status)
	}

	// A dry run validates without changing anything.
	dry, err := c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: aws.String(arn),
		ZipFile:      zipOf(t, "other.py", "y = 2\n"),
		DryRun:       true,
	})
	if err != nil || *dry.RevisionId != *code.RevisionId {
		t.Fatalf("dry run: %+v %v", dry, err)
	}

	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: aws.String("worker"),
		ZipFile:      []byte("not a zip"),
	})
	if got, _ := lambdaCode(err); got != "InvalidParameterValueException" {
		t.Fatalf("bad zip: %v", err)
	}

	// Publishing versions is not supported and fails explicitly.
	_, err = c.UpdateFunctionCode(ctx, &sdklambda.UpdateFunctionCodeInput{
		FunctionName: aws.String("worker"),
		ZipFile:      zipOf(t, "app.py", "z = 3\n"),
		Publish:      true,
	})
	if got, _ := lambdaCode(err); got != "NotImplemented" {
		t.Fatalf("publish: %v", err)
	}

	// Configuration changes apply only the fields sent, and the next
	// invocation sees them.
	cfg, err := c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("worker"),
		Timeout:      aws.Int32(30),
		Environment:  &lambdatypes.Environment{Variables: map[string]string{"GREETING": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if *cfg.Timeout != 30 || *cfg.MemorySize != 128 || *cfg.Handler != "app.echo" ||
		cfg.Environment.Variables["GREETING"] != "hello" || *cfg.RevisionId == *code.RevisionId {
		t.Fatalf("configuration = %+v", cfg)
	}

	out, err := c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("worker"), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}

	var echoed struct{ Env map[string]string }
	if err = json.Unmarshal(out.Payload, &echoed); err != nil || echoed.Env["GREETING"] != "hello" {
		t.Fatalf("invoke after update: %s %v", out.Payload, err)
	}

	// An empty environment removes every variable.
	cfg, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("worker"),
		Environment:  &lambdatypes.Environment{Variables: map[string]string{}},
	})
	if err != nil || cfg.Environment != nil {
		t.Fatalf("clear environment: %+v %v", cfg, err)
	}

	got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String("worker")})
	if err != nil || *got.Configuration.RevisionId != *cfg.RevisionId || *got.Configuration.Timeout != 30 {
		t.Fatalf("get after updates: %+v %v", got, err)
	}

	for name, in := range map[string]*sdklambda.UpdateFunctionConfigurationInput{
		"timeout": {FunctionName: aws.String("worker"), Timeout: aws.Int32(901)},
		"runtime": {FunctionName: aws.String("worker"), Runtime: "cobol"},
		"handler": {FunctionName: aws.String("worker"), Handler: aws.String("bad handler")},
		"role":    {FunctionName: aws.String("worker"), Role: aws.String("nope")},
	} {
		_, err = c.UpdateFunctionConfiguration(ctx, in)
		if got, _ := lambdaCode(err); got != "InvalidParameterValueException" {
			t.Fatalf("%s: %v", name, err)
		}
	}

	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String("missing"),
		Timeout:      aws.Int32(5),
	})
	if got, _ := lambdaCode(err); got != "ResourceNotFoundException" {
		t.Fatalf("missing: %v", err)
	}
}

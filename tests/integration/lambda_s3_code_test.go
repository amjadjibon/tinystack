package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func TestLambdaCodeFromS3(t *testing.T) {
	s := serverWith(t, &fakeRuntime{})
	c := lambdaClient(s, "test")
	objects := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	versioned(t, objects, "artifacts", s3types.BucketVersioningStatusEnabled)

	upload := func(data []byte) string {
		out, err := objects.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("artifacts"), Key: aws.String("echo.zip"), Body: bytes.NewReader(data),
		})
		if err != nil {
			t.Fatal(err)
		}

		return aws.ToString(out.VersionId)
	}

	first := zipOf(t, "app.py", "def echo(e, c): return e\n")
	second := zipOf(t, "app.py", "def echo(e, c): return {'v': 2}\n")
	v1 := upload(first)

	created, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("echo"),
		Runtime:      lambdatypes.RuntimePython312,
		Role:         aws.String("arn:aws:iam::000000000000:role/lambda"),
		Handler:      aws.String("app.echo"),
		Code:         &lambdatypes.FunctionCode{S3Bucket: aws.String("artifacts"), S3Key: aws.String("echo.zip")},
	})
	if err != nil || aws.ToString(created.CodeSha256) != sha(first) || created.CodeSize != int64(len(first)) {
		t.Fatalf("got %+v, %v", created, err)
	}

	invoked, err := c.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: aws.String("echo"), Payload: []byte(`{}`)})
	if err != nil || invoked.FunctionError != nil {
		t.Fatalf("got %+v, %v", invoked, err)
	}

	// The package is copied: replacing the object does not change the function
	// until its code is updated, which can name an older version.
	upload(second)
	got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String("echo")})
	if err != nil || aws.ToString(got.Configuration.CodeSha256) != sha(first) {
		t.Fatalf("got %+v, %v", got, err)
	}

	for version, want := range map[string][]byte{"": second, v1: first} {
		in := &sdklambda.UpdateFunctionCodeInput{
			FunctionName: aws.String("echo"), S3Bucket: aws.String("artifacts"), S3Key: aws.String("echo.zip"),
		}
		if version != "" {
			in.S3ObjectVersion = aws.String(version)
		}

		updated, updateErr := c.UpdateFunctionCode(ctx, in)
		if updateErr != nil || aws.ToString(updated.CodeSha256) != sha(want) {
			t.Fatalf("version %q: got %+v, %v", version, updated, updateErr)
		}
	}

	for name, code := range map[string]*lambdatypes.FunctionCode{
		"missing key":    {S3Bucket: aws.String("artifacts"), S3Key: aws.String("missing.zip")},
		"missing bucket": {S3Bucket: aws.String("nope"), S3Key: aws.String("echo.zip")},
		"no key":         {S3Bucket: aws.String("artifacts")},
		"both":           {S3Bucket: aws.String("artifacts"), S3Key: aws.String("echo.zip"), ZipFile: first},
	} {
		_, err = c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
			FunctionName: aws.String("other"),
			Runtime:      lambdatypes.RuntimePython312,
			Role:         aws.String("arn:aws:iam::000000000000:role/lambda"),
			Handler:      aws.String("app.echo"),
			Code:         code,
		})
		if errCode, _ := lambdaCode(err); errCode != "InvalidParameterValueException" {
			t.Fatalf("%s: %v", name, err)
		}

		if name == "missing key" && !strings.Contains(err.Error(), "S3 Error Code: NoSuchKey") {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// A bucket in another account is not readable.
	_, err = lambdaClient(s, "team-b").CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: aws.String("stolen"),
		Runtime:      lambdatypes.RuntimePython312,
		Role:         aws.String("arn:aws:iam::111111111111:role/lambda"),
		Handler:      aws.String("app.echo"),
		Code:         &lambdatypes.FunctionCode{S3Bucket: aws.String("artifacts"), S3Key: aws.String("echo.zip")},
	})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("other account: %v", err)
	}
}

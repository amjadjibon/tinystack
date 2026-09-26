package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"
)

// TestErrorMessages checks that JSON error messages reach the SDK under the
// modeled member name: Secrets Manager's "Message" and DynamoDB's "message".
func TestErrorMessages(t *testing.T) {
	s := server(t)
	ctx := context.Background()

	_, secretErr := secretsClient(s, "test", "us-east-1").
		GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String("missing")})
	_, tableErr := client(s, "test", "us-east-1").
		DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("missing")})

	for service, err := range map[string]error{
		"SecretsManager": secretErr,
		"DynamoDB":       tableErr,
	} {
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorMessage() == "" {
			t.Fatalf("%s: got %v, want a message", service, err)
		}
	}
}

package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func TestSecretsManagerResourcePolicies(t *testing.T) {
	c := secretsClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("app/key"), SecretString: aws.String("s3cret"),
	}); err != nil {
		t.Fatal(err)
	}

	policy := `{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", ` +
		`"Principal": {"AWS": "arn:aws:iam::111122223333:root"}, "Action": "secretsmanager:GetSecretValue", ` +
		`"Resource": "*"}]}`
	public := `{"Version": "2012-10-17", "Statement": {"Effect": "Allow", "Principal": "*", ` +
		`"Action": "secretsmanager:GetSecretValue", "Resource": "*"}}`

	validated, err := c.ValidateResourcePolicy(ctx, &secretsmanager.ValidateResourcePolicyInput{
		SecretId: aws.String("app/key"), ResourcePolicy: aws.String(policy),
	})
	if err != nil || !validated.PolicyValidationPassed {
		t.Fatalf("%+v %v", validated, err)
	}

	for _, bad := range []string{`{"Version": "2012-10-17"}`, `not json`} {
		_, err = c.ValidateResourcePolicy(ctx, &secretsmanager.ValidateResourcePolicyInput{
			ResourcePolicy: aws.String(bad),
		})
		code(t, err, "MalformedPolicyDocumentException")
	}

	_, err = c.PutResourcePolicy(ctx, &secretsmanager.PutResourcePolicyInput{
		SecretId: aws.String("app/key"), ResourcePolicy: aws.String(public), BlockPublicPolicy: aws.Bool(true),
	})
	code(t, err, "PublicPolicyException")

	put, err := c.PutResourcePolicy(ctx, &secretsmanager.PutResourcePolicyInput{
		SecretId: aws.String("app/key"), ResourcePolicy: aws.String(policy), BlockPublicPolicy: aws.Bool(true),
	})
	if err != nil || aws.ToString(put.Name) != "app/key" {
		t.Fatalf("%+v %v", put, err)
	}

	got, err := c.GetResourcePolicy(ctx, &secretsmanager.GetResourcePolicyInput{SecretId: aws.String("app/key")})
	if err != nil || aws.ToString(got.ResourcePolicy) == "" || aws.ToString(got.ARN) != aws.ToString(put.ARN) {
		t.Fatalf("%+v %v", got, err)
	}

	if _, err = c.DeleteResourcePolicy(ctx, &secretsmanager.DeleteResourcePolicyInput{
		SecretId: aws.String("app/key"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.GetResourcePolicy(ctx, &secretsmanager.GetResourcePolicyInput{SecretId: aws.String("app/key")})
	if err != nil || got.ResourcePolicy != nil {
		t.Fatalf("%+v %v", got, err)
	}

	_, err = c.PutResourcePolicy(ctx, &secretsmanager.PutResourcePolicyInput{
		SecretId: aws.String("missing"), ResourcePolicy: aws.String(policy),
	})
	code(t, err, "ResourceNotFoundException")
}

package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// aliasedKey creates a key with alias/<name> and returns its ID.
func aliasedKey(t *testing.T, c *kms.Client, name string) string {
	t.Helper()
	id := aws.ToString(createKey(t, c, name).KeyId)
	_, err := c.CreateAlias(context.Background(), &kms.CreateAliasInput{
		AliasName: aws.String("alias/" + name), TargetKeyId: aws.String(id),
	})
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func setKeyEnabled(t *testing.T, c *kms.Client, id string, enabled bool) {
	t.Helper()
	var err error
	if enabled {
		_, err = c.EnableKey(context.Background(), &kms.EnableKeyInput{KeyId: aws.String(id)})
	} else {
		_, err = c.DisableKey(context.Background(), &kms.DisableKeyInput{KeyId: aws.String(id)})
	}

	if err != nil {
		t.Fatal(err)
	}
}

func TestSSMCustomerManagedKeys(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	keys := kmsClient(s, "test", "us-east-1")
	c := ssmClient(s, "test", "us-east-1")
	id := aliasedKey(t, keys, "app")

	_, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/token"), Value: aws.String("s3cret"), Type: ssmtypes.ParameterTypeSecureString,
		KeyId: aws.String("alias/app"),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeParameters(ctx, &ssm.DescribeParametersInput{})
	if err != nil || len(described.Parameters) != 1 || aws.ToString(described.Parameters[0].KeyId) != "alias/app" {
		t.Fatalf("got %+v, %v", described, err)
	}

	decrypted := &ssm.GetParameterInput{Name: aws.String("/app/token"), WithDecryption: aws.Bool(true)}
	got, err := c.GetParameter(ctx, decrypted)
	if err != nil || aws.ToString(got.Parameter.Value) != "s3cret" {
		t.Fatalf("got %+v, %v", got, err)
	}

	// A disabled key fails every decrypting read, but not reads without
	// decryption.
	setKeyEnabled(t, keys, id, false)
	_, err = c.GetParameter(ctx, decrypted)
	wantCode(t, err, "InvalidKeyId")
	_, err = c.GetParameters(ctx, &ssm.GetParametersInput{
		Names: []string{"/app/token"}, WithDecryption: aws.Bool(true),
	})
	wantCode(t, err, "InvalidKeyId")
	_, err = c.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path: aws.String("/app"), WithDecryption: aws.Bool(true),
	})
	wantCode(t, err, "InvalidKeyId")
	_, err = c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{
		Name: aws.String("/app/token"), WithDecryption: aws.Bool(true),
	})
	wantCode(t, err, "InvalidKeyId")
	got, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/app/token")})
	if err != nil || aws.ToString(got.Parameter.Value) == "s3cret" {
		t.Fatalf("got %+v, %v", got, err)
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/other"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeSecureString,
		KeyId: aws.String(id),
	})
	wantCode(t, err, "InvalidKeyId")

	// Overwriting without a key keeps the current one, which must be usable.
	overwrite := &ssm.PutParameterInput{
		Name: aws.String("/app/token"), Value: aws.String("rotated"), Overwrite: aws.Bool(true),
	}
	_, err = c.PutParameter(ctx, overwrite)
	wantCode(t, err, "InvalidKeyId")
	setKeyEnabled(t, keys, id, true)
	if _, err = c.PutParameter(ctx, overwrite); err != nil {
		t.Fatal(err)
	}

	history, err := c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{Name: aws.String("/app/token")})
	if err != nil || len(history.Parameters) != 2 || aws.ToString(history.Parameters[1].KeyId) != "alias/app" {
		t.Fatalf("got %+v, %v", history, err)
	}

	// Keys resolve in the caller's account.
	other := ssmClient(s, "team-b", "us-east-1")
	_, err = other.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/token"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeSecureString,
		KeyId: aws.String(id),
	})
	wantCode(t, err, "InvalidKeyId")
}

func TestSecretsCustomerManagedKeys(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	keys := kmsClient(s, "test", "us-east-1")
	c := secretsClient(s, "test", "us-east-1")
	id := aliasedKey(t, keys, "secrets")
	arn := "arn:aws:kms:us-east-1:000000000000:key/" + id

	_, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("db"), SecretString: aws.String("pw"), KmsKeyId: aws.String(arn),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("db")})
	if err != nil || aws.ToString(described.KmsKeyId) != arn {
		t.Fatalf("got %+v, %v", described, err)
	}

	if v := secretValue(t, c, "db", ""); v != "pw" {
		t.Fatalf("value = %q", v)
	}

	setKeyEnabled(t, keys, id, false)
	_, err = c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String("db")})
	wantCode(t, err, "DecryptionFailure")
	_, err = c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String("db"), SecretString: aws.String("next"),
	})
	wantCode(t, err, "EncryptionFailure")
	_, err = c.UpdateSecret(ctx, &secretsmanager.UpdateSecretInput{
		SecretId: aws.String("db"), SecretString: aws.String("next"),
	})
	wantCode(t, err, "EncryptionFailure")

	// Moving the secret to a usable key lets it change and be read again.
	_, err = c.UpdateSecret(ctx, &secretsmanager.UpdateSecretInput{
		SecretId: aws.String("db"), KmsKeyId: aws.String("alias/aws/secretsmanager"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if v := secretValue(t, c, "db", ""); v != "pw" {
		t.Fatalf("value = %q", v)
	}

	for _, in := range []*secretsmanager.CreateSecretInput{
		{Name: aws.String("missing"), KmsKeyId: aws.String("alias/missing")},
		{Name: aws.String("disabled"), SecretString: aws.String("v"), KmsKeyId: aws.String("alias/secrets")},
	} {
		_, err = c.CreateSecret(ctx, in)
		wantCode(t, err, "EncryptionFailure")
	}

	_, err = c.UpdateSecret(ctx, &secretsmanager.UpdateSecretInput{
		SecretId: aws.String("db"), KmsKeyId: aws.String("alias/missing"),
	})
	wantCode(t, err, "EncryptionFailure")
}

func TestSQSCustomerManagedKeys(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	keys := kmsClient(s, "test", "us-east-1")
	c := sqsClient(s, "test")
	id := aliasedKey(t, keys, "queues")

	encrypted := createQueue(t, c, "encrypted", map[string]string{
		"KmsMasterKeyId": "alias/queues", "KmsDataKeyReusePeriodSeconds": "600",
	})
	attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(encrypted), AttributeNames: []sqstypes.QueueAttributeName{"All"},
	})
	if err != nil || attrs.Attributes["KmsMasterKeyId"] != "alias/queues" ||
		attrs.Attributes["KmsDataKeyReusePeriodSeconds"] != "600" ||
		attrs.Attributes["SqsManagedSseEnabled"] != "false" {
		t.Fatalf("got %+v, %v", attrs, err)
	}

	send := &sqs.SendMessageInput{QueueUrl: aws.String(encrypted), MessageBody: aws.String("hi")}
	if _, err = c.SendMessage(ctx, send); err != nil {
		t.Fatal(err)
	}

	setKeyEnabled(t, keys, id, false)
	_, err = c.SendMessage(ctx, send)
	wantCode(t, err, "KMS.DisabledException")
	_, err = c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: aws.String(encrypted),
		Entries:  []sqstypes.SendMessageBatchRequestEntry{{Id: aws.String("1"), MessageBody: aws.String("hi")}},
	})
	wantCode(t, err, "KMS.DisabledException")

	// Receiving needs no key, as TinyStack stores messages as sent.
	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(encrypted)})
	if err != nil || len(received.Messages) != 1 {
		t.Fatalf("got %+v, %v", received, err)
	}

	if _, err = keys.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{KeyId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}

	_, err = c.SendMessage(ctx, send)
	wantCode(t, err, "KMS.InvalidStateException")

	missing := createQueue(t, c, "missing-key", map[string]string{"KmsMasterKeyId": "alias/missing"})
	_, err = c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(missing), MessageBody: aws.String("hi")})
	wantCode(t, err, "KMS.NotFoundException")

	// The AWS managed key and SSE-SQS need no KMS key.
	managed := createQueue(t, c, "managed", map[string]string{"KmsMasterKeyId": "alias/aws/sqs"})
	_, err = c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(managed), MessageBody: aws.String("hi")})
	if err != nil {
		t.Fatal(err)
	}

	plain := createQueue(t, c, "plain", nil)
	for _, want := range []string{"true", "false"} {
		attrs, err = c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(plain), AttributeNames: []sqstypes.QueueAttributeName{"All"},
		})
		if err != nil || attrs.Attributes["SqsManagedSseEnabled"] != want || attrs.Attributes["KmsMasterKeyId"] != "" {
			t.Fatalf("got %+v, %v, want SqsManagedSseEnabled %s", attrs, err, want)
		}

		_, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl: aws.String(plain), Attributes: map[string]string{"SqsManagedSseEnabled": "false"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, attributes := range []map[string]string{
		{"KmsMasterKeyId": "alias/queues", "SqsManagedSseEnabled": "true"},
		{"KmsDataKeyReusePeriodSeconds": "10"},
	} {
		_, err = c.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl: aws.String(plain), Attributes: attributes,
		})
		if err == nil {
			t.Fatalf("%v accepted", attributes)
		}
	}
}

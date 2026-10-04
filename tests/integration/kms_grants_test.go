package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func TestKMSGrantsAndReEncrypt(t *testing.T) {
	c := kmsClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	newKey := func(in *kms.CreateKeyInput) string {
		t.Helper()
		out, err := c.CreateKey(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		return aws.ToString(out.KeyMetadata.KeyId)
	}

	keyID := newKey(&kms.CreateKeyInput{})
	signing := newKey(&kms.CreateKeyInput{
		KeySpec: kmstypes.KeySpecEccNistP256, KeyUsage: kmstypes.KeyUsageTypeSignVerify,
	})
	grantee := "arn:aws:iam::000000000000:role/app"
	retiring := "arn:aws:iam::000000000000:role/admin"

	created, err := c.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: &keyID, GranteePrincipal: &grantee, RetiringPrincipal: &retiring, Name: aws.String("app"),
		Operations:  []kmstypes.GrantOperation{kmstypes.GrantOperationEncrypt, kmstypes.GrantOperationDecrypt},
		Constraints: &kmstypes.GrantConstraints{EncryptionContextSubset: map[string]string{"app": "web"}},
	})
	if err != nil || aws.ToString(created.GrantId) == "" || aws.ToString(created.GrantToken) == "" {
		t.Fatalf("%+v %v", created, err)
	}

	// The same named grant is returned again.
	again, err := c.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: &keyID, GranteePrincipal: &grantee, RetiringPrincipal: &retiring, Name: aws.String("app"),
		Operations:  []kmstypes.GrantOperation{kmstypes.GrantOperationEncrypt, kmstypes.GrantOperationDecrypt},
		Constraints: &kmstypes.GrantConstraints{EncryptionContextSubset: map[string]string{"app": "web"}},
	})
	if err != nil || aws.ToString(again.GrantId) != aws.ToString(created.GrantId) {
		t.Fatalf("%+v %v", again, err)
	}

	second, err := c.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: &keyID, GranteePrincipal: aws.String("arn:aws:iam::000000000000:role/other"),
		Operations: []kmstypes.GrantOperation{kmstypes.GrantOperationGenerateDataKey},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId: &signing, GranteePrincipal: &grantee, Operations: []kmstypes.GrantOperation{"Encrypt"},
	})
	code(t, err, "ValidationException")

	grants, err := c.ListGrants(ctx, &kms.ListGrantsInput{KeyId: &keyID})
	if err != nil || len(grants.Grants) != 2 {
		t.Fatalf("%+v %v", grants, err)
	}

	byGrantee, err := c.ListGrants(ctx, &kms.ListGrantsInput{KeyId: &keyID, GranteePrincipal: &grantee})
	if err != nil || len(byGrantee.Grants) != 1 || aws.ToString(byGrantee.Grants[0].Name) != "app" ||
		byGrantee.Grants[0].Constraints.EncryptionContextSubset["app"] != "web" ||
		aws.ToString(byGrantee.Grants[0].RetiringPrincipal) != retiring {
		t.Fatalf("%+v %v", byGrantee, err)
	}

	retirable, err := c.ListRetirableGrants(ctx, &kms.ListRetirableGrantsInput{RetiringPrincipal: &retiring})
	if err != nil || len(retirable.Grants) != 1 {
		t.Fatalf("%+v %v", retirable, err)
	}

	if _, err = c.RetireGrant(ctx, &kms.RetireGrantInput{GrantToken: created.GrantToken}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.RevokeGrant(ctx, &kms.RevokeGrantInput{KeyId: &keyID, GrantId: second.GrantId}); err != nil {
		t.Fatal(err)
	}

	_, err = c.RevokeGrant(ctx, &kms.RevokeGrantInput{KeyId: &keyID, GrantId: second.GrantId})
	code(t, err, "InvalidGrantIdException")
	_, err = c.RetireGrant(ctx, &kms.RetireGrantInput{GrantToken: aws.String("bogus")})
	code(t, err, "InvalidGrantTokenException")

	grants, err = c.ListGrants(ctx, &kms.ListGrantsInput{KeyId: &keyID})
	if err != nil || len(grants.Grants) != 0 {
		t.Fatalf("%+v %v", grants, err)
	}

	// ReEncrypt moves a ciphertext to another key and encryption context.
	other := newKey(&kms.CreateKeyInput{})
	encrypted, err := c.Encrypt(ctx, &kms.EncryptInput{
		KeyId: &keyID, Plaintext: []byte("payload"), EncryptionContext: map[string]string{"v": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	moved, err := c.ReEncrypt(ctx, &kms.ReEncryptInput{
		CiphertextBlob: encrypted.CiphertextBlob, SourceEncryptionContext: map[string]string{"v": "1"},
		DestinationKeyId: &other, DestinationEncryptionContext: map[string]string{"v": "2"},
	})
	if err != nil || aws.ToString(moved.SourceKeyId) != aws.ToString(encrypted.KeyId) {
		t.Fatalf("%+v %v", moved, err)
	}

	decrypted, err := c.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: moved.CiphertextBlob, EncryptionContext: map[string]string{"v": "2"},
	})
	if err != nil || string(decrypted.Plaintext) != "payload" ||
		aws.ToString(decrypted.KeyId) != aws.ToString(moved.KeyId) {
		t.Fatalf("%+v %v", decrypted, err)
	}

	_, err = c.ReEncrypt(ctx, &kms.ReEncryptInput{CiphertextBlob: encrypted.CiphertextBlob, DestinationKeyId: &other})
	code(t, err, "InvalidCiphertextException")

	// To an RSA key, the result is an RSA-OAEP ciphertext.
	rsaKey := newKey(&kms.CreateKeyInput{
		KeySpec: kmstypes.KeySpecRsa2048, KeyUsage: kmstypes.KeyUsageTypeEncryptDecrypt,
	})
	toRSA, err := c.ReEncrypt(ctx, &kms.ReEncryptInput{
		CiphertextBlob: moved.CiphertextBlob, SourceEncryptionContext: map[string]string{"v": "2"},
		DestinationKeyId:               &rsaKey,
		DestinationEncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	})
	if err != nil || toRSA.DestinationEncryptionAlgorithm != kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256 {
		t.Fatalf("%+v %v", toRSA, err)
	}
}

package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func TestKMSMacs(t *testing.T) {
	s := server(t)
	c := kmsClient(s, "test", "us-east-1")
	ctx := context.Background()

	_, err := c.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: kmstypes.KeySpecHmac256})
	wantCode(t, err, "ValidationException")

	created, err := c.CreateKey(ctx, &kms.CreateKeyInput{
		KeySpec: kmstypes.KeySpecHmac384, KeyUsage: kmstypes.KeyUsageTypeGenerateVerifyMac,
	})
	if err != nil {
		t.Fatal(err)
	}

	meta := created.KeyMetadata
	if meta.KeySpec != kmstypes.KeySpecHmac384 || meta.KeyUsage != kmstypes.KeyUsageTypeGenerateVerifyMac ||
		len(meta.MacAlgorithms) != 1 || meta.MacAlgorithms[0] != kmstypes.MacAlgorithmSpecHmacSha384 ||
		len(meta.EncryptionAlgorithms) != 0 {
		t.Fatalf("metadata = %+v", meta)
	}

	id := meta.KeyId
	message := []byte("payload")
	generated, err := c.GenerateMac(ctx, &kms.GenerateMacInput{
		KeyId: id, Message: message, MacAlgorithm: kmstypes.MacAlgorithmSpecHmacSha384,
	})
	if err != nil || len(generated.Mac) != 48 {
		t.Fatalf("mac = %+v, %v", generated, err)
	}

	verified, err := c.VerifyMac(ctx, &kms.VerifyMacInput{
		KeyId: id, Message: message, Mac: generated.Mac, MacAlgorithm: kmstypes.MacAlgorithmSpecHmacSha384,
	})
	if err != nil || !verified.MacValid {
		t.Fatalf("verify = %+v, %v", verified, err)
	}

	_, err = c.VerifyMac(ctx, &kms.VerifyMacInput{
		KeyId: id, Message: []byte("other"), Mac: generated.Mac, MacAlgorithm: kmstypes.MacAlgorithmSpecHmacSha384,
	})
	wantCode(t, err, "KMSInvalidMacException")

	_, err = c.GenerateMac(ctx, &kms.GenerateMacInput{
		KeyId: id, Message: message, MacAlgorithm: kmstypes.MacAlgorithmSpecHmacSha256,
	})
	wantCode(t, err, "InvalidKeyUsageException")

	_, err = c.Encrypt(ctx, &kms.EncryptInput{KeyId: id, Plaintext: message})
	wantCode(t, err, "InvalidKeyUsageException")

	_, err = c.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{KeyId: id})
	wantCode(t, err, "UnsupportedOperationException")

	symmetric := createKey(t, c, "envelope")
	_, err = c.GenerateMac(ctx, &kms.GenerateMacInput{
		KeyId: symmetric.KeyId, Message: message, MacAlgorithm: kmstypes.MacAlgorithmSpecHmacSha256,
	})
	wantCode(t, err, "InvalidKeyUsageException")
}

func TestKMSDataKeyPairs(t *testing.T) {
	s := server(t)
	c := kmsClient(s, "test", "us-east-1")
	ctx := context.Background()
	id := createKey(t, c, "pairs").KeyId
	encryptionContext := map[string]string{"purpose": "signing"}

	pair, err := c.GenerateDataKeyPair(ctx, &kms.GenerateDataKeyPairInput{
		KeyId: id, KeyPairSpec: kmstypes.DataKeyPairSpecEccNistP256, EncryptionContext: encryptionContext,
	})
	if err != nil {
		t.Fatal(err)
	}

	private, err := x509.ParsePKCS8PrivateKey(pair.PrivateKeyPlaintext)
	if err != nil {
		t.Fatal(err)
	}

	public, err := x509.ParsePKIXPublicKey(pair.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	ecPrivate, ok := private.(*ecdsa.PrivateKey)
	if !ok || !ecPrivate.PublicKey.Equal(public) || pair.KeyPairSpec != kmstypes.DataKeyPairSpecEccNistP256 {
		t.Fatalf("pair = %T %T", private, public)
	}

	// The sealed private key decrypts with the same encryption context.
	opened, err := c.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: pair.PrivateKeyCiphertextBlob, EncryptionContext: encryptionContext,
	})
	if err != nil || !bytes.Equal(opened.Plaintext, pair.PrivateKeyPlaintext) {
		t.Fatalf("decrypt = %v", err)
	}

	sealed, err := c.GenerateDataKeyPairWithoutPlaintext(ctx, &kms.GenerateDataKeyPairWithoutPlaintextInput{
		KeyId: id, KeyPairSpec: kmstypes.DataKeyPairSpecRsa2048,
	})
	if err != nil {
		t.Fatal(err)
	}

	rsaPublic, err := x509.ParsePKIXPublicKey(sealed.PublicKey)
	if _, isRSA := rsaPublic.(*rsa.PublicKey); err != nil || !isRSA || len(sealed.PrivateKeyCiphertextBlob) == 0 {
		t.Fatalf("pair without plaintext = %T, %v", rsaPublic, err)
	}

	_, err = c.GenerateDataKeyPair(ctx, &kms.GenerateDataKeyPairInput{
		KeyId: id, KeyPairSpec: kmstypes.DataKeyPairSpecEccSecgP256k1,
	})
	wantCode(t, err, "ValidationException")

	_, err = c.GenerateDataKeyPair(ctx, &kms.GenerateDataKeyPairInput{
		KeyId: aws.String("alias/missing"), KeyPairSpec: kmstypes.DataKeyPairSpecEccNistP256,
	})
	wantCode(t, err, "NotFoundException")
}

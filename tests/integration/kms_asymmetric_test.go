package integration_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func TestKMSAsymmetricKeys(t *testing.T) {
	s := server(t)
	c := kmsClient(s, "test", "us-east-1")
	ctx := context.Background()
	create := func(spec kmstypes.KeySpec, usage kmstypes.KeyUsageType) *kmstypes.KeyMetadata {
		t.Helper()
		out, err := c.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: spec, KeyUsage: usage})
		if err != nil {
			t.Fatal(err)
		}

		return out.KeyMetadata
	}

	_, err := c.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: kmstypes.KeySpecRsa2048})
	code(t, err, "ValidationException")
	_, err = c.CreateKey(ctx, &kms.CreateKeyInput{
		KeySpec: kmstypes.KeySpecEccNistP256, KeyUsage: kmstypes.KeyUsageTypeEncryptDecrypt,
	})
	code(t, err, "ValidationException")
	_, err = c.CreateKey(ctx, &kms.CreateKeyInput{
		KeySpec: kmstypes.KeySpecEccSecgP256k1, KeyUsage: kmstypes.KeyUsageTypeSignVerify,
	})
	code(t, err, "UnsupportedOperationException")

	message := []byte("hello, kms")
	digest := sha256.Sum256(message)
	for _, tc := range []struct {
		spec      kmstypes.KeySpec
		algorithm kmstypes.SigningAlgorithmSpec
	}{
		{kmstypes.KeySpecRsa2048, kmstypes.SigningAlgorithmSpecRsassaPssSha256},
		{kmstypes.KeySpecRsa3072, kmstypes.SigningAlgorithmSpecRsassaPkcs1V15Sha256},
		{kmstypes.KeySpecEccNistP256, kmstypes.SigningAlgorithmSpecEcdsaSha256},
		{kmstypes.KeySpecEccNistEdwards25519, kmstypes.SigningAlgorithmSpecEd25519Sha512},
	} {
		meta := create(tc.spec, kmstypes.KeyUsageTypeSignVerify)
		if !slices.Contains(meta.SigningAlgorithms, tc.algorithm) || len(meta.EncryptionAlgorithms) != 0 ||
			meta.KeyUsage != kmstypes.KeyUsageTypeSignVerify || meta.KeySpec != tc.spec {
			t.Fatalf("%s: %+v", tc.spec, meta)
		}

		signed, signErr := c.Sign(ctx, &kms.SignInput{
			KeyId: meta.KeyId, Message: message, SigningAlgorithm: tc.algorithm,
		})
		if signErr != nil || len(signed.Signature) == 0 || signed.SigningAlgorithm != tc.algorithm {
			t.Fatalf("%s: %+v %v", tc.spec, signed, signErr)
		}

		verified, verifyErr := c.Verify(ctx, &kms.VerifyInput{
			KeyId: meta.KeyId, Message: message, Signature: signed.Signature, SigningAlgorithm: tc.algorithm,
		})
		if verifyErr != nil || !verified.SignatureValid {
			t.Fatalf("%s: %+v %v", tc.spec, verified, verifyErr)
		}

		_, err = c.Verify(ctx, &kms.VerifyInput{
			KeyId: meta.KeyId, Message: []byte("tampered"), Signature: signed.Signature,
			SigningAlgorithm: tc.algorithm,
		})
		code(t, err, "KMSInvalidSignatureException")

		// The public key verifies the signature outside KMS.
		public, pubErr := c.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: meta.KeyId})
		if pubErr != nil || public.KeySpec != tc.spec || !slices.Contains(public.SigningAlgorithms, tc.algorithm) {
			t.Fatalf("%s: %+v %v", tc.spec, public, pubErr)
		}

		parsed, parseErr := x509.ParsePKIXPublicKey(public.PublicKey)
		if parseErr != nil {
			t.Fatal(parseErr)
		}

		var valid bool
		switch pub := parsed.(type) {
		case *rsa.PublicKey:
			if tc.algorithm == kmstypes.SigningAlgorithmSpecRsassaPssSha256 {
				valid = rsa.VerifyPSS(pub, crypto.SHA256, digest[:], signed.Signature, nil) == nil
			} else {
				valid = rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], signed.Signature) == nil
			}
		case *ecdsa.PublicKey:
			valid = ecdsa.VerifyASN1(pub, digest[:], signed.Signature)
		case ed25519.PublicKey:
			valid = ed25519.Verify(pub, message, signed.Signature)
		}

		if !valid {
			t.Fatalf("%s: the public key does not verify the signature", tc.spec)
		}
	}

	// A digest signs like its message, and the algorithm must suit the key.
	ecc := create(kmstypes.KeySpecEccNistP256, kmstypes.KeyUsageTypeSignVerify)
	signed, err := c.Sign(ctx, &kms.SignInput{
		KeyId: ecc.KeyId, Message: digest[:], MessageType: kmstypes.MessageTypeDigest,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.Verify(ctx, &kms.VerifyInput{
		KeyId: ecc.KeyId, Message: message, Signature: signed.Signature,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.Sign(ctx, &kms.SignInput{
		KeyId: ecc.KeyId, Message: message, SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha384,
	})
	code(t, err, "InvalidKeyUsageException")
	_, err = c.Sign(ctx, &kms.SignInput{
		KeyId: ecc.KeyId, Message: message, MessageType: kmstypes.MessageTypeDigest,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	code(t, err, "ValidationException")

	// RSA encryption keys encrypt and decrypt, also with the public key.
	rsaKey := create(kmstypes.KeySpecRsa2048, kmstypes.KeyUsageTypeEncryptDecrypt)
	if !slices.Contains(rsaKey.EncryptionAlgorithms, kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256) {
		t.Fatalf("%+v", rsaKey)
	}

	encrypted, err := c.Encrypt(ctx, &kms.EncryptInput{
		KeyId: rsaKey.KeyId, Plaintext: []byte("secret"),
		EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	})
	if err != nil {
		t.Fatal(err)
	}

	public, err := c.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: rsaKey.KeyId})
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := x509.ParsePKIXPublicKey(public.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	offline, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, parsed.(*rsa.PublicKey), []byte("offline"), nil)
	if err != nil {
		t.Fatal(err)
	}

	for blob, want := range map[string]string{string(encrypted.CiphertextBlob): "secret", string(offline): "offline"} {
		decrypted, decryptErr := c.Decrypt(ctx, &kms.DecryptInput{
			KeyId: rsaKey.KeyId, CiphertextBlob: []byte(blob),
			EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
		})
		if decryptErr != nil || string(decrypted.Plaintext) != want {
			t.Fatalf("%+v %v", decrypted, decryptErr)
		}
	}

	_, err = c.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: offline, EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	})
	code(t, err, "ValidationException")
	_, err = c.Decrypt(ctx, &kms.DecryptInput{
		KeyId: rsaKey.KeyId, CiphertextBlob: offline,
		EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha1,
	})
	code(t, err, "InvalidCiphertextException")
	_, err = c.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{KeyId: rsaKey.KeyId, KeySpec: kmstypes.DataKeySpecAes256})
	code(t, err, "InvalidKeyUsageException")
	_, err = c.Sign(ctx, &kms.SignInput{
		KeyId: rsaKey.KeyId, Message: message, SigningAlgorithm: kmstypes.SigningAlgorithmSpecRsassaPssSha256,
	})
	code(t, err, "InvalidKeyUsageException")

	symmetric := create(kmstypes.KeySpecSymmetricDefault, kmstypes.KeyUsageTypeEncryptDecrypt)
	_, err = c.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: symmetric.KeyId})
	code(t, err, "UnsupportedOperationException")
	_, err = c.Encrypt(ctx, &kms.EncryptInput{
		KeyId: symmetric.KeyId, Plaintext: []byte("x"),
		EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	})
	code(t, err, "InvalidKeyUsageException")
}

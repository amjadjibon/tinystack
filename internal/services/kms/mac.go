package kms

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"hash"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const maxMessage = 4096

// macKey is an HMAC key spec: its key length and its one MAC algorithm.
type macKey struct {
	hash      func() hash.Hash
	algorithm string
	size      int
}

var macKeys = map[string]macKey{
	"HMAC_224": {sha256.New224, "HMAC_SHA_224", 28},
	"HMAC_256": {sha256.New, "HMAC_SHA_256", 32},
	"HMAC_384": {sha512.New384, "HMAC_SHA_384", 48},
	"HMAC_512": {sha512.New, "HMAC_SHA_512", 64},
}

// mac computes a message's MAC with an HMAC key, which must be usable and
// support the algorithm.
func (s *Service) mac(ctx context.Context, ref *string, message []byte, algorithm *string, dry *bool) (
	sum []byte, arn string, err error,
) {
	if len(message) == 0 || len(message) > maxMessage {
		return nil, "", awserror.Validation("Message must be 1 to %d bytes.", maxMessage)
	}

	err = s.view(ctx, ref, true, func(scope request.Scope, k *key) error {
		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if usageErr := k.requireUsage(scope, generateVerifyMAC); usageErr != nil {
			return usageErr
		}

		spec := macKeys[k.spec()]
		if value(algorithm) != spec.algorithm {
			return awserror.New("InvalidKeyUsageException", "Algorithm "+value(algorithm)+" is incompatible "+
				"with key spec "+k.spec()+".")
		}

		if dryErr := dryRun(dry); dryErr != nil {
			return dryErr
		}

		h := hmac.New(spec.hash, k.Material)
		h.Write(message)
		sum, arn = h.Sum(nil), keyARN(scope, k.ID)
		return nil
	})

	return sum, arn, err
}

func (s *Service) GenerateMac(ctx context.Context, in *api.GenerateMacRequest) (*api.GenerateMacResponse, error) {
	sum, arn, err := s.mac(ctx, in.KeyId, in.Message, in.MacAlgorithm, in.DryRun)
	if err != nil {
		return nil, err
	}

	return &api.GenerateMacResponse{Mac: sum, MacAlgorithm: in.MacAlgorithm, KeyId: new(arn)}, nil
}

func (s *Service) VerifyMac(ctx context.Context, in *api.VerifyMacRequest) (*api.VerifyMacResponse, error) {
	sum, arn, err := s.mac(ctx, in.KeyId, in.Message, in.MacAlgorithm, in.DryRun)
	if err != nil {
		return nil, err
	}

	if !hmac.Equal(sum, in.Mac) {
		return nil, awserror.New("KMSInvalidMacException", "The MAC is not valid for the message.")
	}

	return &api.VerifyMacResponse{KeyId: new(arn), MacValid: new(true), MacAlgorithm: in.MacAlgorithm}, nil
}

// keyPair returns a new key pair of a data key pair spec, as a PKCS #8 private
// key and an X.509 SubjectPublicKeyInfo public key, both DER.
func keyPair(spec string) (private, public []byte, err error) {
	var key crypto.Signer
	switch spec {
	case "RSA_2048", "RSA_3072", "RSA_4096":
		bits := map[string]int{"RSA_2048": 2048, "RSA_3072": 3072, "RSA_4096": 4096}[spec]
		key, err = rsa.GenerateKey(rand.Reader, bits)
	case "ECC_NIST_P256":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ECC_NIST_P384":
		key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "ECC_NIST_P521":
		key, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	default:
		return nil, nil, awserror.Validation("KeyPairSpec must be RSA_2048, RSA_3072, RSA_4096, ECC_NIST_P256, " +
			"ECC_NIST_P384, or ECC_NIST_P521.")
	}

	if err != nil {
		return nil, nil, err
	}

	if private, err = x509.MarshalPKCS8PrivateKey(key); err != nil {
		return nil, nil, err
	}

	public, err = x509.MarshalPKIXPublicKey(key.Public())
	return private, public, err
}

// dataKeyPair returns a new data key pair with its private key sealed under the
// symmetric encryption key ref names.
func (s *Service) dataKeyPair(ctx context.Context, ref *string, encryptionContext map[string]string, spec *string,
	dry *bool,
) (private, sealed, public []byte, arn string, err error) {
	if private, public, err = keyPair(value(spec)); err != nil {
		return nil, nil, nil, "", err
	}

	err = s.encrypting(ctx, ref, func(scope request.Scope, k *key) error {
		if dryErr := dryRun(dry); dryErr != nil {
			return dryErr
		}

		var sealErr error
		sealed, sealErr = k.seal(private, encryptionContext)
		arn = keyARN(scope, k.ID)
		return sealErr
	})

	return private, sealed, public, arn, err
}

func (s *Service) GenerateDataKeyPair(
	ctx context.Context,
	in *api.GenerateDataKeyPairRequest,
) (*api.GenerateDataKeyPairResponse, error) {
	private, sealed, public, arn, err := s.dataKeyPair(ctx, in.KeyId, in.EncryptionContext, in.KeyPairSpec, in.DryRun)
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyPairResponse{
		PrivateKeyCiphertextBlob: sealed, PrivateKeyPlaintext: private, PublicKey: public, KeyId: new(arn),
		KeyPairSpec: in.KeyPairSpec,
	}, nil
}

func (s *Service) GenerateDataKeyPairWithoutPlaintext(
	ctx context.Context,
	in *api.GenerateDataKeyPairWithoutPlaintextRequest,
) (*api.GenerateDataKeyPairWithoutPlaintextResponse, error) {
	_, sealed, public, arn, err := s.dataKeyPair(ctx, in.KeyId, in.EncryptionContext, in.KeyPairSpec, in.DryRun)
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyPairWithoutPlaintextResponse{
		PrivateKeyCiphertextBlob: sealed, PublicKey: public, KeyId: new(arn), KeyPairSpec: in.KeyPairSpec,
	}, nil
}

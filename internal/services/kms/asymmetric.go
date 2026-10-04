package kms

import (
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- RSAES_OAEP_SHA_1 is a KMS encryption algorithm.
	"crypto/sha256"
	"crypto/x509"
	"hash"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// Asymmetric keys keep their private key as PKCS #8 DER in the key's
// Material. Signatures and RSA ciphertexts are the standard ones, so a
// public key from GetPublicKey verifies and encrypts outside KMS.

const (
	signVerify    = "SIGN_VERIFY"
	messageRaw    = "RAW"
	messageDigest = "DIGEST"
	edwards       = "ECC_NIST_EDWARDS25519"
	ed25519Pure   = "ED25519_SHA_512"
	rsa2048       = "RSA_2048"
	rsa3072       = "RSA_3072"
	rsa4096       = "RSA_4096"
)

// asymmetricKey is an asymmetric key spec: the usages it allows and the
// algorithms of each.
type asymmetricKey struct {
	usages     []string
	signing    []string
	encryption []string
}

var (
	rsaKey = asymmetricKey{
		usages: []string{encryptDecrypt, signVerify},
		signing: []string{
			"RSASSA_PSS_SHA_256", "RSASSA_PSS_SHA_384", "RSASSA_PSS_SHA_512",
			"RSASSA_PKCS1_V1_5_SHA_256", "RSASSA_PKCS1_V1_5_SHA_384", "RSASSA_PKCS1_V1_5_SHA_512",
		},
		encryption: []string{"RSAES_OAEP_SHA_1", "RSAES_OAEP_SHA_256"},
	}
	asymmetricKeys = map[string]asymmetricKey{
		rsa2048:         rsaKey,
		rsa3072:         rsaKey,
		rsa4096:         rsaKey,
		"ECC_NIST_P256": {usages: []string{signVerify}, signing: []string{"ECDSA_SHA_256"}},
		"ECC_NIST_P384": {usages: []string{signVerify}, signing: []string{"ECDSA_SHA_384"}},
		"ECC_NIST_P521": {usages: []string{signVerify}, signing: []string{"ECDSA_SHA_512"}},
		edwards:         {usages: []string{signVerify}, signing: []string{ed25519Pure, "ED25519_PH_SHA_512"}},
	}
)

// algorithms returns the encryption and signing algorithms a key supports.
func (k *key) algorithms() (encryption, signing []string) {
	if spec, ok := asymmetricKeys[k.spec()]; ok {
		if k.usage() == signVerify {
			return nil, spec.signing
		}

		return spec.encryption, nil
	}

	if _, ok := macKeys[k.spec()]; ok {
		return nil, nil
	}

	return []string{symmetricDefault}, nil
}

// newMaterial returns new key material for a key spec: random bytes for
// symmetric encryption and HMAC keys, and a PKCS #8 DER private key for
// asymmetric ones.
func newMaterial(spec string) ([]byte, error) {
	if _, ok := asymmetricKeys[spec]; !ok {
		size := 32
		if mac, isMAC := macKeys[spec]; isMAC {
			size = mac.size
		}

		material := make([]byte, size)
		_, _ = rand.Read(material)
		return material, nil
	}

	if spec != edwards {
		private, _, err := keyPair(spec)
		return private, err
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	return x509.MarshalPKCS8PrivateKey(private)
}

func (k *key) signer() (crypto.Signer, error) {
	private, err := x509.ParsePKCS8PrivateKey(k.Material)
	if err != nil {
		return nil, err
	}

	return private.(crypto.Signer), nil
}

// asymmetric loads the asymmetric key ref names, which must be usable and
// have usage.
func (s *Service) asymmetric(
	ctx context.Context,
	ref *string,
	usage string,
	fn func(request.Scope, *key, crypto.Signer) error,
) error {
	return s.view(ctx, ref, true, func(scope request.Scope, k *key) error {
		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if _, ok := asymmetricKeys[k.spec()]; !ok {
			return awserror.New("InvalidKeyUsageException", keyARN(scope, k.ID)+" is not an asymmetric key.")
		}

		if usageErr := k.requireUsage(scope, usage); usageErr != nil {
			return usageErr
		}

		signer, err := k.signer()
		if err != nil {
			return err
		}

		return fn(scope, k, signer)
	})
}

// algorithmHash is the hash a signing algorithm uses.
func algorithmHash(algorithm string) crypto.Hash {
	switch {
	case strings.HasSuffix(algorithm, "_256"):
		return crypto.SHA256
	case strings.HasSuffix(algorithm, "_384"):
		return crypto.SHA384
	}

	return crypto.SHA512
}

// signing is what Sign and Verify work on for a message: the bytes the key
// signs and the signer options that say how.
type signing struct {
	opts  crypto.SignerOpts
	input []byte
}

// prepare checks a message and its type against the key's algorithm and
// returns what to sign.
func prepare(scope request.Scope, k *key, message []byte, messageType, algorithm *string) (*signing, error) {
	if len(message) == 0 || len(message) > maxMessage {
		return nil, awserror.Validation("Message must be 1 to %d bytes.", maxMessage)
	}

	alg := value(algorithm)
	if _, signing := k.algorithms(); !slices.Contains(signing, alg) {
		return nil, awserror.New("InvalidKeyUsageException", "Algorithm "+alg+" is incompatible with key spec "+
			k.spec()+" of "+keyARN(scope, k.ID)+".")
	}

	kind := cmp.Or(value(messageType), messageRaw)
	h := algorithmHash(alg)
	switch {
	case kind != messageRaw && kind != messageDigest:
		return nil, awserror.Validation("MessageType must be RAW or DIGEST.")
	case alg == ed25519Pure && kind != messageRaw:
		return nil, awserror.Validation("ED25519_SHA_512 signs RAW messages only.")
	case alg == ed25519Pure:
		return &signing{input: message, opts: &ed25519.Options{}}, nil
	case kind == messageDigest && len(message) != h.Size():
		return nil, awserror.Validation("A %s digest must be %d bytes.", alg, h.Size())
	}

	digest := message
	if kind == messageRaw {
		d := h.New()
		d.Write(message)
		digest = d.Sum(nil)
	}

	switch {
	case strings.HasPrefix(alg, "RSASSA_PSS"):
		return &signing{input: digest, opts: &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h}}, nil
	case alg == "ED25519_PH_SHA_512":
		return &signing{input: digest, opts: &ed25519.Options{Hash: crypto.SHA512}}, nil
	}

	return &signing{input: digest, opts: h}, nil
}

func (s *Service) Sign(ctx context.Context, in *api.SignRequest) (*api.SignResponse, error) {
	out := &api.SignResponse{SigningAlgorithm: in.SigningAlgorithm}
	err := s.asymmetric(ctx, in.KeyId, signVerify, func(scope request.Scope, k *key, signer crypto.Signer) error {
		sg, err := prepare(scope, k, in.Message, in.MessageType, in.SigningAlgorithm)
		if err != nil {
			return err
		}

		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		out.Signature, err = signer.Sign(rand.Reader, sg.input, sg.opts)
		out.KeyId = new(keyARN(scope, k.ID))
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) Verify(ctx context.Context, in *api.VerifyRequest) (*api.VerifyResponse, error) {
	out := &api.VerifyResponse{SigningAlgorithm: in.SigningAlgorithm}
	err := s.asymmetric(ctx, in.KeyId, signVerify, func(scope request.Scope, k *key, signer crypto.Signer) error {
		sg, err := prepare(scope, k, in.Message, in.MessageType, in.SigningAlgorithm)
		if err != nil {
			return err
		}

		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		if !verify(signer.Public(), sg, in.Signature) {
			return awserror.New("KMSInvalidSignatureException", "The signature is not valid for the message.")
		}

		out.KeyId, out.SignatureValid = new(keyARN(scope, k.ID)), new(true)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// verify checks a signature with a public key.
func verify(public crypto.PublicKey, sg *signing, signature []byte) bool {
	switch pub := public.(type) {
	case *rsa.PublicKey:
		if pss, ok := sg.opts.(*rsa.PSSOptions); ok {
			return rsa.VerifyPSS(pub, pss.Hash, sg.input, signature, pss) == nil
		}

		return rsa.VerifyPKCS1v15(pub, sg.opts.HashFunc(), sg.input, signature) == nil
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(pub, sg.input, signature)
	case ed25519.PublicKey:
		opts, _ := sg.opts.(*ed25519.Options)
		return ed25519.VerifyWithOptions(pub, sg.input, signature, opts) == nil
	}

	return false
}

func (s *Service) GetPublicKey(ctx context.Context, in *api.GetPublicKeyRequest) (*api.GetPublicKeyResponse, error) {
	out := &api.GetPublicKeyResponse{}
	err := s.view(ctx, in.KeyId, true, func(scope request.Scope, k *key) error {
		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if _, ok := asymmetricKeys[k.spec()]; !ok {
			return unsupported("%s is not an asymmetric key, which has no public key.", keyARN(scope, k.ID))
		}

		signer, err := k.signer()
		if err != nil {
			return err
		}

		if out.PublicKey, err = x509.MarshalPKIXPublicKey(signer.Public()); err != nil {
			return err
		}

		out.EncryptionAlgorithms, out.SigningAlgorithms = k.algorithms()
		out.KeyId, out.KeySpec, out.CustomerMasterKeySpec = new(keyARN(scope, k.ID)), new(k.spec()), new(k.spec())
		out.KeyUsage = new(k.usage())
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// oaepHash is the hash of an RSAES_OAEP encryption algorithm, which the key
// must support.
func oaepHash(scope request.Scope, k *key, algorithm *string) (hash.Hash, error) {
	encryption, _ := k.algorithms()
	alg := value(algorithm)
	if !slices.Contains(encryption, alg) {
		return nil, awserror.New("InvalidKeyUsageException", "Algorithm "+alg+" is incompatible with key spec "+
			k.spec()+" of "+keyARN(scope, k.ID)+".")
	}

	if alg == "RSAES_OAEP_SHA_1" {
		return sha1.New(), nil // #nosec G401 -- the caller chose RSAES_OAEP_SHA_1.
	}

	return sha256.New(), nil
}

// encryptAsymmetric encrypts with an RSA key, which takes no encryption
// context.
func (s *Service) encryptAsymmetric(ctx context.Context, in *api.EncryptRequest) (*api.EncryptResponse, error) {
	if len(in.EncryptionContext) > 0 {
		return nil, awserror.Validation("Asymmetric keys do not take an encryption context.")
	}

	out := &api.EncryptResponse{EncryptionAlgorithm: in.EncryptionAlgorithm}
	err := s.asymmetric(ctx, in.KeyId, encryptDecrypt, func(scope request.Scope, k *key, signer crypto.Signer) error {
		h, err := oaepHash(scope, k, in.EncryptionAlgorithm)
		if err != nil {
			return err
		}

		pub, _ := signer.Public().(*rsa.PublicKey)
		if most := pub.Size() - 2*h.Size() - 2; len(in.Plaintext) > most {
			return awserror.Validation("Plaintext must be at most %d bytes for %s with %s.", most, k.spec(),
				value(in.EncryptionAlgorithm))
		}

		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		out.CiphertextBlob, err = rsa.EncryptOAEP(h, rand.Reader, pub, in.Plaintext, nil)
		out.KeyId = new(keyARN(scope, k.ID))
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// decryptAsymmetric decrypts with the RSA key KeyId names, which an RSA
// ciphertext does not identify.
func (s *Service) decryptAsymmetric(ctx context.Context, in *api.DecryptRequest) (*api.DecryptResponse, error) {
	if in.KeyId == nil {
		return nil, awserror.Validation("KeyId is required to decrypt with an asymmetric key.")
	}

	if len(in.EncryptionContext) > 0 {
		return nil, awserror.Validation("Asymmetric keys do not take an encryption context.")
	}

	out := &api.DecryptResponse{EncryptionAlgorithm: in.EncryptionAlgorithm}
	err := s.asymmetric(ctx, in.KeyId, encryptDecrypt, func(scope request.Scope, k *key, signer crypto.Signer) error {
		h, err := oaepHash(scope, k, in.EncryptionAlgorithm)
		if err != nil {
			return err
		}

		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		private, _ := signer.(*rsa.PrivateKey)
		if out.Plaintext, err = rsa.DecryptOAEP(h, nil, private, in.CiphertextBlob, nil); err != nil {
			return invalidCiphertext()
		}

		out.KeyId = new(keyARN(scope, k.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// requireSymmetric fails unless the key is a symmetric encryption key, as
// the envelope encryption operations need.
func (k *key) requireSymmetric(scope request.Scope) error {
	if err := k.requireUsage(scope, encryptDecrypt); err != nil {
		return err
	}

	if k.spec() != symmetricDefault {
		return awserror.New("InvalidKeyUsageException", keyARN(scope, k.ID)+" key spec is "+k.spec()+
			" which is not valid for this operation.")
	}

	return nil
}

// isAsymmetricAlgorithm reports whether an encryption algorithm is RSA's.
func isAsymmetricAlgorithm(algorithm *string) bool {
	return algorithm != nil && *algorithm != symmetricDefault
}

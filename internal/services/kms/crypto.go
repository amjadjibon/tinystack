package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxPlaintext = 4096
	maxDataKey   = 1024
	// blobVersion starts every ciphertext TinyStack produces:
	// version | key ID (16 bytes) | nonce (12 bytes) | AES-GCM sealed plaintext.
	blobVersion = 1
	nonceSize   = 12
	blobHeader  = 1 + 16 + nonceSize
)

func invalidCiphertext() error {
	return awserror.New("InvalidCiphertextException", "The ciphertext is not valid.")
}

func dryRun(set *bool) error {
	if value(set) {
		return awserror.New("DryRunOperationException",
			"The request would have succeeded, but the DryRun option is set.")
	}

	return nil
}

func checkAlgorithm(algorithm *string) error {
	if algorithm != nil && *algorithm != symmetricDefault {
		return awserror.New("InvalidKeyUsageException",
			"Symmetric keys support only the "+symmetricDefault+" encryption algorithm.")
	}

	return nil
}

// additionalData binds the encryption context to a ciphertext: the same pairs,
// in any order, produce the same bytes.
func additionalData(encryptionContext map[string]string) []byte {
	if len(encryptionContext) == 0 {
		return nil
	}

	b, _ := json.Marshal(encryptionContext) // map keys are sorted
	return b
}

func (k *key) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.Material)
	if err != nil {
		return nil, err
	}

	return cipher.NewGCM(block)
}

// seal encrypts plaintext under the key and encryption context.
func (k *key) seal(plaintext []byte, encryptionContext map[string]string) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}

	id, err := hex.DecodeString(strings.ReplaceAll(k.ID, "-", ""))
	if err != nil {
		return nil, err
	}

	blob := make([]byte, blobHeader, blobHeader+len(plaintext)+aead.Overhead())
	blob[0] = blobVersion
	copy(blob[1:], id)
	_, _ = rand.Read(blob[1+16 : blobHeader])
	return aead.Seal(blob, blob[1+16:blobHeader], plaintext, additionalData(encryptionContext)), nil
}

// blobKeyID returns the ID of the key that sealed blob.
func blobKeyID(blob []byte) (string, error) {
	if len(blob) < blobHeader || blob[0] != blobVersion {
		return "", invalidCiphertext()
	}

	s := hex.EncodeToString(blob[1:17])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// open decrypts a blob that the key sealed.
func (k *key) open(blob []byte, encryptionContext map[string]string) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}

	plaintext, err := aead.Open(nil, blob[1+16:blobHeader], blob[blobHeader:], additionalData(encryptionContext))
	if err != nil {
		return nil, invalidCiphertext()
	}

	return plaintext, nil
}

// encrypting loads the key that ref names and checks that it can encrypt.
func (s *Service) encrypting(
	ctx context.Context,
	ref *string,
	fn func(request.Scope, *key) error,
) error {
	return s.view(ctx, ref, true, func(scope request.Scope, k *key) error {
		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if usageErr := k.requireUsage(scope, encryptDecrypt); usageErr != nil {
			return usageErr
		}

		return fn(scope, k)
	})
}

func (s *Service) Encrypt(ctx context.Context, in *api.EncryptRequest) (*api.EncryptResponse, error) {
	if len(in.Plaintext) == 0 || len(in.Plaintext) > maxPlaintext {
		return nil, awserror.Validation("Plaintext must be 1 to %d bytes.", maxPlaintext)
	}

	if guardErr := checkAlgorithm(in.EncryptionAlgorithm); guardErr != nil {
		return nil, guardErr
	}

	out := &api.EncryptResponse{EncryptionAlgorithm: new(symmetricDefault)}
	err := s.encrypting(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		blob, sealErr := k.seal(in.Plaintext, in.EncryptionContext)
		out.CiphertextBlob, out.KeyId = blob, new(keyARN(scope, k.ID))
		return sealErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Decrypt finds the key from the ciphertext; KeyId, when given, must name it.
func (s *Service) Decrypt(ctx context.Context, in *api.DecryptRequest) (*api.DecryptResponse, error) {
	if guardErr := checkAlgorithm(in.EncryptionAlgorithm); guardErr != nil {
		return nil, guardErr
	}

	id, err := blobKeyID(in.CiphertextBlob)
	if err != nil {
		return nil, err
	}

	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DecryptResponse{EncryptionAlgorithm: new(symmetricDefault)}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.KeyId != nil {
			named, loadErr := s.load(tx, scope, in.KeyId, true)
			if loadErr != nil {
				return loadErr
			}

			if named.ID != id {
				return awserror.New("IncorrectKeyException",
					"The key ID in the request does not identify a CMK that can perform this operation.")
			}
		}

		k, found, readErr := read[key](tx, keyKey(scope, id))
		if readErr != nil {
			return readErr
		}

		if !found || k.deleted(s.now()) {
			return invalidCiphertext()
		}

		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		plaintext, openErr := k.open(in.CiphertextBlob, in.EncryptionContext)
		out.Plaintext, out.KeyId = plaintext, new(keyARN(scope, k.ID))
		return openErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// dataKeySize returns the data key length that KeySpec (AES_256 or AES_128)
// or NumberOfBytes asks for; exactly one of them must be set.
func dataKeySize(spec *string, bytes *int64) (int, error) {
	switch {
	case spec != nil && bytes != nil:
		return 0, awserror.Validation("Specify either KeySpec or NumberOfBytes, not both.")
	case bytes != nil:
		if *bytes < 1 || *bytes > maxDataKey {
			return 0, awserror.Validation("NumberOfBytes must be between 1 and %d.", maxDataKey)
		}

		return int(*bytes), nil
	case value(spec) == "AES_256":
		return 32, nil
	case value(spec) == "AES_128":
		return 16, nil
	case spec != nil:
		return 0, awserror.Validation("KeySpec must be AES_256 or AES_128.")
	default:
		return 0, awserror.Validation("Please specify either number of bytes or key spec.")
	}
}

// dataKey returns a new data key and its ciphertext under the key ref names.
func (s *Service) dataKey(
	ctx context.Context,
	ref *string,
	encryptionContext map[string]string,
	spec *string,
	bytes *int64,
	dry *bool,
) (plaintext, blob []byte, arn string, err error) {
	size, err := dataKeySize(spec, bytes)
	if err != nil {
		return nil, nil, "", err
	}

	plaintext = make([]byte, size)
	_, _ = rand.Read(plaintext)
	err = s.encrypting(ctx, ref, func(scope request.Scope, k *key) error {
		if dryErr := dryRun(dry); dryErr != nil {
			return dryErr
		}

		var sealErr error
		blob, sealErr = k.seal(plaintext, encryptionContext)
		arn = keyARN(scope, k.ID)
		return sealErr
	})

	return plaintext, blob, arn, err
}

func (s *Service) GenerateDataKey(
	ctx context.Context,
	in *api.GenerateDataKeyRequest,
) (*api.GenerateDataKeyResponse, error) {
	plaintext, blob, arn, err := s.dataKey(
		ctx, in.KeyId, in.EncryptionContext, in.KeySpec, in.NumberOfBytes, in.DryRun,
	)
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyResponse{CiphertextBlob: blob, Plaintext: plaintext, KeyId: new(arn)}, nil
}

func (s *Service) GenerateDataKeyWithoutPlaintext(
	ctx context.Context,
	in *api.GenerateDataKeyWithoutPlaintextRequest,
) (*api.GenerateDataKeyWithoutPlaintextResponse, error) {
	_, blob, arn, err := s.dataKey(ctx, in.KeyId, in.EncryptionContext, in.KeySpec, in.NumberOfBytes, in.DryRun)
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyWithoutPlaintextResponse{CiphertextBlob: blob, KeyId: new(arn)}, nil
}

func (s *Service) GenerateRandom(
	_ context.Context,
	in *api.GenerateRandomRequest,
) (*api.GenerateRandomResponse, error) {
	n := value(in.NumberOfBytes)
	if n < 1 || n > maxDataKey {
		return nil, awserror.Validation("NumberOfBytes must be between 1 and %d.", maxDataKey)
	}

	out := make([]byte, n)
	_, _ = rand.Read(out)
	return &api.GenerateRandomResponse{Plaintext: out}, nil
}

package acm

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/pem"
	"errors"

	"github.com/amjadjibon/tinystack/internal/services/pki"
)

const pbkdf2Iterations = 100000

var (
	oidPBES2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACSHA256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidAES256CBC  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
)

type algorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

//nolint:govet // ASN.1 fixes the field order.
type pbkdf2Params struct {
	Salt       []byte
	Iterations int
	KeyLength  int
	PRF        algorithm
}

type pbes2Params struct {
	KeyDerivation algorithm
	Encryption    algorithm
}

type encryptedKeyInfo struct {
	Algorithm algorithm
	Data      []byte
}

func rawParams(v any) (asn1.RawValue, error) {
	der, err := asn1.Marshal(v)
	if err != nil {
		return asn1.RawValue{}, err
	}

	return asn1.RawValue{FullBytes: der}, nil
}

// encryptKey encrypts a PEM PKCS #8 private key with passphrase as ACM's
// ExportCertificate does: an ENCRYPTED PRIVATE KEY (PBES2 with PBKDF2 and
// HMAC-SHA256, and AES-256-CBC).
func encryptKey(pemKey string, passphrase []byte) (string, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return "", errors.New("the stored private key is not PEM")
	}

	salt, iv := make([]byte, 16), make([]byte, aes.BlockSize)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(iv)
	derived, err := pbkdf2.Key(sha256.New, string(passphrase), salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}

	c, err := aes.NewCipher(derived)
	if err != nil {
		return "", err
	}

	pad := aes.BlockSize - len(block.Bytes)%aes.BlockSize
	data := append(bytes.Clone(block.Bytes), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(c, iv).CryptBlocks(data, data)

	null := asn1.RawValue{Tag: asn1.TagNull}
	kdf, err := rawParams(pbkdf2Params{
		Salt: salt, Iterations: pbkdf2Iterations, KeyLength: 32, PRF: algorithm{oidHMACSHA256, null},
	})
	if err != nil {
		return "", err
	}

	ivParams, err := rawParams(iv)
	if err != nil {
		return "", err
	}

	scheme, err := rawParams(pbes2Params{
		KeyDerivation: algorithm{oidPBKDF2, kdf}, Encryption: algorithm{oidAES256CBC, ivParams},
	})
	if err != nil {
		return "", err
	}

	der, err := asn1.Marshal(encryptedKeyInfo{Algorithm: algorithm{oidPBES2, scheme}, Data: data})
	if err != nil {
		return "", err
	}

	return pki.Encode("ENCRYPTED PRIVATE KEY", der), nil
}

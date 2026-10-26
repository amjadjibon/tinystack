package sigv4

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
)

// p256MinusTwo bounds SigV4a key candidates: a candidate c below n-2 gives the private key c+1, which lies in [1, n-1].
var p256MinusTwo = new(big.Int).Sub(elliptic.P256().Params().N, big.NewInt(2)).FillBytes(make([]byte, 32))

// V4AKey derives the ECDSA P-256 key that SigV4a signs with from an access key pair, as the AWS SDKs do: candidates
// come from an HMAC-SHA256 counter-mode KDF (NIST SP 800-108) keyed with "AWS4A"+secret, labelled with the algorithm
// and given the access key and a one-byte counter as context, until one is below n-2 (FIPS 186-4 B.4.2).
func V4AKey(accessKey, secret string) (*ecdsa.PrivateKey, error) {
	mac := hmac.New(sha256.New, []byte("AWS4A"+secret))
	for counter := 1; counter <= 0xFF; counter++ {
		// The KDF needs one 256-bit block: [i=1]₃₂ || label || 0x00 || context || [L=256]₃₂.
		mac.Reset()
		mac.Write(binary.BigEndian.AppendUint32(nil, 1))
		mac.Write([]byte(AlgorithmV4A))
		mac.Write([]byte{0})
		mac.Write([]byte(accessKey))
		mac.Write([]byte{byte(counter)})
		mac.Write(binary.BigEndian.AppendUint32(nil, 256))
		candidate := mac.Sum(nil)
		if !lessThan(candidate, p256MinusTwo) {
			continue
		}

		d := new(big.Int).Add(new(big.Int).SetBytes(candidate), big.NewInt(1))
		return ecdsa.ParseRawPrivateKey(elliptic.P256(), d.FillBytes(make([]byte, 32)))
	}

	return nil, errors.New("no SigV4a key candidate below the curve order")
}

// lessThan compares two equal-length big-endian integers in constant time.
func lessThan(x, y []byte) bool {
	borrow := 0
	for i := len(x) - 1; i >= 0; i-- {
		diff := int(x[i]) - int(y[i]) - borrow
		borrow = 0
		if diff < 0 {
			borrow = 1
		}
	}

	return subtle.ConstantTimeEq(int32(borrow), 1) == 1
}

// verifyV4A checks a hex DER ECDSA signature over the SHA-256 of stringToSign with the key derived from the pair.
func verifyV4A(accessKey, secret, stringToSign, signature string) error {
	key, err := V4AKey(accessKey, secret)
	if err != nil {
		return err
	}

	sig, err := hex.DecodeString(signature)
	if err != nil {
		return ErrMismatch
	}

	digest := sha256.Sum256([]byte(stringToSign))
	if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], sig) {
		return ErrMismatch
	}

	return nil
}

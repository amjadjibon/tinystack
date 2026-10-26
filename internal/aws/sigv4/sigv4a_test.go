package sigv4

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The AWS SDK for Go's own derivation vector (internal/v4a TestDeriveECDSAKeyPairFromSecret).
func TestV4AKey(t *testing.T) {
	key, err := V4AKey("AKISORANDOMAASORANDOM", "q+jcrXGc+0zWN6uzclKVhvMmUsIfRPa4rlRandom")
	if err != nil {
		t.Fatal(err)
	}

	public, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	want := "04" + "15D242CEEBF8D8169FD6A8B5A746C41140414C3B07579038DA06AF89190FFFCB" +
		"0515242CEDD82E94799482E4C0514B505AFCCF2C0C98D6A553BF539F424C5EC0"
	if got := strings.ToUpper(hex.EncodeToString(public)); got != want {
		t.Fatalf("public key %s, want %s", got, want)
	}
}

func TestVerifyV4A(t *testing.T) {
	key, err := V4AKey("AKID", "secret")
	if err != nil {
		t.Fatal(err)
	}

	const stringToSign = "AWS4-ECDSA-P256-SHA256\n20261026T000000Z\n20261026/cloudfront-keyvaluestore/aws4_request\nab"
	digest := sha256.Sum256([]byte(stringToSign))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	if err := verifyV4A("AKID", "secret", stringToSign, hex.EncodeToString(sig)); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][3]string{
		"wrong secret":     {"AKID", "other", hex.EncodeToString(sig)},
		"wrong access key": {"OTHER", "secret", hex.EncodeToString(sig)},
		"not hex":          {"AKID", "secret", "zz"},
	} {
		if err := verifyV4A(args[0], args[1], stringToSign, args[2]); !errors.Is(err, ErrMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

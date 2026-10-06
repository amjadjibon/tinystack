package acm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/pki"
)

const (
	algRSA2048   = "RSA_2048"
	algEC256     = "EC_prime256v1"
	algEC384     = "EC_secp384r1"
	validity     = 395 * 24 * time.Hour
	caCommonName = "TinyStack Local Certificate Authority"
)

// newKey generates a private key for one of the key algorithms ACM issues.
func newKey(alg string) (crypto.Signer, error) {
	switch alg {
	case algRSA2048:
		return rsa.GenerateKey(rand.Reader, 2048)
	case algEC256:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case algEC384:
		return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	}

	return nil, invalidParameter("The key algorithm %s is not supported for requested certificates.", alg)
}

// issue signs a new server certificate for names, the first being the
// subject, and returns it with its key.
func issue(ca *pki.Authority, names []string, alg string, now time.Time) (cert *x509.Certificate, body,
	pemKey string, err error,
) {
	k, err := newKey(alg)
	if err != nil {
		return nil, "", "", err
	}

	cert, body, err = ca.Issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: names[0]},
		DNSNames:    names,
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}, k.Public())
	if err != nil {
		return nil, "", "", err
	}

	pemKey, err = pki.EncodeKey(k)
	return cert, body, pemKey, err
}

// keyAlgorithm names a public key's algorithm as ACM does, or "".
func keyAlgorithm(pub crypto.PublicKey) string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if bits := k.N.BitLen(); bits == 1024 || bits == 2048 || bits == 3072 || bits == 4096 {
			return fmt.Sprintf("RSA_%d", bits)
		}
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return algEC256
		case elliptic.P384():
			return algEC384
		case elliptic.P521():
			return "EC_secp521r1"
		}
	}

	return ""
}

// serialString formats a serial number as colon-separated hex bytes.
func serialString(n *big.Int) string {
	b := hex.EncodeToString(n.Bytes())
	parts := make([]string, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		parts = append(parts, b[i:i+2])
	}

	return strings.Join(parts, ":")
}

// signatureName names a signature algorithm as ACM does.
func signatureName(alg x509.SignatureAlgorithm) string {
	names := map[x509.SignatureAlgorithm]string{
		x509.SHA256WithRSA: "SHA256WITHRSA", x509.SHA384WithRSA: "SHA384WITHRSA", x509.SHA512WithRSA: "SHA512WITHRSA",
		x509.ECDSAWithSHA256: "SHA256WITHECDSA", x509.ECDSAWithSHA384: "SHA384WITHECDSA",
		x509.ECDSAWithSHA512: "SHA512WITHECDSA", x509.SHA1WithRSA: "SHA1WITHRSA",
	}
	if name, ok := names[alg]; ok {
		return name
	}

	return strings.ToUpper(alg.String())
}

// issuerName is the name ACM reports for a certificate's issuer.
func issuerName(c *x509.Certificate) string {
	if len(c.Issuer.Organization) > 0 {
		return c.Issuer.Organization[0]
	}

	return c.Issuer.CommonName
}

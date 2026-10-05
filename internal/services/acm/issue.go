package acm

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	algRSA2048   = "RSA_2048"
	algEC256     = "EC_prime256v1"
	algEC384     = "EC_secp384r1"
	validity     = 395 * 24 * time.Hour
	caValidity   = 20 * 365 * 24 * time.Hour
	caCommonName = "TinyStack Local Certificate Authority"
	blockCert    = "CERTIFICATE"
	blockKey     = "PRIVATE KEY"
)

// authority is TinyStack's certificate authority.
type authority struct {
	key  crypto.Signer
	cert *x509.Certificate
	pem  string
}

// stored is the authority's record.
type stored struct{ Key, Cert string }

// authority returns the certificate authority, creating it on first use.
func (s *Service) authority(ctx context.Context) (*authority, error) {
	s.caMu.Lock()
	defer s.caMu.Unlock()
	if s.ca != nil {
		return s.ca, nil
	}

	fresh, err := newAuthority(s.now())
	if err != nil {
		return nil, err
	}

	var rec *stored
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var found bool
		var readErr error
		if rec, found, readErr = storage.Read[stored](tx, caKey); readErr != nil || found {
			return readErr
		}

		rec = fresh
		return storage.Write(tx, caKey, rec)
	})
	if err != nil {
		return nil, err
	}

	if s.ca, err = parseAuthority(rec); err != nil {
		return nil, err
	}

	return s.ca, nil
}

func newAuthority(now time.Time) (*stored, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{"TinyStack"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return nil, err
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}

	return &stored{Key: encode(blockKey, keyDER), Cert: encode(blockCert, der)}, nil
}

func parseAuthority(rec *stored) (*authority, error) {
	certs, err := parseCertificates([]byte(rec.Cert))
	if err != nil {
		return nil, err
	}

	k, err := parseKey([]byte(rec.Key))
	if err != nil {
		return nil, err
	}

	return &authority{key: k, cert: certs[0], pem: rec.Cert}, nil
}

func encode(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

func serialNumber() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n.Add(n, big.NewInt(1))
}

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
func (a *authority) issue(names []string, alg string, now time.Time) (cert *x509.Certificate, body, pemKey string,
	err error,
) {
	k, err := newKey(alg)
	if err != nil {
		return nil, "", "", err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, k.Public(), a.key)
	if err != nil {
		return nil, "", "", err
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, "", "", err
	}

	cert, err = x509.ParseCertificate(der)
	return cert, encode(blockCert, der), encode(blockKey, keyDER), err
}

// parseCertificates reads one or more PEM certificates.
func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}

		if block.Type != blockCert {
			return nil, fmt.Errorf("unexpected PEM block %s", block.Type)
		}

		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}

		out = append(out, c)
	}

	if len(out) == 0 || strings.TrimSpace(string(data)) != "" {
		return nil, errors.New("expected PEM certificates")
	}

	return out, nil
}

// parseKey reads a PEM private key in PKCS #8, PKCS #1, or SEC 1 form.
func parseKey(data []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(data)
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("expected one PEM private key")
	}

	var k any
	var err error
	switch block.Type {
	case blockKey:
		k, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		k, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		k, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block %s", block.Type)
	}

	if err != nil {
		return nil, err
	}

	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported private key type")
	}

	return signer, nil
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

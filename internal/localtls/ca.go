// Package localtls manages TinyStack's local certificate authority and gateway certificates.
package localtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Authority is a persisted local CA. Its private key never leaves this package.
type Authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	PEM  []byte
}

// Load creates a CA only when its directory is absent. An incomplete, expired or
// corrupt authority fails explicitly; it is never silently replaced.
func Load(dataDir string) (*Authority, error) {
	dir := filepath.Join(dataDir, "tls")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if createErr := create(dataDir, dir); createErr != nil {
			return nil, createErr
		}
	} else if err != nil {
		return nil, err
	}

	// #nosec G304 -- CA paths are selected by the user's data directory.
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}

	// #nosec G304 -- private CA file in the user's data directory.
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		return nil, err
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load local CA: %w", err)
	}

	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}

	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() || !cert.IsCA || !cert.BasicConstraintsValid ||
		cert.CheckSignatureFrom(cert) != nil || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
		return nil, errors.New("local CA must be a valid, unexpired self-signed ECDSA P-256 authority")
	}

	return &Authority{cert: cert, key: key, PEM: certPEM}, nil
}

func serial() (*big.Int, error) {
	// Positive 128-bit serial, including in the extremely unlikely all-zero case.
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	return n.Add(n, big.NewInt(1)), nil
}

func create(dataDir, dir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	stage, err := os.MkdirTemp(dataDir, ".tls-")
	if err != nil {
		return err
	}

	defer func() { _ = os.RemoveAll(stage) }()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	number, err := serial()
	if err != nil {
		return err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: number, Subject: pkix.Name{CommonName: "TinyStack Local CA"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}

	if writeErr := os.WriteFile(filepath.Join(stage, "ca-key.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0o600); writeErr != nil {
		return writeErr
	}

	if writeErr := os.WriteFile(filepath.Join(stage, "ca.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); writeErr != nil {
		return writeErr
	}

	// Publish the complete pair together. A concurrent creator wins without
	// replacing its nonempty directory; Load then reads that authority.
	if err = os.Rename(stage, dir); err != nil {
		if _, statErr := os.Stat(dir); statErr != nil {
			return err
		}
	}

	return nil
}

// maxCached bounds the leaves Config keeps; names past it are issued per handshake.
const maxCached = 4096

// Config returns the gateway's TLS configuration. Each server name a client
// asks for gets its own leaf on first use, when it is a gateway endpoint name
// or one of names; other names, and clients that send none, get the leaf for
// localhost, the loopback IPs and the IPs in names.
func (a *Authority) Config(names []string) (*tls.Config, error) {
	if err := ValidateNames(names); err != nil {
		return nil, err
	}

	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			ips = append(ips, ip)
		}
	}

	fallback, err := a.issue("localhost", ips)
	if err != nil {
		return nil, err
	}

	var mu sync.Mutex
	leaves := map[string]*tls.Certificate{}
	get := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
		if !covered(name, names) {
			return fallback, nil
		}

		mu.Lock()
		defer mu.Unlock()
		if leaf, ok := leaves[name]; ok && time.Now().Before(leaf.Leaf.NotAfter) {
			return leaf, nil
		}

		leaf, issueErr := a.issue(name, nil)
		if issueErr != nil {
			return nil, issueErr
		}

		if len(leaves) < maxCached {
			leaves[name] = leaf
		}

		return leaf, nil
	}

	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: get}, nil
}

// issue signs a 365-day leaf for name and ips, capped by the authority's expiry.
func (a *Authority) issue(name string, ips []net.IP) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	number, err := serial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	notAfter := now.Add(365 * 24 * time.Hour)
	if a.cert.NotAfter.Before(notAfter) {
		notAfter = a.cert.NotAfter
	}

	template := &x509.Certificate{
		SerialNumber: number, Subject: pkix.Name{CommonName: name},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: notAfter,
		DNSNames: []string{name}, IPAddresses: ips, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

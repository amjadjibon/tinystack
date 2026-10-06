// Package pki holds the X.509 work that services handing out certificates
// share: a certificate authority kept under one storage key and created on
// first use, issuing certificates from it, and PEM encoding and parsing.
package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// BlockCertificate and BlockKey are the PEM block types of a certificate
	// and of a PKCS #8 private key.
	BlockCertificate = "CERTIFICATE"
	BlockKey         = "PRIVATE KEY"
	caValidity       = 20 * 365 * 24 * time.Hour
)

// Authority is a certificate authority: its key, its certificate, and that
// certificate as PEM.
type Authority struct {
	Key         crypto.Signer
	Certificate *x509.Certificate
	PEM         string
}

// record is an authority's storage record, both fields PEM.
type record struct{ Key, Cert string }

// Lazy is the certificate authority stored under one key, created the first
// time it is needed and then kept in memory.
type Lazy struct {
	store      storage.Store
	ca         *Authority
	key        string
	commonName string
	mu         sync.Mutex
}

// NewLazy returns the authority stored under key, whose certificate, if it
// has to be created, names commonName.
func NewLazy(store storage.Store, key, commonName string) *Lazy {
	return &Lazy{store: store, key: key, commonName: commonName}
}

// Get returns the authority, creating it, valid from now, on first use.
func (l *Lazy) Get(ctx context.Context, now time.Time) (*Authority, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ca != nil {
		return l.ca, nil
	}

	fresh, err := newRecord(l.commonName, now)
	if err != nil {
		return nil, err
	}

	var rec *record
	err = l.store.Update(ctx, func(tx storage.WriteTx) error {
		var found bool
		var readErr error
		if rec, found, readErr = storage.Read[record](tx, l.key); readErr != nil || found {
			return readErr
		}

		rec = fresh
		return storage.Write(tx, l.key, rec)
	})
	if err != nil {
		return nil, err
	}

	certs, err := ParseCertificates([]byte(rec.Cert))
	if err != nil {
		return nil, err
	}

	k, err := ParseKey([]byte(rec.Key))
	if err != nil {
		return nil, err
	}

	l.ca = &Authority{Key: k, Certificate: certs[0], PEM: rec.Cert}
	return l.ca, nil
}

func newRecord(commonName string, now time.Time) (*record, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          SerialNumber(),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"TinyStack"}},
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

	return &record{Key: Encode(BlockKey, keyDER), Cert: Encode(BlockCertificate, der)}, nil
}

// Issue signs a certificate for pub from tmpl (giving it a serial number when
// it has none) and returns it with its PEM.
func (a *Authority) Issue(tmpl *x509.Certificate, pub crypto.PublicKey) (*x509.Certificate, string, error) {
	if tmpl.SerialNumber == nil {
		tmpl.SerialNumber = SerialNumber()
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Certificate, pub, a.Key)
	if err != nil {
		return nil, "", err
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, "", err
	}

	return cert, Encode(BlockCertificate, der), nil
}

// Encode writes one PEM block.
func Encode(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

// EncodeKey writes a private key as a PKCS #8 PEM block.
func EncodeKey(k crypto.Signer) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", err
	}

	return Encode(BlockKey, der), nil
}

// SerialNumber returns a random positive 127-bit serial number.
func SerialNumber() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n.Add(n, big.NewInt(1))
}

// ParseCertificates reads one or more PEM certificates and nothing else.
func ParseCertificates(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}

		if block.Type != BlockCertificate {
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

// ParseKey reads a PEM private key in PKCS #8, PKCS #1, or SEC 1 form.
func ParseKey(data []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(data)
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("expected one PEM private key")
	}

	var k any
	var err error
	switch block.Type {
	case BlockKey:
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

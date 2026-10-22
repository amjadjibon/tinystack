package app

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/localtls"
)

func TestSuppliedGatewayCertificate(t *testing.T) {
	dir := t.TempDir()
	authority, err := localtls.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	issuer, err := authority.Config([]string{"custom.test"})
	if err != nil {
		t.Fatal(err)
	}

	pair, err := issuer.GetCertificate(&tls.ClientHelloInfo{ServerName: "custom.test"})
	if err != nil {
		t.Fatal(err)
	}

	private, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Storage.Mode = config.StoragePersistent
	cfg.Storage.Path = t.TempDir()
	cfg.TLS.CertFile = filepath.Join(dir, "server.pem")
	cfg.TLS.KeyFile = filepath.Join(dir, "server-key.pem")
	if err = os.WriteFile(cfg.TLS.CertFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o644); err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(cfg.TLS.KeyFile,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := gatewayTLS(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(loaded.Certificates[0].Certificate[0], pair.Certificate[0]) {
		t.Fatal("supplied certificate was not used")
	}

	if _, err = os.Stat(filepath.Join(cfg.Storage.Path, "tls")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supplied pair created a local CA: %v", err)
	}

	cfg.TLS.KeyFile = filepath.Join(dir, "missing.pem")
	if _, err = gatewayTLS(&cfg); err == nil {
		t.Fatal("missing key was silently ignored")
	}
}

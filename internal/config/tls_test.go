package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTLSConfiguration(t *testing.T) {
	cfg := Default()
	if cfg.TLS.Enabled || cfg.TLS.Address != "127.0.0.1:4443" {
		t.Fatal("unexpected TLS defaults")
	}

	for _, address := range []string{"127.0.0.1:0", "[::1]:4443", "localhost:4443"} {
		cfg.TLS.Address = address
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", address, err)
		}
	}

	for _, address := range []string{"127.0.0.1", ":4443", "localhost:https", "127.0.0.1:65536", "127.0.0.1:-1"} {
		cfg.TLS.Address = address
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted address %q", address)
		}
	}

	cfg = Default()
	cfg.TLS.CertFile = "cert.pem"
	if cfg.Validate() == nil {
		t.Fatal("accepted incomplete certificate pair")
	}

	cfg.TLS.KeyFile = "key.pem"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	cfg.TLS.Names = []string{"custom.local"}
	if cfg.Validate() == nil {
		t.Fatal("accepted names with supplied certificate")
	}

	cfg = Default()
	cfg.TLS.Names = []string{"https://bad"}
	if cfg.Validate() == nil {
		t.Fatal("accepted invalid SAN")
	}
}

func TestTLSDataDir(t *testing.T) {
	cfg := Default()
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}

	got, err := cfg.DataDir()
	if err != nil || got != filepath.Join(base, "tinystack") {
		t.Fatalf("memory directory %q: %v", got, err)
	}

	cfg.Storage.Mode = StoragePersistent
	cfg.Storage.Path = "~/tinystack-test"
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	got, err = cfg.DataDir()
	if err != nil || got != filepath.Join(home, "tinystack-test") {
		t.Fatalf("expanded directory %q: %v", got, err)
	}

	cfg.Storage.Path = filepath.Join(t.TempDir(), "data")
	got, err = cfg.DataDir()
	if err != nil || got != cfg.Storage.Path {
		t.Fatalf("persistent directory %q: %v", got, err)
	}
}

package localtls

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceAndNames(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	second, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first.PEM, second.PEM) {
		t.Fatal("CA changed on second load")
	}

	for file, want := range map[string]os.FileMode{"ca.pem": 0o644, "ca-key.pem": 0o600} {
		info, statErr := os.Stat(filepath.Join(dir, "tls", file))
		if statErr != nil {
			t.Fatal(statErr)
		}

		if info.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", file, info.Mode().Perm(), want)
		}
	}

	cfg, err := second.Config([]string{"custom.example.test", "192.0.2.1", "*.extra.test"})
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(first.PEM)
	leaf := func(name string) *x509.Certificate {
		t.Helper()
		pair, getErr := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
		if getErr != nil {
			t.Fatal(getErr)
		}

		return pair.Leaf
	}

	for _, name := range []string{
		"sqs.us-east-1.amazonaws.com", "dotted.bucket.s3.us-east-1.amazonaws.com", "SQS.cn-north-1.amazonaws.com.cn.",
		"abc.lambda-url.us-east-1.on.aws", "pool.auth.us-east-1.amazoncognito.com", "s3.localhost.localstack.cloud",
		"localhost.localstack.cloud", "localhost", "app.localhost", "custom.example.test", "a.extra.test",
	} {
		cert := leaf(name)
		if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("%s: %v", name, err)
		}

		if cert.NotAfter.Sub(cert.NotBefore) > 397*24*time.Hour {
			t.Errorf("%s: leaf exceeds 397 days", name)
		}
	}

	if cached := leaf("sqs.us-east-1.amazonaws.com"); leaf("sqs.us-east-1.amazonaws.com") != cached {
		t.Error("leaf was not cached")
	}

	fallback := leaf("")
	for _, name := range []string{"localhost", "127.0.0.1", "::1", "192.0.2.1"} {
		if err = fallback.VerifyHostname(name); err != nil {
			t.Errorf("fallback %s: %v", name, err)
		}
	}

	for _, name := range []string{"unrelated.example", "b.a.extra.test", "extra.test"} {
		if leaf(name) != fallback {
			t.Errorf("issued a leaf for %s", name)
		}
	}

	if err = os.Remove(filepath.Join(dir, "tls", "ca-key.pem")); err != nil {
		t.Fatal(err)
	}

	if _, err = Load(dir); err == nil {
		t.Fatal("incomplete CA silently replaced")
	}
}

func TestInvalidNames(t *testing.T) {
	for _, name := range []string{"", "https://example.com", "a.*.example.com", "*.*.com", "bad_name", "-bad", "bad."} {
		if err := ValidateNames([]string{name}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

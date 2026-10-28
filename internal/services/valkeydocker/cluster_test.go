package valkeydocker

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/localtls"
)

func TestClusterArgs(t *testing.T) {
	spec := &ClusterSpec{Port: 51234, TLS: true}
	args := ClusterArgs(spec)
	for _, pair := range [][2]string{
		{"--cluster-enabled", "yes"},
		{"--cluster-announce-ip", "127.0.0.1"},
		{"--cluster-announce-port", "51234"},
		{"--cluster-announce-tls-port", "51234"},
		{"--port", "0"},
		{"--tls-port", "6379"},
		{"--appendonly", "yes"},
		{"--tls-auth-clients", "no"},
	} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 == len(args) || args[i+1] != pair[1] {
			t.Fatal(pair, args)
		}
	}

	spec.TLS = false
	if slices.Contains(ClusterArgs(spec), "--tls-port") {
		t.Fatal("plain cluster enables TLS")
	}
}

func TestACLCommand(t *testing.T) {
	got := ACLCommand("alice", "on ~* +@read", []string{"a", "b"})
	want := []any{"ACL", "SETUSER", "alice", "reset", "on", "~*", "+@read", "#a", "#b"}
	if !slices.Equal(got, want) {
		t.Fatal(got)
	}
}

func TestClusterCertificateConfiguration(t *testing.T) {
	ca, err := localtls.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	runtime := NewCluster(ca, "owner")
	dir, err := runtime.configuration(&ClusterSpec{TLS: true, Admin: "private-administrator-password"})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = os.RemoveAll(filepath.Dir(dir)) }()
	info, err := os.Stat(filepath.Dir(dir))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal(info, err)
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM)
	if _, err = leaf.Verify(x509.VerifyOptions{DNSName: "127.0.0.1", Roots: roots}); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(filepath.Join(dir, "users.acl"))
	if err != nil || strings.Contains(string(contents), "private-administrator-password") ||
		!strings.Contains(string(contents), "user default off") {
		t.Fatal(string(contents), err)
	}
}

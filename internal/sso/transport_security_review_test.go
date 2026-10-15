package sso

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestDeviceLoginRejectsInsecureEndpointBeforeDiscovery(t *testing.T) {
	called := false
	issuer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer issuer.Close()
	_, err := Login(context.Background(), &config.OIDC{Issuer: issuer.URL}, "http://remote.example", "", io.Discard)
	if err == nil || called {
		t.Fatalf("insecure endpoint reached discovery: error=%v called=%v", err, called)
	}
}

func TestSSOClientDoesNotForwardTokenOnRedirect(t *testing.T) {
	leaked := false
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer receiver.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, receiver.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := redeem(context.Background(), &http.Client{}, server.URL, "secret-id-token", "")
	if err == nil || leaked {
		t.Fatalf("ID token forwarded to redirect: error=%v leaked=%v", err, leaked)
	}
}

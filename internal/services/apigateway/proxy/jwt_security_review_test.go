package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJWTDiscoveryRefusesRemoteHTTPKeysAndRedirects(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		contacted := false
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/jwks" {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			contacted = true
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
		}))
		t.Cleanup(target.Close)
		issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/.well-known/openid-configuration" {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			if redirect {
				http.Redirect(w, r, target.URL+"/jwks", http.StatusTemporaryRedirect)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": "http://remote.example/jwks"})
		}))
		t.Cleanup(issuer.Close)
		previous := http.DefaultTransport
		http.DefaultTransport = reviewTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "remote.example" {
				contacted = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"keys":[]}`)),
					Header:     http.Header{},
				}, nil
			}
			return previous.RoundTrip(r)
		})
		t.Cleanup(func() { http.DefaultTransport = previous })
		if _, err := readKeys(context.Background(), issuer.URL); err == nil || contacted {
			t.Fatalf("untrusted keys accepted/fetched: redirect=%v error=%v contacted=%v", redirect, err, contacted)
		}
	}
}

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

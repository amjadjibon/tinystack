package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOriginURLAcceptsDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		configured, public, origin string
		want                       bool
	}{
		{"https://console.example:443/_tinystack/auth/callback", "https://console.example", "https://console.example", true},
		{"https://CONSOLE.EXAMPLE/_tinystack/auth/callback", "https://console.example:443", "https://console.example", true},
		{"http://localhost:80/_tinystack/auth/callback", "http://localhost", "http://localhost", true},
		{"https://console.example:443/_tinystack/auth/callback", "https://console.example", "https://evil.example", false},
		{"https://console.example/_tinystack/auth/callback", "https://console.example", "http://console.example", false},
		{
			"https://console.example/_tinystack/auth/callback", "https://console.example:8443",
			"https://console.example", false,
		},
		{
			"https://console.example/_tinystack/auth/callback", "https://console.example",
			"https://console.example:8443", false,
		},
		{
			"https://console.example/_tinystack/auth/callback", "https://console.example",
			"https://user@console.example", false,
		},
		{"https://console.example/_tinystack/auth/callback", "https://console.example", "null", false},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.public+"/_tinystack/actions", http.NoBody)
		r.Header.Set("Origin", tc.origin)
		if got := SameOriginURL(r, tc.configured); got != tc.want {
			t.Fatalf("configured %s, request %s, origin %s: got %v, want %v", tc.configured, tc.public,
				tc.origin, got, tc.want)
		}
	}
}

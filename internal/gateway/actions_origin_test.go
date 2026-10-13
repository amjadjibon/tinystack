package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestConsoleActionOriginUsesPinnedHTTPSOriginBehindProxy(t *testing.T) {
	cfg := config.Default()
	cfg.Console.Auth.OIDC = new(config.OIDC{RedirectURL: "https://console.example/_tinystack/auth/callback"})
	s := new(Server{cfg: cfg})
	for _, tc := range []struct {
		name, host, origin, site, mode string
		want                           bool
	}{
		{"public_https", "console.example", "https://console.example", "same-origin", "cors", true},
		{"metadata_only", "console.example", "", "same-origin", "cors", true},
		{"programmatic_aws", "127.0.0.1:4566", "", "", "", true},
		{"programmatic_console", "127.0.0.1:4567", "", "", "", true},
		{"wrong_scheme", "console.example", "http://console.example", "same-origin", "cors", false},
		{"wrong_origin", "console.example", "https://evil.example", "same-origin", "cors", false},
		{"wrong_host", "127.0.0.1:4566", "https://console.example", "same-origin", "cors", false},
		{"wrong_metadata_host", "127.0.0.1:4566", "", "same-origin", "cors", false},
		{"mode_without_site", "127.0.0.1:4566", "", "", "cors", false},
		{"cross_site", "console.example", "https://console.example", "cross-site", "cors", false},
		{"same_site", "console.example", "https://console.example", "same-site", "cors", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/_tinystack/actions", http.NoBody)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-Forwarded-Proto", "https")
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}

			if tc.mode != "" {
				r.Header.Set("Sec-Fetch-Mode", tc.mode)
			}

			if got := s.sameOrigin(r); got != tc.want {
				t.Fatalf("sameOrigin = %v, want %v", got, tc.want)
			}

			// Cookie handoffs keep using the strict helper, including without headers.
			if tc.host != "console.example" && SameOriginURL(r, cfg.Console.Auth.OIDC.RedirectURL) {
				t.Fatal("strict public-origin helper accepted another listener host")
			}
		})
	}
}

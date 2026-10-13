package sso

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestHTTPSCookieSchemeIsCaseInsensitive(t *testing.T) {
	if err := config.SecureURL("redirect", "HTTPS://console.example/_tinystack/auth/callback"); err != nil {
		t.Fatal(err)
	}

	s, err := New(&config.OIDC{RedirectURL: "HTTPS://console.example/_tinystack/auth/callback"}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{txCookie, sessionCookie} {
		w := httptest.NewRecorder()
		s.cookie(w, name, "opaque", time.Minute, http.SameSiteStrictMode)
		cookie := w.Result().Cookies()[0]
		if !cookie.Secure || cookie.Name != "__Host-"+name || cookie.Path != "/" {
			t.Fatalf("HTTPS scheme bypassed host-bound cookie protections: %s", cookie.String())
		}
	}
}

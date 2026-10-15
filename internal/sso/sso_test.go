package sso

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestGroups(t *testing.T) {
	claims := map[string]any{
		"groups":     []any{"a", 7, "b"},
		"single":     "solo",
		"https://x/": map[string]any{"ignored": true},
		"realm_access": map[string]any{
			"roles": []any{"admin"},
		},
		"https://example.com/roles": []any{"namespaced"},
		"number":                    5,
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"viewer": map[string]any{"123": "org.example"}, "admin": map[string]any{},
		},
	}
	for path, want := range map[string]string{
		"groups":                            "a,b",
		"single":                            "solo",
		"realm_access.roles":                "admin",
		"https://example.com/roles":         "namespaced",
		"number":                            "",
		"urn:zitadel:iam:org:project:roles": "admin,viewer",
		"missing":                           "",
		"realm_access.missing":              "",
		"single.deeper":                     "",
	} {
		if got := strings.Join(groups(claims, path), ","); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestSessionName(t *testing.T) {
	for _, tc := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"email": "ada@example.com", "preferred_username": "ada"}, "ada@example.com"},
		{map[string]any{"preferred_username": "ada lovelace", "sub": "1"}, "ada-lovelace"},
		{map[string]any{"email": "a", "sub": "user|123/45"}, "user-123-45"},
		{map[string]any{"email": strings.Repeat("x", 100) + "@e.com"}, strings.Repeat("x", 64)},
		{map[string]any{}, "sso-user"},
	} {
		if got := sessionName(tc.claims); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.claims, got, tc.want)
		}
	}
}

func TestStoreIsSingleUseAndBounded(t *testing.T) {
	s := newStore[string](time.Minute, 2)
	now := time.Now()
	s.now = func() time.Time { return now }
	first, ok := s.put("one")
	second, _ := s.put("two")
	if _, full := s.put("three"); !ok || full {
		t.Fatal("a full store takes no more")
	}

	if v, found := s.take(first); !found || v != "one" {
		t.Fatal("take")
	}

	if _, found := s.take(first); found {
		t.Fatal("a value is taken once")
	}

	now = now.Add(time.Minute)
	if _, found := s.take(second); found {
		t.Fatal("expired values are gone")
	}

	if _, found := s.take("unknown"); found {
		t.Fatal("unknown id")
	}
}

func TestLoginTransactionsAreLimitedPerSource(t *testing.T) {
	s := &Service{txs: newStore[transaction](txTTL, pending), discovered: &discovery{
		oauth: &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://issuer.example/auth"}},
	}}
	for i := range 64 {
		r := httptest.NewRequest(http.MethodGet, "http://localhost"+Path+"login", http.NoBody)
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		s.login(w, r)
		want := http.StatusFound
		if i >= 32 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d = %d, want %d", i, w.Code, want)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://localhost"+Path+"login", http.NoBody)
	r.RemoteAddr = "192.0.2.2:1234"
	w := httptest.NewRecorder()
	s.login(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("another person could not sign in: %d", w.Code)
	}
}

func TestSessionOriginUsesConfiguredHTTPSOriginBehindProxy(t *testing.T) {
	s := &Service{
		cfg:    config.OIDC{RedirectURL: "https://console.example/_tinystack/auth/callback"},
		secure: true, sessions: newStore[signedIn](0, pending),
	}
	id, _ := s.sessions.putUntil(
		signedIn{AccessKeyID: "ASIA", Role: "arn:aws:iam::000000000000:role/Admin"},
		time.Now().Add(time.Hour),
	)
	for _, origin := range []string{"https://console.example", "https://evil.example", ""} {
		r := httptest.NewRequest(http.MethodPost, "http://console.example"+Path+"session", http.NoBody)
		r.AddCookie(&http.Cookie{Name: "__Host-tinystack_sso_session", Value: id})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		s.session(w, r)
		want := http.StatusForbidden
		if origin == "https://console.example" {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("origin=%q status=%d, want %d", origin, w.Code, want)
		}
	}
}

func TestHTTPSCookiesAreHostBound(t *testing.T) {
	s := &Service{secure: true}
	for _, name := range []string{txCookie, sessionCookie} {
		w := httptest.NewRecorder()
		s.cookie(w, name, "opaque", time.Minute, http.SameSiteStrictMode)
		cookie := w.Result().Cookies()[0]
		if cookie.Name != "__Host-"+name || cookie.Path != "/" || cookie.Domain != "" || !cookie.Secure ||
			!cookie.HttpOnly {
			t.Fatalf("cookie is not host bound: %+v", cookie)
		}
	}
}

func TestCodeExchangeErrorsDoNotExposeProviderBodies(t *testing.T) {
	const secret = "access-token-from-provider"
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"` + secret + `"}`))
	}))
	defer issuer.Close()

	s := &Service{}
	d := &discovery{oauth: &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: issuer.URL}}}
	_, err := s.identify(context.Background(), d, "code", &transaction{verifier: "verifier"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("provider response exposed in loggable error: %v", err)
	}
}

func TestDeviceReplayStoreBoundsEachSubject(t *testing.T) {
	s := newStore[struct{}](maxIDTokenAge, pending)
	for i := range 33 {
		accepted := s.markOwned(fmt.Sprint(i), "attacker", 32)
		if accepted != (i < 32) {
			t.Fatalf("redemption %d accepted=%v", i, accepted)
		}
	}
	if !s.markOwned("other", "victim", 32) {
		t.Fatal("another subject was locked out")
	}

	if s.markOwned("other", "victim", 32) {
		t.Fatal("replay was accepted")
	}
}

func TestAllowedRolesAreDeduplicatedInConfigOrder(t *testing.T) {
	s := &Service{cfg: config.OIDC{Claim: "groups", Roles: config.OIDCRoles{
		{Group: "a", Role: "arn:aws:iam::000000000000:role/Admin", Admin: true},
		{Group: "b", Role: "arn:aws:iam::111111111111:role/Developer"},
		{Group: "b", Role: "arn:aws:iam::000000000000:role/Admin", Admin: true},
	}}}
	roles := s.allowedRoles(map[string]any{"groups": []any{"b", "a"}})
	if len(roles) != 2 || roles[0].Name != "Admin" || roles[1].AccountID != "111111111111" {
		t.Fatalf("unexpected allowed roles: %+v", roles)
	}
}

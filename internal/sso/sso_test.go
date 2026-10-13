package sso

import (
	"strings"
	"testing"
	"time"
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

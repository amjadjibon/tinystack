package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oidcYAML = `
identity:
  mode: verify
  secret_keys: {root: secret}
console:
  auth:
    oidc:
      issuer: https://idp.example.com/realms/tinystack
      client_id: tinystack-console
      client_secret_file: /run/secrets/oidc
      redirect_url: https://console.example.com/_tinystack/auth/callback
      claim: groups
      roles:
        team-b-dev:      { role: "arn:aws:iam::111111111111:role/Developer" }
        platform-admins: { role: "arn:aws:iam::000000000000:role/Admin", admin: true }
        auditors:        { role: "arn:aws:iam::000000000000:role/team/Auditor" }
`

func load(t *testing.T, yaml string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tinystack.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	return Load(path)
}

func TestOIDCLoadsInOrderWithDefaults(t *testing.T) {
	cfg, err := load(t, oidcYAML)
	if err != nil {
		t.Fatal(err)
	}

	oidc := cfg.Console.Auth.OIDC
	if oidc == nil || oidc.SessionSeconds != DefaultSessionSeconds ||
		strings.Join(oidc.Scopes, " ") != "profile email" {
		t.Fatalf("defaults: %+v", oidc)
	}

	var order []string
	for _, r := range oidc.Roles {
		order = append(order, r.Group+"="+r.Account())
	}

	if got := strings.Join(
		order,
		",",
	); got != "team-b-dev=111111111111,platform-admins=000000000000,auditors=000000000000" {
		t.Fatalf("roles keep the file's order: %s", got)
	}

	if admins := oidc.AdminRoles(); len(admins) != 1 || !admins["arn:aws:iam::000000000000:role/Admin"] {
		t.Fatalf("admins: %v", admins)
	}

	if cfg, err = load(t, "server: {port: 4566}\n"); err != nil || cfg.Console.Auth.OIDC != nil {
		t.Fatalf("SSO is off unless configured: %v", err)
	}
}

func TestOIDCValidation(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"http issuer":          {"issuer: https://idp.example.com", "issuer: http://idp.example.com", "must use https"},
		"loopback http issuer": {"issuer: https://idp.example.com/realms/tinystack", "issuer: http://127.0.0.1:9091", ""},
		"localhost issuer":     {"issuer: https://idp.example.com/realms/tinystack", "issuer: http://localhost:9091", ""},
		"issuer credentials": {
			"issuer: https://idp.example.com", "issuer: https://user:pass@idp.example.com", "without credentials",
		},
		"relative redirect": {
			"redirect_url: https://console.example.com/_tinystack/auth/callback", "redirect_url: /_tinystack/auth/callback",
			"absolute URL",
		},
		"redirect elsewhere": {"/_tinystack/auth/callback", "/callback", "must end with"},
		"http redirect": {
			"redirect_url: https://console.example.com", "redirect_url: http://console.example.com", "must use https",
		},
		"no client":     {"client_id: tinystack-console", "client_id: ''", "client_id is required"},
		"empty claim":   {"claim: groups", "claim: a..b", "claim must be"},
		"short session": {"claim: groups", "claim: groups\n      session_seconds: 60", "session_seconds"},
		"long session":  {"claim: groups", "claim: groups\n      session_seconds: 90000", "session_seconds"},
		"bad role":      {"arn:aws:iam::111111111111:role/Developer", "arn:aws:iam::1:role/Developer", "not a role ARN"},
		"user ARN":      {"arn:aws:iam::111111111111:role/Developer", "arn:aws:iam::111111111111:user/dev", "not a role ARN"},
		"conflicting admin": {
			"auditors:        { role: \"arn:aws:iam::000000000000:role/team/Auditor\" }",
			"auditors:        { role: \"arn:aws:iam::000000000000:role/Admin\" }", "both an admin and a non-admin",
		},
		"unknown role setting": {
			"{ role: \"arn:aws:iam::111111111111:role/Developer\" }",
			"{ role: \"arn:aws:iam::111111111111:role/Developer\", adminn: true }", "unknown setting",
		},
		"duplicate group": {"auditors:  ", "team-b-dev:", "mapped twice"},
		"parse mode":      {"mode: verify", "mode: parse", "requires identity.mode: verify"},
		"no console":      {"console:\n", "server: {console_port: 0}\nconsole:\n", "requires server.console_port"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, strings.Replace(oidcYAML, tc.from, tc.to, 1))
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOIDCRejectsInlineSecret(t *testing.T) {
	_, err := load(t, strings.Replace(oidcYAML, "client_secret_file:", "client_secret:", 1))
	if err == nil || !strings.Contains(err.Error(), "client_secret") {
		t.Fatalf("an inline secret is not a setting: %v", err)
	}
}

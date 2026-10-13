package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// CallbackPath is where the console's sign-in returns from the identity
// provider; the configured redirect_url must end with it.
const CallbackPath = "/_tinystack/auth/callback"

// Bounds on the length of an SSO session, in seconds: STS's own limits.
const (
	MinSessionSeconds     = 900
	MaxSessionSeconds     = 43200
	DefaultSessionSeconds = 3600
)

var roleARN = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/(?:[\w+=,.@-]+/)*[\w+=,.@-]{1,64}$`)

// OIDC configures single sign-on to the console through an OpenID Connect
// provider. The groups the provider reports select the IAM role whose
// temporary credentials the signed-in person gets.
type OIDC struct {
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// ClientSecretFile names a file holding the client secret. A public client
	// that relies on PKCE alone leaves it empty.
	ClientSecretFile string `yaml:"client_secret_file"`
	RedirectURL      string `yaml:"redirect_url"`
	// Claim is the ID token claim that holds the group names: a string or a
	// list of strings, reached through dots for nested objects.
	Claim string `yaml:"claim"`
	// Scopes are requested besides openid.
	Scopes []string `yaml:"scopes"`
	// Roles maps groups to roles, in the order the file lists them; the first
	// group a person belongs to wins.
	Roles OIDCRoles `yaml:"roles"`
	// SessionSeconds is how long the issued credentials last.
	SessionSeconds int `yaml:"session_seconds"`
}

// OIDCRole maps one group to a role.
type OIDCRole struct {
	Group string
	// Role is the ARN of the role; its account is the person's account.
	Role string
	// Admin lets the person use the administrative endpoints.
	Admin bool
}

// Account is the account of the role's ARN.
func (r OIDCRole) Account() string {
	m := roleARN.FindStringSubmatch(r.Role)
	if m == nil {
		return ""
	}

	return m[1]
}

// OIDCRoles are the group mappings in the order of the configuration file.
type OIDCRoles []OIDCRole

// UnmarshalYAML keeps the order of the mapping, which a Go map would lose.
func (r *OIDCRoles) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: roles must map group names to roles", node.Line)
	}

	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if seen[key.Value] {
			return fmt.Errorf("line %d: group %q is mapped twice", key.Line, key.Value)
		}

		seen[key.Value] = true
		var entry struct {
			Role  string `yaml:"role"`
			Admin bool   `yaml:"admin"`
		}
		if value.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: group %q needs role and admin settings", value.Line, key.Value)
		}

		for j := 0; j < len(value.Content); j += 2 {
			if name := value.Content[j].Value; name != "role" && name != "admin" {
				return fmt.Errorf("line %d: unknown setting %q for group %q", value.Content[j].Line, name, key.Value)
			}
		}

		if err := value.Decode(&entry); err != nil {
			return err
		}

		*r = append(*r, OIDCRole{Group: key.Value, Role: entry.Role, Admin: entry.Admin})
	}

	return nil
}

// applyDefaults fills in what a configuration file may leave out.
func (o *OIDC) applyDefaults() {
	if o.SessionSeconds == 0 {
		o.SessionSeconds = DefaultSessionSeconds
	}

	if len(o.Scopes) == 0 {
		o.Scopes = []string{"profile", "email"}
	}
}

// AdminRoles returns the ARNs of the roles that administrators sign in with.
func (o *OIDC) AdminRoles() map[string]bool {
	admins := map[string]bool{}
	for _, r := range o.Roles {
		if r.Admin {
			admins[r.Role] = true
		}
	}

	return admins
}

func (o *OIDC) validate() error {
	if err := SecureURL("console.auth.oidc.issuer", o.Issuer); err != nil {
		return err
	}

	if u, _ := url.Parse(o.Issuer); u.RawQuery != "" {
		return fmt.Errorf("console.auth.oidc.issuer must not have a query")
	}

	if err := SecureURL("console.auth.oidc.redirect_url", o.RedirectURL); err != nil {
		return err
	}

	if u, _ := url.Parse(o.RedirectURL); u.Path != CallbackPath || u.RawQuery != "" {
		return fmt.Errorf("console.auth.oidc.redirect_url must end with %s", CallbackPath)
	}

	if o.ClientID == "" {
		return fmt.Errorf("console.auth.oidc.client_id is required")
	}

	if o.Claim == "" || slices.Contains(strings.Split(o.Claim, "."), "") {
		return fmt.Errorf("console.auth.oidc.claim must be a claim name, with dots for nested claims")
	}

	if o.SessionSeconds < MinSessionSeconds || o.SessionSeconds > MaxSessionSeconds {
		return fmt.Errorf("console.auth.oidc.session_seconds must be between %d and %d",
			MinSessionSeconds, MaxSessionSeconds)
	}

	for _, scope := range o.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n\"\\") {
			return fmt.Errorf("console.auth.oidc.scopes must be names without spaces")
		}
	}

	if len(o.Roles) == 0 {
		return fmt.Errorf("console.auth.oidc.roles must map at least one group")
	}

	admin := map[string]bool{}
	for _, r := range o.Roles {
		if r.Group == "" {
			return fmt.Errorf("console.auth.oidc.roles has an empty group name")
		}

		if r.Account() == "" {
			return fmt.Errorf(
				"console.auth.oidc.roles: %q is not a role ARN (arn:aws:iam::<account>:role/<name>)",
				r.Role,
			)
		}

		if previous, ok := admin[r.Role]; ok && previous != r.Admin {
			return fmt.Errorf("console.auth.oidc.roles: %s is both an admin and a non-admin role", r.Role)
		}

		admin[r.Role] = r.Admin
	}

	return nil
}

// SecureURL requires an absolute https URL, or http for a loopback host.
func SecureURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%s must be an absolute URL without credentials or fragment", name)
	}

	if u.Scheme == "https" || (u.Scheme == "http" && LoopbackHost(u.Hostname())) {
		return nil
	}

	return fmt.Errorf("%s must use https (http only for localhost or a loopback address)", name)
}

// LoopbackHost reports whether host is localhost or a loopback address.
func LoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

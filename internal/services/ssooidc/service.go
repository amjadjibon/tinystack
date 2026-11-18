// Package ssooidc implements IAM Identity Center's OIDC service: public clients register, then sign a user in with
// the device authorization grant or the authorization code grant with PKCE, and receive access and refresh tokens
// for the access portal (package sso). Users approve sign-ins on TinyStack's local pages by Identity Store user
// name; there are no passwords.
//
// Records are partition-wide, like the tokens and start URLs that name them: aws/ssooidc/<kind>/<id>, where codes
// and tokens are stored under their SHA-256 hash.
package ssooidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssooidc"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	prefix = "aws/ssooidc/"

	kindClient        = "client"
	kindDevice        = "device"
	kindUserCode      = "user-code"
	kindAuthorization = "authorization"
	kindCode          = "code"
	kindToken         = "token"

	grantDevice  = "urn:ietf:params:oauth:grant-type:device_code"
	grantCode    = "authorization_code"
	grantRefresh = "refresh_token"
	scopeAccess  = "sso:account:access"

	clientLifetime = 90 * 24 * time.Hour
	deviceLifetime = 10 * time.Minute
	codeLifetime   = 5 * time.Minute
	// A client registered with scopes gets short access tokens it refreshes for the rest of the session; one
	// registered without scopes gets one long access token and no refresh token, as AWS issues them.
	scopedTokenLifetime   = time.Hour
	unscopedTokenLifetime = 8 * time.Hour
	sessionLifetime       = 8 * time.Hour

	// DevicePath is the local page where a user approves a device sign-in.
	DevicePath = "/_tinystack/sso/device"
	// AuthorizePath is the authorization endpoint; SDKs build it from the OIDC endpoint.
	AuthorizePath = "/authorize"
	// ApprovePath receives the authorization code sign-in form.
	ApprovePath = "/_tinystack/sso/authorize"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service { return &Service{store: store, now: time.Now} }

type client struct {
	Name         string
	SecretHash   string
	IssuerURL    string   `json:",omitempty"`
	Scopes       []string `json:",omitempty"`
	RedirectURIs []string `json:",omitempty"`
	GrantTypes   []string
	Expires      int64
}

func (c *client) allows(grant string) bool { return slices.Contains(c.GrantTypes, grant) }

// device is a pending device authorization, stored under its device code's hash.
type device struct {
	ClientID string
	UserCode string
	Account  string
	UserID   string `json:",omitempty"`
	Expires  int64
}

// userCodeRef finds a pending device authorization by the code the user types.
type userCodeRef struct {
	Device  string
	Expires int64
}

// authorization is a browser sign-in between /authorize and the user's approval, stored under its transaction ID.
type authorization struct {
	ClientID    string
	RedirectURI string
	State       string `json:",omitempty"`
	Challenge   string
	Account     string
	Scopes      []string `json:",omitempty"`
	Expires     int64
}

// code is an approved authorization code, stored under its hash.
type code struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Account     string
	UserID      string
	Scopes      []string `json:",omitempty"`
	Expires     int64
}

// token is an access or refresh token, stored under its hash. The tokens of one sign-in share its SessionID, and
// SessionEnds is when it ends: refreshing never extends it.
type token struct {
	ClientID    string
	Account     string
	UserID      string
	SessionID   string
	Scopes      []string `json:",omitempty"`
	Refresh     bool     `json:",omitempty"`
	Expires     int64
	SessionEnds int64
}

func key(kind, id string) string { return prefix + kind + "/" + id }

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// oauthError is an OIDC error: its shape carries the OAuth error code and description.
func oauthError(shape, code, description string) error {
	return &awserror.Error{
		Code: shape, Message: description, StatusCode: 400,
		Fields: map[string]any{"error": code, "error_description": description},
	}
}

func invalidRequest(description string) error {
	return oauthError("InvalidRequestException", "invalid_request", description)
}

func invalidClient() error {
	return oauthError("InvalidClientException", "invalid_client", "Invalid client credentials")
}

func invalidGrant(description string) error {
	return oauthError("InvalidGrantException", "invalid_grant", description)
}

// read returns a record of kind that has not expired.
func read[T any](tx storage.ReadTx, now time.Time, kind, id string, expires func(*T) int64) (*T, error) {
	r, found, err := storage.Read[T](tx, key(kind, id))
	if err != nil || !found || expires(r) <= now.Unix() {
		return nil, err
	}

	return r, nil
}

// loadClient checks a client's ID and secret.
func loadClient(tx storage.ReadTx, now time.Time, id, secret *string) (*client, error) {
	if id == nil || secret == nil {
		return nil, invalidClient()
	}

	c, err := read(tx, now, kindClient, *id, func(c *client) int64 { return c.Expires })
	switch {
	case err != nil:
		return nil, err
	case c == nil || c.SecretHash != hash(*secret):
		return nil, invalidClient()
	}

	return c, nil
}

// prune removes expired records; every write runs it, so pending sign-ins and old tokens do not accumulate.
func prune(tx storage.WriteTx, now time.Time) error {
	for _, k := range tx.Keys(prefix) {
		data, _ := tx.Get(k)
		var r struct{ Expires int64 }
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}

		if r.Expires <= now.Unix() {
			tx.Delete(k)
		}
	}

	return nil
}

// Session is who an access token signed in.
type Session struct {
	Account  string
	UserID   string
	UserName string
}

// Session returns the Identity Center user an unexpired access token belongs to; ok is false for an unknown or
// expired token, or one whose user has been deleted.
func (s *Service) Session(tx storage.ReadTx, accessToken string) (Session, bool, error) {
	t, err := read(tx, s.now(), kindToken, hash(accessToken), func(t *token) int64 { return t.Expires })
	if err != nil || t == nil || t.Refresh {
		return Session{}, false, err
	}

	u, found, err := identitystore.UserByID(tx, t.Account, t.UserID)
	if err != nil || !found {
		return Session{}, false, err
	}

	return Session{Account: t.Account, UserID: t.UserID, UserName: *u.UserName}, true, nil
}

// Revoke ends the sign-in an access token belongs to, removing its access and refresh tokens.
func (s *Service) Revoke(tx storage.WriteTx, accessToken string) error {
	t, err := read(tx, s.now(), kindToken, hash(accessToken), func(t *token) int64 { return t.Expires })
	if err != nil || t == nil {
		return err
	}

	for _, k := range tx.Keys(prefix + kindToken + "/") {
		r, _, readErr := storage.Read[token](tx, k)
		if readErr != nil {
			return readErr
		}

		if r.SessionID == t.SessionID {
			tx.Delete(k)
		}
	}

	return nil
}

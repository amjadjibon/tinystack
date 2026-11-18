package ssooidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssooidc"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// userCodeAlphabet leaves out vowels and look-alike letters, as device codes typed by hand do.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

func (s *Service) RegisterClient(
	ctx context.Context,
	in *api.RegisterClientRequest,
) (*api.RegisterClientResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.ClientName) == "" || common.Value(in.ClientType) != "public" {
		return nil, oauthError("InvalidClientMetadataException", "invalid_client_metadata",
			"clientName is required and clientType must be public")
	}

	c := &client{Name: *in.ClientName, IssuerURL: common.Value(in.IssuerUrl), GrantTypes: in.GrantTypes}
	if len(c.GrantTypes) == 0 {
		c.GrantTypes = []string{grantDevice, grantRefresh}
	}

	for _, grant := range c.GrantTypes {
		if grant != grantDevice && grant != grantCode && grant != grantRefresh {
			return nil, oauthError("InvalidClientMetadataException", "invalid_client_metadata",
				"Unsupported grant type "+grant)
		}
	}

	for _, scope := range in.Scopes {
		if scope != scopeAccess {
			return nil, oauthError("InvalidScopeException", "invalid_scope", "Unsupported scope "+scope)
		}
	}

	c.Scopes = slices.Compact(slices.Sorted(slices.Values(in.Scopes)))
	for _, uri := range in.RedirectUris {
		if !loopback(uri) {
			return nil, oauthError("InvalidRedirectUriException", "invalid_redirect_uri",
				"Redirect URIs must be http loopback addresses: "+uri)
		}
	}

	c.RedirectURIs = in.RedirectUris
	if c.allows(grantCode) && len(c.RedirectURIs) == 0 {
		return nil, oauthError("InvalidRedirectUriException", "invalid_redirect_uri",
			"The authorization code grant needs a redirect URI")
	}

	if c.IssuerURL != "" {
		if u, parseErr := url.Parse(c.IssuerURL); parseErr != nil || u.Scheme != "https" || u.Host == "" {
			return nil, invalidRequest("issuerUrl must be an https URL")
		}
	}

	now := s.now()
	id, secret := random(16), random(48)
	c.SecretHash, c.Expires = hash(secret), now.Add(clientLifetime).Unix()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if pruneErr := prune(tx, now); pruneErr != nil {
			return pruneErr
		}

		return storage.Write(tx, key(kindClient, id), c)
	})
	if err != nil {
		return nil, err
	}

	out := &api.RegisterClientResponse{
		ClientId: new(id), ClientSecret: new(secret), ClientIdIssuedAt: new(now.Unix()),
		ClientSecretExpiresAt: new(c.Expires),
	}
	if c.allows(grantCode) {
		out.AuthorizationEndpoint, out.TokenEndpoint = new(m.Endpoint+AuthorizePath), new(m.Endpoint+"/token")
	}

	return out, nil
}

// loopback reports whether a redirect URI is a local HTTP address, the only kind a public client may use.
func loopback(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return false
	}

	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (s *Service) StartDeviceAuthorization(
	ctx context.Context,
	in *api.StartDeviceAuthorizationRequest,
) (*api.StartDeviceAuthorizationResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	deviceCode := random(48)
	var userCode string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if pruneErr := prune(tx, now); pruneErr != nil {
			return pruneErr
		}

		c, clientErr := loadClient(tx, now, in.ClientId, in.ClientSecret)
		if clientErr != nil {
			return clientErr
		}

		if !c.allows(grantDevice) {
			return oauthError("UnauthorizedClientException", "unauthorized_client",
				"The client is not registered for the device code grant")
		}

		account, found := identitystore.StartURLAccount(tx, common.Value(in.StartUrl))
		if !found {
			return invalidRequest("Unknown start URL; create an Identity Store user to enable its access portal")
		}

		for {
			id := common.RandomID(8, userCodeAlphabet)
			userCode = id[:4] + "-" + id[4:]
			if _, taken := tx.Get(key(kindUserCode, userCode)); !taken {
				break
			}
		}

		d := &device{
			ClientID: *in.ClientId, UserCode: userCode, Account: account, Expires: now.Add(deviceLifetime).Unix(),
		}
		if writeErr := storage.Write(tx, key(kindDevice, hash(deviceCode)), d); writeErr != nil {
			return writeErr
		}

		return storage.Write(
			tx,
			key(kindUserCode, userCode),
			&userCodeRef{Device: hash(deviceCode), Expires: d.Expires},
		)
	})
	if err != nil {
		return nil, err
	}

	verification := m.Endpoint + DevicePath
	return &api.StartDeviceAuthorizationResponse{
		DeviceCode: new(deviceCode), UserCode: new(userCode), VerificationUri: new(verification),
		VerificationUriComplete: new(verification + "?user_code=" + userCode),
		ExpiresIn:               new(int64(deviceLifetime / time.Second)), Interval: new(int64(1)),
	}, nil
}

func (s *Service) CreateToken(ctx context.Context, in *api.CreateTokenRequest) (*api.CreateTokenResponse, error) {
	now := s.now()
	var out *api.CreateTokenResponse
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if pruneErr := prune(tx, now); pruneErr != nil {
			return pruneErr
		}

		c, err := loadClient(tx, now, in.ClientId, in.ClientSecret)
		if err != nil {
			return err
		}

		grant := common.Value(in.GrantType)
		switch {
		case grant != grantDevice && grant != grantCode && grant != grantRefresh:
			return oauthError("UnsupportedGrantTypeException", "unsupported_grant_type",
				"Unsupported grant type "+grant)
		case !c.allows(grant):
			return oauthError("UnauthorizedClientException", "unauthorized_client",
				"The client is not registered for the "+grant+" grant")
		}

		for _, scope := range in.Scope {
			if !slices.Contains(c.Scopes, scope) {
				return oauthError("InvalidScopeException", "invalid_scope", "Scope not registered: "+scope)
			}
		}

		t := &token{ClientID: *in.ClientId, Scopes: c.Scopes, SessionEnds: now.Add(sessionLifetime).Unix()}
		if len(in.Scope) > 0 {
			t.Scopes = in.Scope
		}

		switch grant {
		case grantDevice:
			err = redeemDevice(tx, now, t, common.Value(in.DeviceCode))
		case grantCode:
			err = redeemCode(tx, now, t, in)
		default:
			err = redeemRefresh(tx, now, t, common.Value(in.RefreshToken))
		}

		if err != nil {
			return err
		}

		out, err = issue(tx, now, t)
		return err
	})

	return out, err
}

func redeemDevice(tx storage.WriteTx, now time.Time, t *token, deviceCode string) error {
	k := key(kindDevice, hash(deviceCode))
	d, found, err := storage.Read[device](tx, k)
	switch {
	case err != nil:
		return err
	case !found || d.ClientID != t.ClientID:
		return invalidGrant("Invalid device code")
	case d.Expires <= now.Unix():
		return oauthError("ExpiredTokenException", "expired_token", "The device code has expired")
	case d.UserID == "":
		return oauthError("AuthorizationPendingException", "authorization_pending",
			"The user has not approved the sign-in yet")
	}

	tx.Delete(k)
	tx.Delete(key(kindUserCode, d.UserCode))
	t.Account, t.UserID = d.Account, d.UserID
	return nil
}

func redeemCode(tx storage.WriteTx, now time.Time, t *token, in *api.CreateTokenRequest) error {
	k := key(kindCode, hash(common.Value(in.Code)))
	c, found, err := storage.Read[code](tx, k)
	if err != nil {
		return err
	}

	if !found || c.ClientID != t.ClientID || c.Expires <= now.Unix() {
		return invalidGrant("Invalid authorization code")
	}

	// The code is spent whether or not the rest checks out.
	tx.Delete(k)
	sum := sha256.Sum256([]byte(common.Value(in.CodeVerifier)))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	switch {
	case common.Value(in.RedirectUri) != c.RedirectURI:
		return invalidGrant("The redirect URI does not match the authorization request")
	case subtle.ConstantTimeCompare([]byte(challenge), []byte(c.Challenge)) != 1:
		return invalidGrant("The code verifier does not match the code challenge")
	}

	t.Account, t.UserID = c.Account, c.UserID
	if len(in.Scope) == 0 && len(c.Scopes) > 0 {
		t.Scopes = c.Scopes
	}

	return nil
}

func redeemRefresh(tx storage.WriteTx, now time.Time, t *token, refreshToken string) error {
	k := key(kindToken, hash(refreshToken))
	r, found, err := storage.Read[token](tx, k)
	switch {
	case err != nil:
		return err
	case !found || !r.Refresh || r.ClientID != t.ClientID:
		return invalidGrant("Invalid refresh token")
	case r.Expires <= now.Unix():
		return oauthError("ExpiredTokenException", "expired_token", "The refresh token has expired")
	}

	// Refresh tokens rotate: the old one is spent, and the session keeps its end.
	tx.Delete(k)
	t.Account, t.UserID, t.SessionID, t.SessionEnds = r.Account, r.UserID, r.SessionID, r.SessionEnds
	if len(t.Scopes) == 0 {
		t.Scopes = r.Scopes
	}

	return nil
}

// issue stores a new access token, and a refresh token when the sign-in has scopes, for the sign-in t describes.
func issue(tx storage.WriteTx, now time.Time, t *token) (*api.CreateTokenResponse, error) {
	_, found, err := identitystore.UserByID(tx, t.Account, t.UserID)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, invalidGrant("The user no longer exists")
	}

	if t.SessionID == "" {
		t.SessionID = random(16)
	}

	lifetime := unscopedTokenLifetime
	if len(t.Scopes) > 0 {
		lifetime = scopedTokenLifetime
	}

	access := *t
	access.Expires = min(now.Add(lifetime).Unix(), t.SessionEnds)
	accessToken := "aoa" + random(48)
	if err := storage.Write(tx, key(kindToken, hash(accessToken)), &access); err != nil {
		return nil, err
	}

	out := &api.CreateTokenResponse{
		AccessToken: new(accessToken), TokenType: new("Bearer"), ExpiresIn: new(access.Expires - now.Unix()),
	}
	if len(t.Scopes) > 0 {
		refresh := *t
		refresh.Refresh, refresh.Expires = true, t.SessionEnds
		refreshToken := "aor" + random(48)
		if err := storage.Write(tx, key(kindToken, hash(refreshToken)), &refresh); err != nil {
			return nil, err
		}

		out.RefreshToken = new(refreshToken)
	}

	return out, nil
}

// scopes reads an authorization request's scopes, which SDKs send space- or comma-separated.
func scopes(query url.Values) []string {
	var out []string
	for _, name := range []string{"scopes", "scope"} {
		for _, value := range query[name] {
			out = append(out, strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == ',' })...)
		}
	}

	return slices.Compact(slices.Sorted(slices.Values(out)))
}

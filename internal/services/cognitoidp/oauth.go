package cognitoidp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// invalidClient is the OAuth error for credentials that do not match a client.
const (
	responseToken  = "token"
	oauthTokenType = "token_type"
	providerJWKS   = "jwks_uri"

	invalidClient      = "invalid_client"
	allClients         = "ALL"
	scopeProfile       = "profile"
	flowCode           = "code"
	flowImplicit       = "implicit"
	promptNone         = "none"
	providerOIDC       = "OIDC"
	providerCognito    = "COGNITO"
	invalidRedirectURI = "invalid_redirect_uri"
	unauthorizedClient = "unauthorized_client"
	invalidScope       = "invalid_scope"
	invalidRequest     = "invalid_request"
	invalidGrant       = "invalid_grant"
	pkcePlain          = "plain"
	pkceS256           = "S256"
	claimClientID      = "client_id"
	oauthAccessToken   = "access_token"
	bearerTokenType    = "Bearer"
	grantRefreshToken  = "refresh_token"
	oauthExpiresIn     = "expires_in"
	claimScope         = "scope"
	attrName           = "name"
	attrGivenName      = "given_name"
	attrFamilyName     = "family_name"
	attrBirthdate      = "birthdate"
)

// Token serves a pool's OAuth 2.0 token endpoint, POST
// <issuer>/oauth2/token, for the client credentials grant: an app client
// with a secret, allowed the client_credentials flow, trades its credentials
// (HTTP Basic or client_id and client_secret form fields) for an access token
// with the custom scopes it asks for, or all it is allowed. It returns the
// HTTP status and the JSON body, which is an OAuth error on failure, and an
// error when the store fails.
func (s *Service) Token(
	ctx context.Context, poolID, endpoint, authorization string, form url.Values,
) (status int, body []byte, err error) {
	clientID, secret := form.Get(claimClientID), form.Get("client_secret")
	if user, password, ok := basicAuth(authorization); ok {
		clientID, secret = user, password
	}

	if authorization != "" {
		if _, _, ok := basicAuth(authorization); !ok {
			status, body = oauthError(http.StatusUnauthorized, invalidClient)
			return status, body, nil
		}
	}

	if form.Get("grant_type") == "authorization_code" || form.Get("grant_type") == grantRefreshToken {
		return s.userToken(ctx, poolID, endpoint, clientID, secret, form)
	}

	switch {
	case form.Get("grant_type") == "", clientID == "":
		status, body = oauthError(http.StatusBadRequest, invalidRequest)
		return status, body, nil
	case form.Get("grant_type") != flowClientCredentials:
		status, body = oauthError(http.StatusBadRequest, "unsupported_grant_type")
		return status, body, nil
	}

	var token string
	var expiresIn int64
	var failure string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		index, found, readErr := storage.Read[clientIndex](tx, clientIndexKey(clientID))
		if readErr != nil || !found || index.Pool != poolID {
			failure = invalidClient
			return readErr
		}

		scope := request.Scope{AccountID: index.Account, Region: index.Region}
		p, _, readErr := storage.Read[pool](tx, poolKey(scope, poolID))
		if readErr != nil {
			return readErr
		}

		c, _, readErr := storage.Read[client](tx, clientKey(scope, poolID, clientID))
		switch {
		case readErr != nil:
			return readErr
		case c == nil || p == nil || c.Secret == "" || !c.acceptsSecret(secret):
			failure = invalidClient
			return nil
		case !common.Value(c.Settings.AllowedOAuthFlowsUserPoolClient) ||
			!slices.Contains(c.Settings.AllowedOAuthFlows, flowClientCredentials):
			failure = unauthorizedClient
			return nil
		}

		scopes := c.Settings.AllowedOAuthScopes
		if requested := strings.Fields(form.Get(claimScope)); len(requested) > 0 {
			for _, sc := range requested {
				if !slices.Contains(scopes, sc) {
					failure = invalidScope
					return nil
				}
			}

			scopes = requested
		}

		key, created, keyErr := signingKey(p)
		if keyErr != nil {
			return keyErr
		}

		if created {
			if writeErr := storage.Write(tx, poolKey(scope, poolID), p); writeErr != nil {
				return writeErr
			}
		}

		now := s.now()
		validity := c.Settings.validity(tokenAccess)
		expiresIn = int64(validity / time.Second)
		token, keyErr = sign(key, map[string]any{
			attrSub: clientID, claimClientID: clientID, claimTokenUse: "access", claimScope: strings.Join(scopes, " "),
			claimAuthTime: now.Unix(), claimIssuedAt: now.Unix(), claimExpires: now.Add(validity).Unix(), "version": 2,
			claimJTI: newSub(), claimIssuer: strings.TrimSuffix(endpoint, "/") + "/" + poolID,
		})
		return keyErr
	})
	switch {
	case err != nil:
		status, body = oauthError(http.StatusInternalServerError, "server_error")
		return status, body, err
	case failure == invalidClient:
		status, body = oauthError(http.StatusUnauthorized, failure)
		return status, body, nil
	case failure != "":
		status, body = oauthError(http.StatusBadRequest, failure)
		return status, body, nil
	}

	body, err = json.Marshal(
		map[string]any{oauthAccessToken: token, oauthExpiresIn: expiresIn, oauthTokenType: bearerTokenType},
	)
	return http.StatusOK, body, err
}

func oauthError(status int, code string) (int, []byte) { //nolint:gocritic // The status and body to write.
	body, _ := json.Marshal(map[string]string{"error": code})
	return status, body
}

// basicAuth decodes an HTTP Basic Authorization header, whose parts are form
// encoded, as OAuth 2.0 specifies.
func basicAuth(header string) (user, password string, ok bool) {
	encoded, found := strings.CutPrefix(header, "Basic ")
	if !found {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}

	user, password, ok = strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", false
	}

	user, userErr := url.QueryUnescape(user)
	password, passwordErr := url.QueryUnescape(password)
	return user, password, userErr == nil && passwordErr == nil
}

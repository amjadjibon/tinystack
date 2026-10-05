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
	"github.com/amjadjibon/tinystack/internal/storage"
)

// invalidClient is the OAuth error for credentials that do not match a client.
const invalidClient = "invalid_client"

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
	clientID, secret := form.Get("client_id"), form.Get("client_secret")
	if user, password, ok := basicAuth(authorization); ok {
		clientID, secret = user, password
	}

	switch {
	case form.Get("grant_type") == "", clientID == "":
		status, body = oauthError(http.StatusBadRequest, "invalid_request")
		return status, body, nil
	case form.Get("grant_type") != flowClientCredentials:
		status, body = oauthError(http.StatusBadRequest, "unsupported_grant_type")
		return status, body, nil
	}

	var token string
	var expiresIn int64
	var failure string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		index, found, readErr := read[clientIndex](tx, clientIndexKey(clientID))
		if readErr != nil || !found || index.Pool != poolID {
			failure = invalidClient
			return readErr
		}

		scope := request.Scope{AccountID: index.Account, Region: index.Region}
		p, _, readErr := read[pool](tx, poolKey(scope, poolID))
		if readErr != nil {
			return readErr
		}

		c, _, readErr := read[client](tx, clientKey(scope, poolID, clientID))
		switch {
		case readErr != nil:
			return readErr
		case c == nil || p == nil || c.Secret == "" || c.Secret != secret:
			failure = invalidClient
			return nil
		case !value(c.Settings.AllowedOAuthFlowsUserPoolClient) ||
			!slices.Contains(c.Settings.AllowedOAuthFlows, flowClientCredentials):
			failure = "unauthorized_client"
			return nil
		}

		scopes := c.Settings.AllowedOAuthScopes
		if requested := strings.Fields(form.Get("scope")); len(requested) > 0 {
			for _, sc := range requested {
				if !slices.Contains(scopes, sc) {
					failure = "invalid_scope"
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
			if writeErr := write(tx, poolKey(scope, poolID), p); writeErr != nil {
				return writeErr
			}
		}

		now := s.now()
		validity := c.Settings.validity(tokenAccess)
		expiresIn = int64(validity / time.Second)
		token, keyErr = sign(key, map[string]any{
			attrSub: clientID, "client_id": clientID, claimTokenUse: "access", "scope": strings.Join(scopes, " "),
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

	body, err = json.Marshal(map[string]any{"access_token": token, "expires_in": expiresIn, "token_type": "Bearer"})
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

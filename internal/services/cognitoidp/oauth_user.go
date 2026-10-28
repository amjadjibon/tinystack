package cognitoidp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) userToken(
	ctx context.Context, poolID, endpoint, clientID, secret string, form url.Values,
) (resultStatus int, resultBody []byte, resultErr error) {
	ctx = request.WithMetadata(ctx, request.Metadata{Endpoint: endpoint})
	var g *grant
	var failure string
	err := s.byClient(ctx, &clientID, func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
		if p.ID != poolID || !c.acceptsSecret(secret) {
			failure = invalidClient
			return nil
		}

		if !common.Value(c.Settings.AllowedOAuthFlowsUserPoolClient) {
			failure = unauthorizedClient
			return nil
		}

		if !slices.Contains(c.Settings.AllowedOAuthFlows, flowCode) &&
			!slices.Contains(c.Settings.AllowedOAuthFlows, flowImplicit) {
			// Preserve the existing machine-client error when it attempts a user grant.
			failure = "unsupported_grant_type"
			return nil
		}

		if form.Get("grant_type") == grantRefreshToken {
			return nil
		}

		if !slices.Contains(c.Settings.AllowedOAuthFlows, flowCode) {
			failure = unauthorizedClient
			return nil
		}

		codeID := form.Get(flowCode)
		code, ok, err := storage.Read[authorizationCode](tx, hostedKey(scope, "oauthcode", p.ID, codeID))
		if err != nil {
			return err
		}

		if !ok || codeID == "" || !s.now().Before(code.Expires) || code.Authorization.Client != c.ID ||
			!hmac.Equal(
				[]byte(code.Authorization.Redirect),
				[]byte(form.Get("redirect_uri")),
			) {
			failure = invalidGrant
			return nil
		}

		if failure = liveAuthorization(c, &code.Authorization); failure != "" {
			return nil
		}

		verifier := form.Get("code_verifier")
		if code.Authorization.Challenge != "" {
			if !pkceValue.MatchString(verifier) {
				failure = invalidGrant
				return nil
			}

			candidate := verifier
			if code.Authorization.Method == pkceS256 {
				sum := sha256.Sum256([]byte(verifier))
				candidate = b64.EncodeToString(sum[:])
			}

			if !hmac.Equal([]byte(candidate), []byte(code.Authorization.Challenge)) {
				failure = invalidGrant
				return nil
			}
		} else if verifier != "" {
			failure = invalidGrant
			return nil
		}

		u, ok, err := storage.Read[user](tx, userKey(scope, p, code.Username))
		if err != nil {
			return err
		}

		if !ok || u.Disabled || u.Attributes[attrSub] != code.Sub || !slices.Contains(u.Sessions, code.Origin) {
			failure = invalidGrant
			return nil
		}

		key, _, err := signingKey(p)
		if err != nil {
			return err
		}

		g = tokens(ctx, scope, key, p, c, u, &session{
			auth: code.AuthTime, origin: code.Origin, refresh: code.Refresh,
			source: sourceHostedAuth, oauth: true, scopes: code.Authorization.Scopes, nonce: code.Authorization.Nonce,
		}, s.now())
		g.postAuth = nil
		tx.Delete(hostedKey(scope, "oauthcode", p.ID, codeID))
		return nil
	})
	if err != nil {
		failure = invalidClient
	}

	if failure != "" {
		status := http.StatusBadRequest
		if failure == invalidClient {
			status = http.StatusUnauthorized
		}

		status, body := oauthError(status, failure)
		return status, body, nil
	}

	var result *api.AuthenticationResultType
	if form.Get("grant_type") == grantRefreshToken {
		refreshed, refreshErr := s.GetTokensFromRefreshToken(ctx, &api.GetTokensFromRefreshTokenRequest{
			ClientId: &clientID, ClientSecret: &secret, RefreshToken: new(form.Get(grantRefreshToken)),
		})
		if refreshErr != nil {
			status, body := oauthError(http.StatusBadRequest, invalidGrant)
			return status, body, nil //nolint:nilerr // Expected errors are returned in the OAuth response.
		}

		result = refreshed.AuthenticationResult
	} else {
		result, err = s.issue(ctx, g)
		if err != nil {
			status, body := oauthError(http.StatusBadRequest, invalidGrant)
			return status, body, nil //nolint:nilerr // Expected errors are returned in the OAuth response.
		}
	}

	body, err := json.Marshal(oauthResult(result))
	return http.StatusOK, body, err
}

func oauthResult(result *api.AuthenticationResultType) map[string]any {
	body := map[string]any{
		oauthAccessToken: common.Value(result.AccessToken), oauthTokenType: bearerTokenType,
		oauthExpiresIn: common.Value(result.ExpiresIn),
	}

	if result.IdToken != nil {
		body["id_token"] = *result.IdToken
	}

	if result.RefreshToken != nil {
		body[grantRefreshToken] = *result.RefreshToken
	}

	return body
}

// UserInfo accepts only a live, pool-bound access token with openid and filters readable attributes by scopes.
func (s *Service) UserInfo(
	ctx context.Context,
	poolID, authorization string,
) (resultStatus int, resultBody []byte, resultErr error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		status, body := oauthError(http.StatusBadRequest, invalidRequest)
		return status, body, nil
	}

	out := map[string]any{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		a, err := s.checkAccess(tx, &token, "openid")
		if err != nil {
			return err
		}

		if a.pool.ID != poolID {
			return notAuthorized("Wrong user pool.")
		}

		claims, err := claimsOf(token)
		if err != nil {
			return err
		}

		scopes := strings.Fields(claims[claimScope].(string))
		out[attrSub], out[claimUsername] = a.user.Attributes[attrSub], a.user.Username
		for name, value := range a.user.Attributes {
			if name != attrIdentities && readable(a.client, name) && scopedAttribute(name, scopes) {
				out[name] = value
			}
		}

		return nil
	})
	if err != nil {
		status, body := oauthError(http.StatusUnauthorized, "invalid_token")
		return status, body, nil //nolint:nilerr // Expected errors are returned in the OAuth response.
	}

	body, err := json.Marshal(out)
	return http.StatusOK, body, err
}

func (s *Service) Revoke(
	ctx context.Context,
	poolID, authorization string,
	form url.Values,
) (resultStatus int, resultBody []byte, resultErr error) {
	clientID, secret := form.Get(claimClientID), form.Get("client_secret")
	if user, password, ok := basicAuth(authorization); ok {
		clientID, secret = user, password
	}

	err := s.byClient(ctx, &clientID, func(_ storage.WriteTx, _ request.Scope, p *pool, c *client) error {
		if p.ID != poolID || !c.acceptsSecret(secret) {
			return notAuthorized("Invalid client.")
		}

		return nil
	})
	if err != nil {
		status, body := oauthError(http.StatusUnauthorized, invalidClient)
		return status, body, nil //nolint:nilerr // Expected errors are returned in the OAuth response.
	}

	if form.Get(responseToken) == "" {
		status, body := oauthError(http.StatusBadRequest, invalidRequest)
		return status, body, nil
	}

	_, err = s.RevokeToken(
		ctx,
		&api.RevokeTokenRequest{ClientId: &clientID, ClientSecret: &secret, Token: new(form.Get(responseToken))},
	)
	if err != nil {
		status, body := oauthError(http.StatusBadRequest, "unsupported_token_type")
		return status, body, nil //nolint:nilerr // Expected errors are returned in the OAuth response.
	}

	return http.StatusOK, []byte(`{}`), nil
}

func (s *Service) HostedLogout(
	ctx context.Context, poolID string, values url.Values, browser, cookie string,
) (*HostedResult, error) {
	var result *HostedResult
	relogin := values.Get("logout_uri") == ""
	// Validate the re-login request before deleting the session or returning a redirect.
	err := s.byClient(
		ctx,
		new(values.Get(claimClientID)),
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			if p.ID != poolID {
				return invalidParameter("Invalid client.")
			}

			switch {
			case relogin:
				a, failure := parseAuthorization(c, values)
				if a == nil || failure == invalidRedirectURI {
					return invalidParameter("Invalid redirect_uri.")
				}

				if failure != "" {
					result = authorizationFailure(a, failure, "Invalid re-login request.")
				}
			case !slices.Contains(c.Settings.LogoutURLs, values.Get("logout_uri")):
				return invalidParameter("Invalid logout_uri.")
			default:
				result = &HostedResult{Redirect: values.Get("logout_uri")}
			}

			tx.Delete(hostedKey(scope, "browsersession", p.ID, cookie))
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	if relogin && result == nil {
		values.Set("prompt", "login")
		result, err = s.Authorize(ctx, poolID, values, browser, "")
	}

	if result != nil {
		result.ClearSession = true
	}

	return result, err
}

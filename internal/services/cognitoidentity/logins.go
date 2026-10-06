package cognitoidentity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	// developerLogin is the login a developer-authenticated identity
	// presents: the token GetOpenIdTokenForDeveloperIdentity issued.
	developerLogin = "cognito-identity.amazonaws.com"
	tokenIssuer    = "https://" + developerLogin
	// tokenLife is how long GetOpenIdToken's tokens last.
	tokenLife = 10 * time.Minute
)

var b64 = base64.RawURLEncoding

// login is a verified login: the provider, the subject it names, and the
// token's claims.
type login struct {
	claims   map[string]any
	provider string
	subject  string
}

// token issues the pool's OpenID token for an identity, which authenticated
// with methods (amr) such as "authenticated" and its providers.
func (p *pool) token(identityID string, methods []string, life time.Duration, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}

	claims, err := json.Marshal(map[string]any{
		"iss": tokenIssuer, "aud": *p.Pool.IdentityPoolId, "sub": identityID, "amr": methods,
		"iat": now.Unix(), "exp": now.Add(life).Unix(),
	})
	if err != nil {
		return "", err
	}

	unsigned := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	mac := hmac.New(sha256.New, p.Secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + b64.EncodeToString(mac.Sum(nil)), nil
}

// developerSubject checks a token the pool issued and returns the identity
// it names.
func (p *pool) developerSubject(token string, now time.Time) (string, bool) {
	i := strings.LastIndex(token, ".")
	if i < 0 {
		return "", false
	}

	mac := hmac.New(sha256.New, p.Secret)
	mac.Write([]byte(token[:i]))
	signature, err := b64.DecodeString(token[i+1:])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		return "", false
	}

	parts := strings.Split(token[:i], ".")
	payload, err := b64.DecodeString(parts[len(parts)-1])
	var c struct {
		Audience string  `json:"aud"`
		Subject  string  `json:"sub"`
		Expires  float64 `json:"exp"`
	}
	if err != nil || json.Unmarshal(payload, &c) != nil || c.Audience != *p.Pool.IdentityPoolId ||
		!now.Before(time.Unix(int64(c.Expires), 0)) {
		return "", false
	}

	return c.Subject, true
}

// verifyLogins verifies each login's token: a user pool ID token from one of
// the pool's user pool providers, a token of one of its OpenID Connect
// providers, or the pool's own developer token.
func (s *Service) verifyLogins(ctx context.Context, scope request.Scope, p *pool, logins map[string]string) (
	[]login, error,
) {
	var out []login
	for _, provider := range slices.Sorted(maps.Keys(logins)) {
		l, err := s.verifyLogin(ctx, scope, p, provider, logins[provider])
		if err != nil {
			return nil, err
		}

		out = append(out, *l)
	}

	return out, nil
}

func (s *Service) verifyLogin(ctx context.Context, scope request.Scope, p *pool, provider, token string) (
	*login, error,
) {
	invalidToken := notAuthorized("Invalid login token.")
	switch {
	case provider == developerLogin:
		id, ok := p.developerSubject(token, s.now())
		if !ok {
			return nil, invalidToken
		}

		return &login{provider: provider, subject: id}, nil
	case strings.HasPrefix(provider, "cognito-idp."):
		i := slices.IndexFunc(p.Pool.CognitoIdentityProviders, func(c api.CognitoIdentityProvider) bool {
			return common.Value(c.ProviderName) == provider
		})
		if i < 0 || s.pools == nil {
			return nil, notAuthorized("Invalid login token. Issuer doesn't match providerName")
		}

		_, userPool, _ := strings.Cut(provider, "/")
		claims, err := s.pools.Verify(ctx, userPool, token)
		if err != nil || claims["token_use"] != "id" {
			return nil, invalidToken
		}

		if client := common.Value(
			p.Pool.CognitoIdentityProviders[i].ClientId,
		); client != "" &&
			claims["aud"] != client {
			return nil, notAuthorized("Invalid login token. Incorrect token audience.")
		}

		return &login{provider: provider, subject: claimString(claims, "sub"), claims: claims}, nil
	case slices.Contains(socialProviders, provider):
		return nil, invalid("TinyStack does not verify %s tokens: use a user pool, an OpenID Connect provider, "+
			"or developer authentication.", provider)
	}

	arn := "arn:aws:iam::" + scope.AccountID + ":oidc-provider/" + provider
	if !slices.Contains(p.Pool.OpenIdConnectProviderARNs, arn) || s.roles == nil {
		return nil, notAuthorized("Invalid login token. %s is not a provider of the identity pool.", provider)
	}

	idp, found, err := s.roles.OIDCProvider(ctx, scope.AccountID, "https://"+provider)
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notAuthorized("Invalid login token. No OpenID Connect provider %s.", arn))
	}

	claims, err := s.verifier.Verify(ctx, token, "https://"+provider, idp.ClientIDs)
	if err != nil {
		return nil, invalidToken
	}

	return &login{provider: provider, subject: claimString(claims, "sub"), claims: claims}, nil
}

func claimString(claims map[string]any, name string) string {
	v, _ := claims[name].(string)
	return v
}

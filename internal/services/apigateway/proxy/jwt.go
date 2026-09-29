package proxy

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// keysTTL is how long a JWKS is used before it is read again.
const keysTTL = 5 * time.Minute

// Verifier checks RS256 JWTs against their issuer's keys, which it reads from
// the issuer's OpenID discovery document (<issuer>/.well-known/openid-configuration)
// and caches for a few minutes.
type Verifier struct {
	keys map[string]issuerKeys
	now  func() time.Time
	mu   sync.Mutex
}

type issuerKeys struct {
	read time.Time
	keys map[string]*rsa.PublicKey
}

func NewVerifier() *Verifier {
	return &Verifier{keys: map[string]issuerKeys{}, now: time.Now}
}

// ErrUnauthorized means a token is missing, malformed, not signed by its
// issuer, expired, or for another audience.
var ErrUnauthorized = errors.New("unauthorized")

// Verify checks token (with or without a "Bearer " prefix) and returns its
// claims. issuer is the required "iss". When audiences is not empty, the
// token's "aud", or else its "client_id", must be one of them.
func (v *Verifier) Verify(ctx context.Context, token, issuer string, audiences []string) (map[string]any, error) {
	token = strings.TrimSpace(token)
	if len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
		token = strings.TrimSpace(token[7:])
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrUnauthorized
	}

	var header struct{ Alg, Kid string }
	claims := map[string]any{}
	if decodePart(parts[0], &header) != nil || decodePart(parts[1], &claims) != nil || header.Alg != "RS256" {
		return nil, ErrUnauthorized
	}

	if iss, _ := claims["iss"].(string); iss != strings.TrimSuffix(issuer, "/") && iss != issuer {
		return nil, ErrUnauthorized
	}

	now := v.now()
	exp, _ := claims["exp"].(float64)
	if !now.Before(time.Unix(int64(exp), 0)) {
		return nil, ErrUnauthorized
	}

	if nbf, ok := claims["nbf"].(float64); ok && now.Before(time.Unix(int64(nbf), 0)) {
		return nil, ErrUnauthorized
	}

	if len(audiences) > 0 && !audienceMatches(claims, audiences) {
		return nil, ErrUnauthorized
	}

	key, err := v.key(ctx, issuer, header.Kid)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrUnauthorized
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return nil, ErrUnauthorized
	}

	return claims, nil
}

func decodePart(part string, v any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, v)
}

func audienceMatches(claims map[string]any, audiences []string) bool {
	var got []string
	switch aud := claims["aud"].(type) {
	case string:
		got = []string{aud}
	case []any:
		for _, a := range aud {
			if s, ok := a.(string); ok {
				got = append(got, s)
			}
		}
	default:
		if id, ok := claims["client_id"].(string); ok {
			got = []string{id}
		}
	}

	for _, g := range got {
		for _, want := range audiences {
			if g == want {
				return true
			}
		}
	}

	return false
}

// key returns the issuer's key with ID kid, reading the issuer's keys when
// they are not cached, are stale, or lack kid.
func (v *Verifier) key(ctx context.Context, issuer, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	cached, ok := v.keys[issuer]
	v.mu.Unlock()
	if ok && v.now().Sub(cached.read) < keysTTL {
		if key, found := cached.keys[kid]; found {
			return key, nil
		}
	}

	keys, err := readKeys(ctx, issuer)
	if err != nil {
		return nil, err
	}

	v.mu.Lock()
	v.keys[issuer] = issuerKeys{read: v.now(), keys: keys}
	v.mu.Unlock()
	if key, found := keys[kid]; found {
		return key, nil
	}

	return nil, ErrUnauthorized
}

// maxDocument bounds discovery documents and key sets.
const maxDocument = 1 << 20

func getJSON(ctx context.Context, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- the authorizer's issuer names the URL.
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument))
	if err != nil {
		return err
	}

	return json.Unmarshal(data, v)
}

// readKeys reads an issuer's RSA signing keys by key ID.
func readKeys(ctx context.Context, issuer string) (map[string]*rsa.PublicKey, error) {
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	discoveryURL := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	if err := getJSON(ctx, discoveryURL, &discovery); err != nil {
		return nil, fmt.Errorf("reading the issuer's discovery document: %w", err)
	}

	var set struct {
		Keys []struct{ Kty, Kid, N, E string }
	}
	if err := getJSON(ctx, discovery.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("reading the issuer's keys: %w", err)
	}

	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		n, nErr := base64.RawURLEncoding.DecodeString(k.N)
		e, eErr := base64.RawURLEncoding.DecodeString(k.E)
		if k.Kty != "RSA" || nErr != nil || eErr != nil || len(e) == 0 || len(e) > 4 {
			continue
		}

		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}

	return keys, nil
}

// Scopes returns a token's OAuth scopes: its space-separated "scope" claim,
// or its "scp" list.
func Scopes(claims map[string]any) []string {
	if s, ok := claims["scope"].(string); ok {
		return strings.Fields(s)
	}

	var out []string
	if list, ok := claims["scp"].([]any); ok {
		for _, s := range list {
			if str, isString := s.(string); isString {
				out = append(out, str)
			}
		}
	}

	return out
}

// Strings renders claims as API Gateway passes them to integrations: every
// value as a string, with lists as "[a b]".
func Strings(claims map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range claims {
		switch v := v.(type) {
		case string:
			out[k] = v
		case float64:
			out[k] = big.NewFloat(v).Text('f', -1)
		case []any:
			parts := make([]string, 0, len(v))
			for _, p := range v {
				parts = append(parts, fmt.Sprint(p))
			}

			out[k] = "[" + strings.Join(parts, " ") + "]"
		default:
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}

	return out
}

package cognitoidp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	keyBits     = 2048
	maxSessions = 100
	adminScope  = "aws.cognito.signin.user.admin"
	algorithm   = "RS256"
)

var b64 = base64.RawURLEncoding

// refresh is a refresh token's record, under refresh/<pool ID>/<token>.
type refresh struct {
	Expires  time.Time
	AuthTime time.Time
	Username string
	Client   string
	Origin   string
}

func refreshKey(scope request.Scope, pool, token string) string {
	return prefix(scope, "refresh") + pool + "/" + token
}

// signingKey returns the pool's RSA signing key, creating it on first use so
// that pools which never issue tokens never pay for one.
func signingKey(p *pool) (*rsa.PrivateKey, bool, error) {
	if len(p.Key) > 0 {
		key, err := x509.ParsePKCS1PrivateKey(p.Key)
		return key, false, err
	}

	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, false, err
	}

	p.Key = x509.MarshalPKCS1PrivateKey(key)
	return key, true, nil
}

// keyID names a public key in the pool's JWKS.
func keyID(key *rsa.PublicKey) string {
	sum := sha256.Sum256(x509.MarshalPKCS1PublicKey(key))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func sign(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"kid": keyID(&key.PublicKey), "alg": algorithm})
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	unsigned := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}

	return unsigned + "." + b64.EncodeToString(signature), nil
}

// claimsOf decodes a JWT's claims without verifying it.
func claimsOf(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}

	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}

	var claims map[string]any
	err = json.Unmarshal(payload, &claims)
	return claims, err
}

// verify checks a JWT's RS256 signature with key.
func verify(key *rsa.PublicKey, token string) error {
	i := strings.LastIndex(token, ".")
	if i < 0 {
		return errors.New("malformed token")
	}

	sig, err := b64.DecodeString(token[i+1:])
	if err != nil {
		return err
	}

	digest := sha256.Sum256([]byte(token[:i]))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig)
}

// issuer is a pool's token issuer URL: the endpoint the client used, so that
// the pool's JWKS can be read at <issuer>/.well-known/jwks.json.
func issuer(ctx context.Context, pool string) string {
	m, _ := request.FromContext(ctx)
	return strings.TrimSuffix(m.Endpoint, "/") + "/" + pool
}

// session is one sign-in: the tokens it issues share its origin.
type session struct {
	auth   time.Time
	origin string
	// refresh is the refresh token to issue, or "" to issue none.
	refresh string
}

// tokens issues access and ID tokens for u through c, signed with key.
func tokens(
	ctx context.Context, key *rsa.PrivateKey, p *pool, c *client, u *user, sess session, now time.Time,
) (*api.AuthenticationResultType, error) {
	var err error
	iss := issuer(ctx, p.ID)
	common := func(use string, validity time.Duration) map[string]any {
		claims := map[string]any{
			"sub": u.Attributes[attrSub], "iss": iss, "origin_jti": sess.origin, "event_id": newSub(),
			"token_use": use, "auth_time": sess.auth.Unix(), "iat": now.Unix(), "exp": now.Add(validity).Unix(),
			"jti": newSub(),
		}
		if len(u.Groups) > 0 {
			claims["cognito:groups"] = u.Groups
		}

		return claims
	}

	accessValidity := c.Settings.validity(tokenAccess)
	access := common("access", accessValidity)
	access["client_id"], access["scope"], access["username"], access["version"] = c.ID, adminScope, u.Username, 2
	id := common("id", c.Settings.validity(tokenID))
	id["aud"], id["cognito:username"] = c.ID, u.Username
	for name, v := range u.Attributes {
		if name == attrSub || !readable(c, name) {
			continue
		}

		if strings.HasSuffix(name, verifiedSuffix) {
			id[name] = v == valueTrue
		} else {
			id[name] = v
		}
	}

	out := &api.AuthenticationResultType{
		TokenType: new("Bearer"), ExpiresIn: new(int64(accessValidity / time.Second)),
	}
	if out.AccessToken, err = signed(key, access); err != nil {
		return nil, err
	}

	if out.IdToken, err = signed(key, id); err != nil {
		return nil, err
	}

	if sess.refresh != "" {
		out.RefreshToken = new(sess.refresh)
	}

	return out, nil
}

func signed(key *rsa.PrivateKey, claims map[string]any) (*string, error) {
	token, err := sign(key, claims)
	return &token, err
}

// readable reports whether a client may read an attribute.
func readable(c *client, name string) bool {
	return len(c.Settings.ReadAttributes) == 0 || slices.Contains(c.Settings.ReadAttributes, name)
}

// signIn starts a session for u through c and issues its tokens, with a
// refresh token when the client allows refreshing.
func (s *Service) signIn(
	ctx context.Context, tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user,
) (*api.AuthenticationResultType, error) {
	now := s.now().UTC()
	sess := session{auth: now, origin: newSub()}
	u.Sessions = append(u.Sessions, sess.origin)
	if n := len(u.Sessions); n > maxSessions {
		u.Sessions = u.Sessions[n-maxSessions:]
	}

	if slices.Contains(c.Settings.flows(), flowRefresh) {
		sess.refresh = randomString(alphaNum, 256)
		r := refresh{
			Expires: now.Add(c.Settings.validity(tokenRefresh)), AuthTime: now, Username: u.Username, Client: c.ID,
			Origin: sess.origin,
		}
		if err := write(tx, refreshKey(scope, p.ID, sess.refresh), r); err != nil {
			return nil, err
		}
	}

	key, created, err := signingKey(p)
	if err != nil {
		return nil, err
	}

	if created {
		if writeErr := write(tx, poolKey(scope, p.ID), p); writeErr != nil {
			return nil, writeErr
		}
	}

	out, err := tokens(ctx, key, p, c, u, sess, now)
	if err != nil {
		return nil, err
	}

	u.Modified = now
	return out, write(tx, userKey(scope, p, u.Username), u)
}

// signOut ends the user's sessions and deletes their refresh tokens: all of
// them, or only origin's.
func signOut(tx storage.WriteTx, scope request.Scope, p *pool, u *user, origin string) error {
	for _, k := range tx.Keys(prefix(scope, "refresh") + p.ID + "/") {
		r, _, err := read[refresh](tx, k)
		if err != nil {
			return err
		}

		if r.Username == u.Username && (origin == "" || r.Origin == origin) {
			tx.Delete(k)
		}
	}

	u.Sessions = slices.DeleteFunc(u.Sessions, func(o string) bool { return origin == "" || o == origin })
	return nil
}

func notAuthorized(message string) error { return awserror.New("NotAuthorizedException", message) }

// accessToken identifies the signed-in user an access token stands for.
type accessToken struct {
	scope  request.Scope
	pool   *pool
	client *client
	user   *user
	origin string
}

// checkAccess verifies an access token and loads what it stands for.
func (s *Service) checkAccess(tx storage.ReadTx, token *string) (*accessToken, error) {
	invalid := notAuthorized("Invalid Access Token")
	claims, err := claimsOf(value(token))
	if err != nil || claims["token_use"] != "access" {
		return nil, invalid
	}

	clientID, _ := claims["client_id"].(string)
	username, _ := claims["username"].(string)
	origin, _ := claims["origin_jti"].(string)
	iss, _ := claims["iss"].(string)
	exp, _ := claims["exp"].(float64)
	index, ok, err := read[clientIndex](tx, clientIndexKey(clientID))
	if err != nil {
		return nil, err
	}

	if !ok || !strings.HasSuffix(iss, "/"+index.Pool) {
		return nil, invalid
	}

	scope := request.Scope{AccountID: index.Account, Region: index.Region}
	p, ok, err := read[pool](tx, poolKey(scope, index.Pool))
	if err != nil || !ok || len(p.Key) == 0 {
		return nil, errors.Join(err, invalid)
	}

	key, _, err := signingKey(p)
	if err != nil {
		return nil, err
	}

	if verify(&key.PublicKey, *token) != nil {
		return nil, invalid
	}

	if !s.now().Before(time.Unix(int64(exp), 0)) {
		return nil, notAuthorized("Access Token has expired")
	}

	c, ok, err := read[client](tx, clientKey(scope, p.ID, clientID))
	if err != nil || !ok {
		return nil, errors.Join(err, invalid)
	}

	u, ok, err := read[user](tx, userKey(scope, p, username))
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, userNotFound()
	case u.Disabled:
		return nil, notAuthorized("User is disabled.")
	case !slices.Contains(u.Sessions, origin):
		return nil, notAuthorized("Access Token has been revoked")
	}

	return &accessToken{scope: scope, pool: p, client: c, user: u, origin: origin}, nil
}

// Documents returns a pool's OpenID discovery document ("openid-configuration")
// or its JSON Web Key Set ("jwks.json"), which clients read to verify its
// tokens, and whether the pool has it. endpoint is the base URL the client
// used.
func (s *Service) Documents(ctx context.Context, id, name, endpoint string) (doc []byte, found bool, err error) {
	region, _, _ := strings.Cut(id, "_")
	if !poolID.MatchString(id) || (name != "jwks.json" && name != "openid-configuration") {
		return nil, false, nil
	}

	var key *rsa.PublicKey
	load := func(tx storage.ReadTx) (request.Scope, *pool, error) {
		account, ok := tx.Get(poolIndexKey(id))
		if !ok {
			return request.Scope{}, nil, nil
		}

		scope := request.Scope{AccountID: string(account), Region: region}
		p, _, readErr := read[pool](tx, poolKey(scope, id))
		return scope, p, readErr
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, p, loadErr := load(tx)
		if loadErr != nil || p == nil || len(p.Key) == 0 {
			return loadErr
		}

		private, _, keyErr := signingKey(p)
		if keyErr != nil {
			return keyErr
		}

		key = &private.PublicKey
		return nil
	})
	if err == nil && key == nil {
		// A pool that never issued a token gets its key now.
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			scope, p, loadErr := load(tx)
			if loadErr != nil || p == nil {
				return loadErr
			}

			private, created, keyErr := signingKey(p)
			if keyErr != nil {
				return keyErr
			}

			key = &private.PublicKey
			if created {
				return write(tx, poolKey(scope, id), p)
			}

			return nil
		})
	}

	if err != nil || key == nil {
		return nil, false, err
	}

	if name == "jwks.json" {
		doc, err = jwks(key)
		return doc, true, err
	}

	iss := strings.TrimSuffix(endpoint, "/") + "/" + id
	doc, err = json.Marshal(map[string]any{
		"issuer": iss, "jwks_uri": iss + "/.well-known/jwks.json", "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported": []string{algorithm}, "response_types_supported": []string{"code"},
	})
	return doc, true, err
}

// jwks renders a public key as a JSON Web Key Set.
func jwks(key *rsa.PublicKey) ([]byte, error) {
	return json.Marshal(map[string]any{"keys": []map[string]string{{
		"alg": algorithm, "e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes()), "kid": keyID(key),
		"kty": "RSA", "n": b64.EncodeToString(key.N.Bytes()), "use": "sig",
	}}})
}

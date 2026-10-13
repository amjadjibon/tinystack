package integration_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var jwtEncoding = base64.RawURLEncoding

// signJWT signs claims with an RS256 header, or leaves the signature empty
// when the header names "none".
func signJWT(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}

	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}

	unsigned := jwtEncoding.EncodeToString(h) + "." + jwtEncoding.EncodeToString(c)
	if header["alg"] == "none" {
		return unsigned + "."
	}

	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return unsigned + "." + jwtEncoding.EncodeToString(signature)
}

func jwksJSON(key *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
		"n": jwtEncoding.EncodeToString(key.N.Bytes()),
		"e": jwtEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
}

// fakeIDP is an in-process OpenID Connect provider: discovery, keys and a token
// endpoint. Tests play the browser's part at the authorization endpoint.
type fakeIDP struct {
	tb         *testing.T
	key, other *rsa.PrivateKey
	server     *httptest.Server
	grants     map[string]*idpGrant
	devices    map[string]*idpGrant
	// deviceUser is the person who approves device sign-ins.
	deviceUser map[string]any
	// Knobs a test sets to make the provider misbehave.
	signWith *rsa.PrivateKey
	header   map[string]any
	audience any
	issuer   string
	nonce    string
	// lastVerifier is the PKCE verifier of the latest token request.
	lastVerifier string
	expiresIn    time.Duration
	// iat shifts the issue time of ID tokens from now.
	iat time.Duration
	mu  sync.Mutex
}

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// device starts a device authorization: whoever is in deviceUser approves it at once.
func (p *fakeIDP) device(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// Like Authelia, the provider wants a confidential client to authenticate here too.
	if id, secret, basic := r.BasicAuth(); !basic || id != idpClientID || secret != idpSecret ||
		r.PostForm.Get("code_challenge_method") != "S256" ||
		r.PostForm.Get("code_challenge") == "" {
		http.Error(w, "invalid_client", http.StatusUnauthorized)
		return
	}

	p.mu.Lock()
	p.devices["device-code"] = &idpGrant{claims: p.deviceUser, challenge: r.PostForm.Get("code_challenge")}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code": "device-code", "user_code": "WDJB-MJHT", "verification_uri": p.server.URL + "/activate",
		"verification_uri_complete": p.server.URL + "/activate?user_code=WDJB-MJHT", "expires_in": 300, "interval": 1,
	})
}

type idpGrant struct {
	claims             map[string]any
	nonce, challenge   string
	redirect, clientID string
}

const (
	idpClientID = "tinystack-console"
	idpSecret   = "idp-test-secret"
)

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	generate := func() *rsa.PrivateKey {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}

		return key
	}

	idp := &fakeIDP{
		tb:        t,
		key:       generate(),
		other:     generate(),
		grants:    map[string]*idpGrant{},
		devices:   map[string]*idpGrant{},
		expiresIn: time.Hour,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint": idp.server.URL + "/token", "jwks_uri": idp.server.URL + "/jwks",
			"end_session_endpoint":                  idp.server.URL + "/logout",
			"device_authorization_endpoint":         idp.server.URL + "/device",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"subject_types_supported":               []string{"public"},
			"response_types_supported":              []string{"code"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksJSON(&idp.key.PublicKey, "k1"))
	})
	mux.HandleFunc("/token", idp.token)
	mux.HandleFunc("/device", idp.device)
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

// authorize answers an authorization request the way the provider does once the
// person signed in as claims: it checks the request and returns the redirect
// back to the application.
func (p *fakeIDP) authorize(t *testing.T, location string, claims map[string]any) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, p.server.URL+"/authorize?") {
		t.Fatalf("not an authorization request: %q", location)
	}

	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != idpClientID ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("nonce") == "" ||
		!strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		t.Fatalf("authorization request is missing a required parameter: %v", q)
	}

	code := q.Get("state") + "-code"
	p.mu.Lock()
	p.grants[code] = &idpGrant{
		claims: claims, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"),
		clientID: q.Get("client_id"),
	}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}

	values := back.Query()
	values.Set("code", code)
	values.Set("state", q.Get("state"))
	back.RawQuery = values.Encode()
	return back.String()
}

func (p *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}

	if err := r.ParseForm(); err != nil {
		fail("invalid_request")
		return
	}

	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}

	p.mu.Lock()
	grant := p.grants[r.PostForm.Get("code")]
	delete(p.grants, r.PostForm.Get("code"))
	verifier := r.PostForm.Get("code_verifier")
	p.lastVerifier = verifier
	p.mu.Unlock()
	digest := sha256.Sum256([]byte(verifier))
	switch {
	case id != idpClientID || secret != idpSecret:
		fail("invalid_client")
		return
	case r.PostForm.Get("grant_type") == deviceGrant:
		p.mu.Lock()
		pending := p.devices[r.PostForm.Get("device_code")]
		p.mu.Unlock()
		if pending == nil || jwtEncoding.EncodeToString(digest[:]) != pending.challenge {
			fail("invalid_grant")
			return
		}

		p.respondWithIDToken(w, pending.claims, "")
		return
	case grant == nil || r.PostForm.Get("grant_type") != "authorization_code" ||
		r.PostForm.Get("redirect_uri") != grant.redirect || verifier == "" ||
		jwtEncoding.EncodeToString(digest[:]) != grant.challenge:
		fail("invalid_grant")
		return
	}

	p.respondWithIDToken(w, grant.claims, grant.nonce)
}

// respondWithIDToken answers a token request with an ID token for claims.
func (p *fakeIDP) respondWithIDToken(w http.ResponseWriter, personClaims map[string]any, nonce string) {
	claims := map[string]any{
		"iss": p.server.URL, "aud": idpClientID, "nonce": nonce, "iat": time.Now().Unix(),
		"exp": time.Now().Add(p.expiresIn).Unix(),
	}
	for name, value := range personClaims {
		claims[name] = value
	}

	if p.iat != 0 {
		claims["iat"] = time.Now().Add(p.iat).Unix()
	}

	if p.issuer != "" {
		claims["iss"] = p.issuer
	}

	if p.audience != nil {
		claims["aud"] = p.audience
	}

	if p.nonce != "" {
		claims["nonce"] = p.nonce
	}

	key, header := p.key, map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"}
	if p.signWith != nil {
		key = p.signWith
	}

	if p.header != nil {
		header = p.header
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access-token-not-used", "token_type": "Bearer", "expires_in": 3600,
		"id_token": signJWT(p.tb, key, header, claims),
	})
}
